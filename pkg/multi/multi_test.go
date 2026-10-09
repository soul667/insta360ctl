package multi

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFanOutFiresAllAtDeadline(t *testing.T) {
	const n = 6
	fireAt := time.Now().Add(40 * time.Millisecond)

	var calls int64
	results := FanOut(context.Background(), n, fireAt, func(ctx context.Context, i int) error {
		atomic.AddInt64(&calls, 1)
		if i == 2 {
			return fmt.Errorf("camera %d failed", i)
		}
		return nil
	})

	require.EqualValues(t, n, atomic.LoadInt64(&calls))
	require.Len(t, results, n)
	require.Error(t, results[2].Err)
	require.NoError(t, results[0].Err)

	var minD, maxD time.Time
	for _, r := range results {
		require.False(t, r.DispatchedAt.IsZero(), "result %d was never dispatched", r.Index)
		require.False(t, r.DispatchedAt.Before(fireAt),
			"result %d dispatched at %v, before fireAt %v", r.Index, r.DispatchedAt, fireAt)
		require.False(t, r.FinishedAt.Before(r.DispatchedAt))
		if minD.IsZero() || r.DispatchedAt.Before(minD) {
			minD = r.DispatchedAt
		}
		if maxD.IsZero() || r.DispatchedAt.After(maxD) {
			maxD = r.DispatchedAt
		}
	}
	spread := maxD.Sub(minD)
	require.Less(t, spread, 15*time.Millisecond, "dispatch spread too large: %v", spread)
}

func TestFanOutZeroFireAt(t *testing.T) {
	results := FanOut(context.Background(), 3, time.Time{}, func(ctx context.Context, i int) error {
		return nil
	})
	require.Len(t, results, 3)
	for _, r := range results {
		require.NoError(t, r.Err)
		require.False(t, r.DispatchedAt.IsZero())
	}
}

func TestFanOutHonorsCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	results := FanOut(ctx, 2, time.Time{}, func(ctx context.Context, i int) error {
		t.Fatal("action must not run on a cancelled context")
		return nil
	})
	require.Len(t, results, 2)
	for _, r := range results {
		require.ErrorIs(t, r.Err, context.Canceled)
	}
}

func TestSummarize(t *testing.T) {
	now := time.Now()
	results := []Result{
		{DispatchedAt: now, FinishedAt: now.Add(5 * time.Millisecond)},
		{DispatchedAt: now.Add(2 * time.Millisecond), FinishedAt: now.Add(20 * time.Millisecond)},
		{DispatchedAt: now, FinishedAt: now, Err: fmt.Errorf("boom")},
	}
	s := Summarize(results)
	require.Equal(t, 3, s.Total)
	require.Equal(t, 1, s.Failed)
	require.Equal(t, 2*time.Millisecond, s.DispatchSpread)
	require.Equal(t, 18*time.Millisecond, s.MaxRTT)
}
