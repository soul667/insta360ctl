// Command x5probe is an experimental tool to reverse-engineer the BLE
// protocol of newer Insta360 cameras (X5) that appear to use the
// network/WiFi packet framing (4-byte length prefix + packet type byte)
// over BLE instead of the bare 16-byte Header16 framing.
//
// It connects to a camera, logs every BLE notification with a decoded
// description, and can optionally send a sync handshake and
// start/stop-capture commands using the network framing.
//
// Usage:
//
//	x5probe --addr AA:BB:CC:DD:EE:FF --sync --start --stop-after 6s --listen 16s
package main

import (
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/xaionaro-go/insta360ctl/pkg/ble"
	"github.com/xaionaro-go/insta360ctl/pkg/direct"
)

const (
	be80Service = 0xBE80
	be81Char    = 0xBE81 // app -> camera
	be82Char    = 0xBE82 // camera -> app
)

func main() {
	addr := flag.String("addr", "", "camera BLE address (required)")
	syncFirst := flag.Bool("sync", false, "send the network sync handshake before commands")
	start := flag.Bool("start", false, "send START_CAPTURE (network framing)")
	stopAfter := flag.Duration("stop-after", 0, "after this long, send STOP_CAPTURE")
	listenFor := flag.Duration("listen", 15*time.Second, "total listen duration")
	seq := flag.Uint("seq", 1, "first sequence number")
	flag.Parse()

	if *addr == "" {
		fmt.Fprintln(os.Stderr, "usage: x5probe --addr AA:BB:CC:DD:EE:FF [--sync] [--start] [--stop-after 6s] [--listen 16s]")
		os.Exit(2)
	}

	if err := run(*addr, *syncFirst, *start, *stopAfter, *listenFor, uint32(*seq)); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(addr string, syncFirst, start bool, stopAfter, listenFor time.Duration, seq uint32) error {
	ctx := context.Background()
	startTime := time.Now()
	logf := func(format string, args ...any) {
		fmt.Printf("[%7.3fs] %s\n", time.Since(startTime).Seconds(), fmt.Sprintf(format, args...))
	}

	adapter, err := ble.NewDBusAdapter(ctx, "")
	if err != nil {
		return fmt.Errorf("ble adapter: %w", err)
	}
	defer adapter.Close(ctx)

	// Discover the camera first so the adapter has it in its scan cache.
	scanCtx, cancelScan := context.WithCancel(ctx)
	defer cancelScan()
	devCh, errCh := direct.Scan(scanCtx, adapter)

	var found *direct.Device
	timeout := time.After(20 * time.Second)
scan:
	for found == nil {
		select {
		case dev := <-devCh:
			if dev == nil {
				break scan
			}
			if strings.Contains(strings.ToLower(dev.Address), strings.ToLower(addr)) {
				found = dev
				break scan
			}
		case err := <-errCh:
			if err != nil {
				return fmt.Errorf("scan: %w", err)
			}
		case <-timeout:
			return fmt.Errorf("camera %s not found", addr)
		}
	}
	cancelScan()
	_ = adapter.StopScan()
	logf("found %s", found)

	periph, err := adapter.Connect(ctx, found.Address)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer periph.Disconnect(ctx)
	logf("connected")

	services, err := periph.DiscoverServices(ctx)
	if err != nil {
		return fmt.Errorf("discover services: %w", err)
	}

	var writeChar, notifyChar ble.Characteristic
	for _, svc := range services {
		if !ble.UUIDMatchesShort(svc.UUID(), be80Service) {
			continue
		}
		chars, err := svc.DiscoverCharacteristics(ctx)
		if err != nil {
			return fmt.Errorf("discover characteristics: %w", err)
		}
		for _, c := range chars {
			switch {
			case ble.UUIDMatchesShort(c.UUID(), be81Char):
				writeChar = c
			case ble.UUIDMatchesShort(c.UUID(), be82Char):
				notifyChar = c
			}
		}
	}
	if writeChar == nil || notifyChar == nil {
		return fmt.Errorf("BE81/BE82 not found (write=%v notify=%v)", writeChar != nil, notifyChar != nil)
	}
	logf("BE81/BE82 located")

	if err := notifyChar.EnableNotifications(func(b []byte) {
		logf("<<< %s | %X", describeNetFrame(b), b)
	}); err != nil {
		return fmt.Errorf("subscribe BE82: %w", err)
	}
	logf("subscribed to BE82")

	write := func(payload []byte, label string) error {
		frame := netFrame(payload)
		logf(">>> %s | %X", label, frame)
		return writeChar.Write(frame, false)
	}

	if syncFirst {
		syncPayload := append([]byte{0x06, 0x00, 0x00}, []byte("syNceNdinS")...)
		if err := write(syncPayload, "SYNC"); err != nil {
			return fmt.Errorf("write sync: %w", err)
		}
		time.Sleep(1500 * time.Millisecond)
	}

	if start {
		if err := write(msgPayload(0x04, seq, nil), "START_CAPTURE"); err != nil { // 0x04 = START_CAPTURE
			return fmt.Errorf("write start: %w", err)
		}
		seq++
	}

	deadline := time.Now().Add(listenFor)
	stopSent := false
	if stopAfter > 0 {
		stopAt := time.Now().Add(stopAfter)
		for time.Now().Before(deadline) {
			if !stopSent && time.Now().After(stopAt) {
				seq++
				if err := write(msgPayload(0x05, seq, nil), "STOP_CAPTURE"); err != nil { // 0x05 = STOP_CAPTURE
					return fmt.Errorf("write stop: %w", err)
				}
				stopSent = true
			}
			time.Sleep(100 * time.Millisecond)
		}
	} else {
		time.Sleep(time.Until(deadline))
	}

	logf("done")
	return nil
}

// netFrame prepends the 4-byte little-endian total-length prefix
// (the length includes the prefix itself), matching the WiFi protocol
// framing observed in X5 BLE notifications.
func netFrame(payload []byte) []byte {
	frame := make([]byte, 4+len(payload))
	binary.LittleEndian.PutUint32(frame[0:4], uint32(len(frame)))
	copy(frame[4:], payload)
	return frame
}

// msgPayload builds a 12-byte network MESSAGE header + body.
func msgPayload(code uint16, seq uint32, body []byte) []byte {
	pkt := make([]byte, 12+len(body))
	pkt[0] = 0x04 // MESSAGE
	pkt[1] = 0x00
	pkt[2] = 0x00
	binary.LittleEndian.PutUint16(pkt[3:5], code)
	pkt[5] = 0x02 // content type: protobuf
	pkt[6] = byte(seq)
	pkt[7] = byte(seq >> 8)
	pkt[8] = byte(seq >> 16)
	pkt[9] = 0x80
	pkt[10] = 0x00
	pkt[11] = 0x00
	copy(pkt[12:], body)
	return pkt
}

// describeNetFrame decodes a received BLE notification as a network frame.
func describeNetFrame(b []byte) string {
	if len(b) < 4 {
		return fmt.Sprintf("short(%d)", len(b))
	}
	total := binary.LittleEndian.Uint32(b[0:4])
	if int(total) != len(b) {
		return fmt.Sprintf("len-prefix=%d actual=%d", total, len(b))
	}
	p := b[4:]
	if len(p) < 3 {
		return fmt.Sprintf("tiny(%d)", len(p))
	}
	switch p[0] {
	case 0x05:
		return "KEEPALIVE"
	case 0x06:
		return fmt.Sprintf("SYNC %q", string(p[3:]))
	case 0x04:
		if len(p) < 12 {
			return "MESSAGE(short)"
		}
		code := binary.LittleEndian.Uint16(p[3:5])
		seq := uint32(p[6]) | uint32(p[7])<<8 | uint32(p[8])<<16
		body := p[12:]
		return fmt.Sprintf("MESSAGE code=0x%04X(%d) seq=%d body=%X", code, code, seq, body)
	default:
		return fmt.Sprintf("type=0x%02X", p[0])
	}
}
