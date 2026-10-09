package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/urfave/cli/v2"
	"github.com/xaionaro-go/insta360ctl/pkg/camera"
	"github.com/xaionaro-go/insta360ctl/pkg/direct"
	"github.com/xaionaro-go/insta360ctl/pkg/multi"
	"github.com/xaionaro-go/insta360ctl/pkg/protocol/messagecode"
)

const shellHelpText = `Commands:
  start [delay]           start recording on all cameras (optional delay, e.g. 5s)
  stop [delay]            stop recording on all cameras
  photo                   take a photo on all cameras
  marker                  set a highlight marker in the current recording
  mode <m>                set capture mode (photo|video|timelapse|hdr|bullettime)
  hdr <on|off>            toggle HDR
  gps <lat> <lon> <alt>   inject GPS coordinates
  battery | storage | status | info | state   query each camera
  raw <cmd-hex> [param-hex ...]   send a raw command to each camera
  devices                 list connected cameras
  help                    show this help
  quit                    disconnect and exit`

func cmdMulti() *cli.Command {
	return &cli.Command{
		Name:  "multi",
		Usage: "Control multiple Insta360 cameras simultaneously over one BLE adapter",
		Description: `Connects to several cameras in one process, keeps all BLE links open, and
fires every command from a shared barrier so the cameras are triggered as
simultaneously as the BLE stack allows. Every broadcast prints per-camera
dispatch offsets and round-trip times, plus the dispatch spread, so the
synchronization quality is measured instead of assumed.

NOTE: this synchronizes command dispatch, not sensor exposure. Each camera
needs its own internal time to start the pipeline; verify the real offset
with a clap/LED marker and correct it in post (see doc/multi_camera.md).`,
		Subcommands: []*cli.Command{
			{
				Name:      "run",
				Usage:     "Connect to all given cameras, fire one command on all of them, then disconnect",
				ArgsUsage: "<record start|record stop|start|stop|photo|marker|mode <m>|hdr <on|off>|gps <lat> <lon> <alt>|battery|storage|status|info|state|power-off>",
				Flags: append(commonMultiFlags(),
					&cli.DurationFlag{
						Name:  "delay",
						Value: 0,
						Usage: "Fire the command this long after connecting (e.g. 5s), so you can get ready",
					},
					&cli.BoolFlag{
						Name:  "no-wait",
						Usage: "Do not wait for the camera acknowledgment (write without response)",
					},
				),
				Action: multiRun,
			},
			{
				Name:  "shell",
				Usage: "Keep connections to all cameras open and fire commands interactively",
				Flags: commonMultiFlags(),
				Action: multiShell,
			},
		},
	}
}

func commonMultiFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringSliceFlag{
			Name:  "addr",
			Usage: "Camera BLE address (repeat for multiple cameras; comma-separated and case-insensitive substrings are accepted)",
		},
		&cli.BoolFlag{
			Name:  "all",
			Usage: "Attach to every Insta360 camera discovered during the scan window",
		},
		&cli.DurationFlag{
			Name:  "scan-window",
			Value: 5 * time.Second,
			Usage: "How long to scan when --all is used",
		},
		&cli.DurationFlag{
			Name:  "connect-timeout",
			Value: 30 * time.Second,
			Usage: "Deadline for discovering and initializing the cameras",
		},
		&cli.DurationFlag{
			Name:  "command-timeout",
			Value: 15 * time.Second,
			Usage: "Deadline for each command sent to a camera during a broadcast (0 = no limit)",
		},
	}
}

// connectMultiManager builds the context, BLE adapter and camera manager.
// Cleanup disconnects all cameras and the adapter. The returned context is
// suitable for long-running sessions: the connect deadline only covers the
// scan/init phase.
func connectMultiManager(c *cli.Context) (context.Context, *multi.Manager, func(), error) {
	loggerLevel, err := getLoggerLevel(c)
	if err != nil {
		return nil, nil, nil, err
	}

	ctx := getContext(loggerLevel, false)

	adapter, err := newAdapter(ctx, c)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to create BLE adapter: %w", err)
	}

	connectCtx := ctx
	if timeout := c.Duration("connect-timeout"); timeout > 0 {
		var cancel context.CancelFunc
		connectCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	mgr, err := multi.Connect(connectCtx, adapter, multi.ConnectOptions{
		Addrs:       c.StringSlice("addr"),
		All:         c.Bool("all"),
		AllWindow:   c.Duration("scan-window"),
		ScanTimeout: c.Duration("connect-timeout"),
	})
	if mgr == nil {
		_ = adapter.Close(ctx)
		if err != nil {
			return nil, nil, nil, err
		}
		return nil, nil, nil, fmt.Errorf("no cameras connected")
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
	}
	mgr.CommandTimeout = c.Duration("command-timeout")

	cleanup := func() {
		mgr.Close(ctx)
		_ = adapter.Close(ctx)
	}
	return ctx, mgr, cleanup, nil
}

func multiRun(c *cli.Context) error {
	args := c.Args().Slice()
	if len(args) == 0 {
		return fmt.Errorf("usage: insta360ctl multi run [flags] <command> [args...]")
	}

	name, action, err := resolveMultiAction(args, c.Bool("no-wait"))
	if err != nil {
		return err
	}

	ctx, mgr, cleanup, err := connectMultiManager(c)
	if err != nil {
		return err
	}
	defer cleanup()

	var fireAt time.Time
	if delay := c.Duration("delay"); delay > 0 {
		fmt.Printf("Firing %q in %s...\n", name, delay)
		fireAt = time.Now().Add(delay)
	}

	results := mgr.BroadcastAt(ctx, fireAt, action)
	printMultiResults(name, results)

	s := multi.Summarize(results)
	if s.Failed > 0 {
		return fmt.Errorf("%s failed on %d/%d cameras", name, s.Failed, s.Total)
	}
	return nil
}

func multiShell(c *cli.Context) error {
	ctx, mgr, cleanup, err := connectMultiManager(c)
	if err != nil {
		return err
	}
	defer cleanup()

	fmt.Println("Connected cameras:")
	for _, d := range mgr.Devices {
		fmt.Printf("  %-12s %s (%s)\n", d.Name(), d.Address, d.Model())
	}
	fmt.Println(shellHelpText)

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 64*1024)

	for {
		fmt.Print("x5> ")
		if !scanner.Scan() {
			fmt.Println()
			return nil
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		fields := strings.Fields(line)
		cmd := strings.ToLower(fields[0])
		args := append([]string{cmd}, fields[1:]...)

		switch cmd {
		case "quit", "exit", "q":
			return nil
		case "help", "?":
			fmt.Println(shellHelpText)
			continue
		case "devices":
			for _, d := range mgr.Devices {
				fmt.Printf("  %-12s %s (%s)\n", d.Name(), d.Address, d.Model())
			}
			continue
		}

		// Optional trailing delay for start/stop: "start 5s", "record start 5".
		var fireAt time.Time
		if cmd == "start" || cmd == "stop" || cmd == "record" {
			if len(args) >= 2 {
				if d, ok := tryParseDelay(args[len(args)-1]); ok {
					args = args[:len(args)-1]
					fireAt = time.Now().Add(d)
					fmt.Printf("Firing in %s...\n", d)
				}
			}
		}

		name, action, err := resolveMultiAction(args, false)
		if err != nil {
			fmt.Println("error:", err)
			continue
		}

		results := mgr.BroadcastAt(ctx, fireAt, action)
		printMultiResults(name, results)
	}
}

func tryParseDelay(s string) (time.Duration, bool) {
	if s == "" {
		return 0, false
	}
	if n, err := strconv.Atoi(s); err == nil && n > 0 {
		return time.Duration(n) * time.Second, true
	}
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d, true
	}
	return 0, false
}

// resolveMultiAction maps a command line to a display name and a per-camera
// action. args[0] is the command itself.
func resolveMultiAction(args []string, noWait bool) (string, multi.Action, error) {
	cmd := strings.ToLower(args[0])
	arg := func(i int) string {
		if i < len(args) {
			return args[i]
		}
		return ""
	}

	switch cmd {
	case "record", "rec":
		switch strings.ToLower(arg(1)) {
		case "start":
			return "record start", recordAction(true, noWait), nil
		case "stop":
			return "record stop", recordAction(false, noWait), nil
		}
		return "", nil, fmt.Errorf("usage: record <start|stop>")
	case "start":
		return "record start", recordAction(true, noWait), nil
	case "stop":
		return "record stop", recordAction(false, noWait), nil
	case "photo", "shutter":
		return "photo", func(ctx context.Context, dev *direct.Device, _ io.Writer) error {
			return dev.TakePhoto(ctx)
		}, nil
	case "marker":
		return "marker", func(ctx context.Context, dev *direct.Device, _ io.Writer) error {
			return dev.SetHighlight(ctx)
		}, nil
	case "timelapse":
		switch strings.ToLower(arg(1)) {
		case "start":
			return "timelapse start", func(ctx context.Context, dev *direct.Device, _ io.Writer) error {
				return dev.StartTimelapse(ctx)
			}, nil
		case "stop":
			return "timelapse stop", func(ctx context.Context, dev *direct.Device, _ io.Writer) error {
				return dev.StopTimelapse(ctx)
			}, nil
		}
		return "", nil, fmt.Errorf("usage: timelapse <start|stop>")
	case "mode":
		if len(args) < 2 {
			return "", nil, fmt.Errorf("usage: mode <photo|video|timelapse|hdr|bullettime>")
		}
		mode, ok := camera.ParseCaptureMode(args[1])
		if !ok {
			return "", nil, fmt.Errorf("invalid mode: %s", args[1])
		}
		return "mode " + args[1], func(ctx context.Context, dev *direct.Device, _ io.Writer) error {
			return dev.SetMode(ctx, mode)
		}, nil
	case "hdr":
		var enable bool
		switch strings.ToLower(arg(1)) {
		case "on", "true", "1":
			enable = true
		case "off", "false", "0":
			enable = false
		default:
			return "", nil, fmt.Errorf("usage: hdr <on|off>")
		}
		return "hdr " + arg(1), func(ctx context.Context, dev *direct.Device, _ io.Writer) error {
			return dev.SetHDR(ctx, enable)
		}, nil
	case "gps":
		if len(args) < 4 {
			return "", nil, fmt.Errorf("usage: gps <latitude> <longitude> <altitude>")
		}
		lat, err := strconv.ParseFloat(args[1], 64)
		if err != nil {
			return "", nil, fmt.Errorf("invalid latitude: %w", err)
		}
		lon, err := strconv.ParseFloat(args[2], 64)
		if err != nil {
			return "", nil, fmt.Errorf("invalid longitude: %w", err)
		}
		alt, err := strconv.ParseFloat(args[3], 64)
		if err != nil {
			return "", nil, fmt.Errorf("invalid altitude: %w", err)
		}
		return "gps", func(ctx context.Context, dev *direct.Device, _ io.Writer) error {
			return dev.InjectGPS(ctx, lat, lon, alt)
		}, nil
	case "battery":
		return "battery", func(ctx context.Context, dev *direct.Device, out io.Writer) error {
			info, err := dev.GetBatteryInfo(ctx)
			if err != nil {
				return err
			}
			if info.Charging {
				fmt.Fprintf(out, "battery: %d%% (charging)\n", info.Level)
			} else {
				fmt.Fprintf(out, "battery: %d%%\n", info.Level)
			}
			return nil
		}, nil
	case "storage":
		return "storage", func(ctx context.Context, dev *direct.Device, out io.Writer) error {
			info, err := dev.GetStorageInfo(ctx)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "storage: %dMB free / %dMB total (%d files)\n",
				info.FreeMB, info.TotalMB, info.FileCount)
			return nil
		}, nil
	case "status":
		return "status", func(ctx context.Context, dev *direct.Device, out io.Writer) error {
			battery, err := dev.GetBatteryInfo(ctx)
			if err != nil {
				fmt.Fprintf(out, "battery: unavailable (%v)\n", err)
			} else if battery.Charging {
				fmt.Fprintf(out, "battery: %d%% (charging)\n", battery.Level)
			} else {
				fmt.Fprintf(out, "battery: %d%%\n", battery.Level)
			}
			storage, err := dev.GetStorageInfo(ctx)
			if err != nil {
				fmt.Fprintf(out, "storage: unavailable (%v)\n", err)
			} else {
				fmt.Fprintf(out, "storage: %dMB free / %dMB total (%d files)\n",
					storage.FreeMB, storage.TotalMB, storage.FileCount)
			}
			return nil
		}, nil
	case "info":
		return "info", func(ctx context.Context, dev *direct.Device, out io.Writer) error {
			fmt.Fprintf(out, "model: %s, addr: %s\n", dev.Model(), dev.Address)
			fw, sn, err := dev.GetDeviceInfo(ctx)
			if err != nil {
				return fmt.Errorf("device info unavailable: %w", err)
			}
			if fw != "" {
				fmt.Fprintf(out, "firmware: %s\n", fw)
			}
			if sn != "" {
				fmt.Fprintf(out, "serial: %s\n", sn)
			}
			return nil
		}, nil
	case "state":
		return "state", func(ctx context.Context, dev *direct.Device, out io.Writer) error {
			resp, err := dev.GetCameraState(ctx)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "camera state: %X\n", resp)
			return nil
		}, nil
	case "raw":
		if len(args) < 2 {
			return "", nil, fmt.Errorf("usage: raw <cmd-hex> [param-hex ...]")
		}
		cmdCode, err := strconv.ParseUint(args[1], 16, 16)
		if err != nil {
			return "", nil, fmt.Errorf("invalid command code: %w", err)
		}
		var params []byte
		for i := 2; i < len(args); i++ {
			b, err := strconv.ParseUint(args[i], 16, 8)
			if err != nil {
				return "", nil, fmt.Errorf("invalid param byte: %w", err)
			}
			params = append(params, byte(b))
		}
		return fmt.Sprintf("raw 0x%02X", cmdCode), func(ctx context.Context, dev *direct.Device, out io.Writer) error {
			resp, err := dev.SendCommand(ctx, messagecode.Code(cmdCode), params)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "response: %X\n", resp)
			return nil
		}, nil
	case "power-off", "poweroff":
		return "power-off", func(ctx context.Context, dev *direct.Device, _ io.Writer) error {
			return dev.SendCommandNoResponse(ctx, messagecode.CodePowerOff, nil)
		}, nil
	default:
		return "", nil, fmt.Errorf("unknown command %q (see 'help')", args[0])
	}
}

func recordAction(start, noWait bool) multi.Action {
	return func(ctx context.Context, dev *direct.Device, _ io.Writer) error {
		if noWait {
			code := messagecode.CodeStopRecording
			if start {
				code = messagecode.CodeStartRecording
			}
			return dev.SendCommandNoResponse(ctx, code, nil)
		}
		if start {
			return dev.StartRecording(ctx)
		}
		return dev.StopRecording(ctx)
	}
}

func printMultiResults(command string, results []multi.Result) {
	fmt.Printf("=== %s ===\n", command)

	base := time.Time{}
	for _, r := range results {
		if r.DispatchedAt.IsZero() {
			continue
		}
		if base.IsZero() || r.DispatchedAt.Before(base) {
			base = r.DispatchedAt
		}
	}

	for _, r := range results {
		dispatch := "n/a"
		if !r.DispatchedAt.IsZero() {
			dispatch = "+" + r.DispatchedAt.Sub(base).Round(50*time.Microsecond).String()
		}
		rtt := "n/a"
		if !r.DispatchedAt.IsZero() && !r.FinishedAt.IsZero() {
			rtt = r.RTT().Round(100 * time.Microsecond).String()
		}
		status := "ok"
		if r.Err != nil {
			status = "error: " + r.Err.Error()
		}
		fmt.Printf("  %-12s %-18s dispatch %-9s rtt %-9s %s\n",
			r.Device.Name(), r.Device.Address, dispatch, rtt, status)
		if len(r.Output) > 0 {
			for _, line := range strings.Split(strings.TrimRight(string(r.Output), "\n"), "\n") {
				fmt.Printf("      | %s\n", line)
			}
		}
	}

	s := multi.Summarize(results)
	fmt.Printf("  -- cameras: %d, failed: %d, dispatch spread: %s, max rtt: %s\n",
		s.Total, s.Failed,
		s.DispatchSpread.Round(50*time.Microsecond),
		s.MaxRTT.Round(100*time.Microsecond))
}
