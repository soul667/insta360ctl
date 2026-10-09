// Package multi provides simultaneous control of several Insta360 cameras
// from a single process over one BLE adapter.
//
// The package exists because the one-shot CLI flow (scan, connect, command,
// disconnect) introduces a different connection-setup latency for every
// camera and every invocation. Multi keeps all BLE connections open and
// dispatches commands from a shared barrier, so the only remaining jitter is
// the BLE write itself, which is measured and reported per camera.
package multi

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/facebookincubator/go-belt/tool/logger"
	"github.com/xaionaro-go/insta360ctl/pkg/ble"
	"github.com/xaionaro-go/insta360ctl/pkg/direct"
)

// Action is a per-camera operation. Any text the operation wants to report
// is written to out; the manager prints it after the broadcast so lines from
// different cameras never interleave.
type Action func(ctx context.Context, dev *direct.Device, out io.Writer) error

// Result is the outcome of running an action on one camera during a broadcast.
type Result struct {
	Device       *direct.Device
	Output       []byte
	DispatchedAt time.Time
	FinishedAt   time.Time
	Err          error
}

// RTT returns how long the device call took, from dispatch to return.
// For commands that wait for an acknowledgment this includes the round trip.
func (r Result) RTT() time.Duration {
	return r.FinishedAt.Sub(r.DispatchedAt)
}

// Summary aggregates a broadcast result set.
type Summary struct {
	Total          int
	Failed         int
	DispatchSpread time.Duration
	MaxRTT         time.Duration
}

// Summarize computes the aggregate statistics for a broadcast.
func Summarize(results []Result) Summary {
	s := Summary{Total: len(results)}
	var minD, maxD time.Time
	for _, r := range results {
		if r.Err != nil {
			s.Failed++
		}
		if r.DispatchedAt.IsZero() {
			continue
		}
		if minD.IsZero() || r.DispatchedAt.Before(minD) {
			minD = r.DispatchedAt
		}
		if maxD.IsZero() || r.DispatchedAt.After(maxD) {
			maxD = r.DispatchedAt
		}
		if rtt := r.RTT(); rtt > s.MaxRTT {
			s.MaxRTT = rtt
		}
	}
	if !minD.IsZero() {
		s.DispatchSpread = maxD.Sub(minD)
	}
	return s
}

// FanoutResult is the scheduling outcome for one worker of FanOut.
type FanoutResult struct {
	Index        int
	DispatchedAt time.Time
	FinishedAt   time.Time
	Err          error
}

// FanOut runs fn once per index in [0, n), as close to simultaneously as the
// Go runtime allows. All workers park on a barrier before the gate opens; when
// fireAt is in the future they additionally wait for that wall-clock moment
// (coarse sleep plus a short busy-wait for a sharp edge).
//
// FanOut is the low-level primitive behind Manager.BroadcastAt; it is
// exported (and unit-tested separately) so the timing behavior can be
// verified without any BLE hardware.
func FanOut(ctx context.Context, n int, fireAt time.Time, fn func(ctx context.Context, i int) error) []FanoutResult {
	results := make([]FanoutResult, n)
	if n <= 0 {
		return results
	}

	var wg sync.WaitGroup
	ready := make(chan struct{}, n)
	gate := make(chan struct{})

	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i].Index = i
			ready <- struct{}{}
			<-gate

			if err := ctx.Err(); err != nil {
				now := time.Now()
				results[i].DispatchedAt = now
				results[i].FinishedAt = now
				results[i].Err = err
				return
			}

			if d := time.Until(fireAt); d > 2*time.Millisecond {
				time.Sleep(d - 2*time.Millisecond)
			}
			for time.Now().Before(fireAt) {
				// Busy-wait the last ~2ms so the dispatch edge stays sharp.
			}

			results[i].DispatchedAt = time.Now()
			err := fn(ctx, i)
			results[i].FinishedAt = time.Now()
			results[i].Err = err
		}()
	}

	for i := 0; i < n; i++ {
		<-ready // every worker is parked on the gate
	}
	close(gate)
	wg.Wait()
	return results
}

// ConnectOptions controls camera discovery and connection.
type ConnectOptions struct {
	// Addrs is the list of wanted BLE addresses. Each entry may itself be a
	// comma-separated list; matching is case-insensitive substring matching,
	// so a unique suffix of the MAC address is enough.
	Addrs []string

	// All connects to every Insta360 camera discovered during AllWindow.
	All bool

	// AllWindow is how long to scan when All is set (default 5s).
	AllWindow time.Duration

	// ScanTimeout is the overall deadline for discovering Addrs (default 30s).
	ScanTimeout time.Duration
}

// Manager holds live BLE connections to multiple cameras.
type Manager struct {
	Devices []*direct.Device
}

// Connect discovers and initializes the requested cameras in parallel.
//
// When some cameras fail to initialize but at least one succeeds, Connect
// returns a non-nil manager (with the cameras that did connect) together
// with a non-nil error describing the failures, so callers can decide
// between continuing with a partial set or aborting.
func Connect(ctx context.Context, adapter ble.Adapter, opts ConnectOptions) (*Manager, error) {
	wanted := make([]string, 0, len(opts.Addrs))
	seenWant := map[string]bool{}
	for _, a := range opts.Addrs {
		for _, part := range strings.Split(a, ",") {
			part = strings.ToLower(strings.TrimSpace(part))
			if part == "" || seenWant[part] {
				continue
			}
			seenWant[part] = true
			wanted = append(wanted, part)
		}
	}
	if len(wanted) == 0 && !opts.All {
		return nil, fmt.Errorf("no camera addresses given (use --addr, or --all)")
	}
	if opts.AllWindow <= 0 {
		opts.AllWindow = 5 * time.Second
	}
	if opts.ScanTimeout <= 0 {
		opts.ScanTimeout = 30 * time.Second
	}

	scanCtx, cancelScan := context.WithCancel(ctx)

	devCh, errCh := direct.Scan(scanCtx, adapter)

	matched := make(map[string]*direct.Device, len(wanted))
	var allDevs []*direct.Device
	assigned := map[string]bool{}

	var allC <-chan time.Time
	if opts.All {
		t := time.NewTimer(opts.AllWindow)
		defer t.Stop()
		allC = t.C
	}
	timeoutC := time.NewTimer(opts.ScanTimeout)
	defer timeoutC.Stop()

scanLoop:
	for {
		if !opts.All && len(matched) == len(wanted) {
			break
		}
		select {
		case dev, ok := <-devCh:
			if !ok {
				devCh = nil // scan ended; nothing more will arrive
				continue
			}
			if dev == nil {
				continue
			}
			if opts.All {
				allDevs = append(allDevs, dev)
				assigned[dev.Address] = true
				continue
			}
			for _, w := range wanted {
				if _, ok := matched[w]; ok {
					continue
				}
				if !strings.Contains(strings.ToLower(dev.Address), w) {
					continue
				}
				if assigned[dev.Address] {
					continue
				}
				matched[w] = dev
				assigned[dev.Address] = true
				logger.Infof(ctx, "matched %q to %s", w, dev)
				break
			}
		case err := <-errCh:
			if err != nil {
				cancelScan()
				return nil, fmt.Errorf("scan failed: %w", err)
			}
			errCh = nil
		case <-allC:
			break scanLoop
		case <-timeoutC.C:
			break scanLoop
		case <-ctx.Done():
			cancelScan()
			return nil, ctx.Err()
		}
	}

	// Stop scanning before opening GATT connections: concurrent discovery
	// slows connection setup down and is not needed anymore.
	cancelScan()
	_ = adapter.StopScan()

	var devs []*direct.Device
	var missing []string
	if opts.All {
		devs = allDevs
	} else {
		for _, w := range wanted {
			if d, ok := matched[w]; ok {
				devs = append(devs, d)
			} else {
				missing = append(missing, w)
			}
		}
	}
	if len(devs) == 0 {
		if len(missing) > 0 {
			return nil, fmt.Errorf("no cameras found (missing: %s)", strings.Join(missing, ", "))
		}
		return nil, fmt.Errorf("no cameras found")
	}

	var wg sync.WaitGroup
	initErrs := make([]error, len(devs))
	for i := range devs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := devs[i].Init(ctx); err != nil {
				initErrs[i] = fmt.Errorf("%s (%s): %w", devs[i].Name(), devs[i].Address, err)
			}
		}(i)
	}
	wg.Wait()

	var ready []*direct.Device
	var failures []string
	for i, d := range devs {
		if initErrs[i] != nil {
			failures = append(failures, initErrs[i].Error())
			_ = d.Close(ctx)
			continue
		}
		ready = append(ready, d)
	}
	for _, w := range missing {
		failures = append(failures, fmt.Sprintf("%s: not found during scan", w))
	}

	m := &Manager{Devices: ready}
	if len(ready) == 0 {
		return nil, fmt.Errorf("failed to initialize any camera: %s", strings.Join(failures, "; "))
	}
	if len(failures) > 0 {
		return m, fmt.Errorf("some cameras failed: %s", strings.Join(failures, "; "))
	}
	return m, nil
}

// Broadcast runs action on every camera as simultaneously as possible.
func (m *Manager) Broadcast(ctx context.Context, action Action) []Result {
	return m.BroadcastAt(ctx, time.Time{}, action)
}

// BroadcastAt is Broadcast with a scheduled fire time. A zero fireAt means
// "fire now". All cameras are dispatched after a shared barrier, so the
// reported dispatch spread is dominated by the BLE stack, not by connection
// setup.
func (m *Manager) BroadcastAt(ctx context.Context, fireAt time.Time, action Action) []Result {
	n := len(m.Devices)
	outs := make([][]byte, n)
	rs := FanOut(ctx, n, fireAt, func(ctx context.Context, i int) error {
		var buf bytes.Buffer
		err := action(ctx, m.Devices[i], &buf)
		outs[i] = buf.Bytes()
		return err
	})

	results := make([]Result, n)
	for i := range rs {
		results[i] = Result{
			Device:       m.Devices[i],
			Output:       outs[i],
			DispatchedAt: rs[i].DispatchedAt,
			FinishedAt:   rs[i].FinishedAt,
			Err:          rs[i].Err,
		}
	}
	return results
}

// Close disconnects from every camera; errors are logged, not returned.
func (m *Manager) Close(ctx context.Context) {
	for _, d := range m.Devices {
		if err := d.Close(ctx); err != nil {
			logger.Warnf(ctx, "failed to disconnect %s (%s): %v", d.Name(), d.Address, err)
		}
	}
}
