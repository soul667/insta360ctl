# Multi-camera control

The `multi` command set controls several Insta360 cameras from one process
over a single BLE adapter. It was written for the "two X5" use case but works
for any mix of BLE-controllable models.

## Why one process instead of two

A one-shot `insta360ctl direct ...` invocation performs the full sequence for
every command:

```
scan -> connect -> service discovery -> subscribe -> command -> disconnect
```

Connection setup takes seconds and varies from run to run. If you trigger two
processes in parallel, each camera sees a different, unpredictable delay.

`multi` changes the sequence to:

```
scan -> connect -> initialize              (once, in parallel)
                    |
                    +-- shared barrier -> command to all cameras -> report
                    |
                    +-- shared barrier -> command to all cameras -> report
                    ...
```

All connections stay open. Commands are dispatched after a barrier in which
every worker is parked, so the residual jitter is only the BLE write path.
After every broadcast the tool prints per-camera dispatch offsets, round-trip
times and the overall dispatch spread.

## Commands

```bash
# One-shot: connect, fire, disconnect
sudo insta360ctl multi run --addr AA:BB:CC:DD:EE:FF --addr 11:22:33:44:55:66 record start
sudo insta360ctl multi run --addr AA:BB --addr CC:DD --no-wait record stop

# Scheduled fire (connect first, then fire after the delay)
sudo insta360ctl multi run --addr AA:BB --addr CC:DD --delay 5s record start

# Interactive session: connections stay open
sudo insta360ctl multi shell --addr AA:BB --addr CC:DD
x5> mode video
x5> start
x5> start 5s
x5> stop
x5> status
x5> quit

# Attach to every Insta360 camera in range
sudo insta360ctl multi shell --all --scan-window 5s
```

Addresses are matched case-insensitively as substrings of the BLE address,
so a unique suffix is enough. Both `--addr A --addr B` and
`--addr A,B` work.

`--no-wait` sends Write Without Response and does not wait for the camera
acknowledgment; it is the lower-latency, lower-reliability option.

## Reading the timing report

```
=== record start ===
  X5 A1B2C3    AA:BB:CC:DD:EE:FF  dispatch +0s        rtt 42.1ms   ok
  X5 D4E5F6    11:22:33:44:55:66  dispatch +1.15ms   rtt 39.8ms   ok
  -- cameras: 2, failed: 0, dispatch spread: 1.15ms, max rtt: 42.1ms
```

- **dispatch spread** — the gap between the first and the last camera being
  handed the command. This is the number the tool can guarantee and improve
  (typically well under 10ms; often around 1ms with the D-Bus backend and
  `--no-wait`).
- **rtt** — time until the camera acknowledgment (or write completion with
  `--no-wait`). It says nothing about when the sensor actually started.

## What is *not* guaranteed

The BLE command is only the trigger. Once received, each camera runs its own
pipeline: mode validation, buffer allocation, sensor start, encoding. That
per-unit internal latency is not observable over BLE and can differ between
two physically different X5s.

Practical consequences:

1. Treat the dispatch spread as the *lower bound* of the real sync error.
2. Measure the real offset once, per pair of cameras:
   - clap once sharply in view of both cameras at recording start
     (sharp audio transient + visible hands), or wave an LED/phone flash;
   - align both clips on the transient (audio waveform or first flash frame)
     in any editor or with a short script.
3. If the measured offset is stable (same sign and magnitude every take),
   correct it in post. If it drifts, repeat the measurement per take.
4. Nothing in the BLE remote protocol provides a timecode or genlock signal;
   frame-exact sync is a post-processing/external-clocking problem.

## Troubleshooting

- `no cameras found` — check that the cameras are powered on, not connected
  to the phone app, and that `sudo insta360ctl scan --timeout 10s` lists
  them.
- One camera fails to initialize — it may still be bonded to another host or
  the phone. Power-cycle it and retry.
- Large dispatch spread (>50ms) — avoid USB 3.0 interference near the
  adapter, try `--no-wait`, and keep both cameras roughly equidistant from
  the host.
