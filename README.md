# fanctl

A single static Go binary that drives Supermicro X10/X11 fan zones from
temperatures, in the spirit of [smfc](https://github.com/petersulyok/smfc).
Built for an X11SSM-F in a SilverStone RM420:

| Zone | IPMI zone | Headers | Follows |
| --- | --- | --- | --- |
| `cpu` | 0 | FAN1–FAN4 | CPU package temperature (hwmon `coretemp`) |
| `fanwall` | 1 | FANA | Hottest disk (smartctl) **or** GPU (nvidia-smi), whichever demands more |

No dependencies beyond `ipmitool`, `smartctl` (smartmontools) and, for the GPU,
`nvidia-smi`.

## How it works

- At start it sets the BMC fan mode to **Full**, so the BMC stops overriding
  the duty cycles it sets.
- Every source is polled on its own `interval` (the GPU every few seconds, the
  disks every minute) and turns its temperature into a duty through a curve
  (linear between points). The zone re-evaluates at its fastest source's
  interval, and again as soon as any reading arrives, using each source's
  latest value; it takes the **highest** demand, clamped to
  `min_duty`…`max_duty`. Sources are read in the background, so a slow or
  hung `smartctl` never delays the GPU or the fan writes, and disks are
  queried in parallel (up to 8 at once).
- **Up fast, down slow.** A higher demand is applied at once. When demand
  drops, the duty is held for `ramp_down_delay` (default 2 min, counted from
  when the drop is first seen), then lowered
  by at most `ramp_down_rate` % per minute (default 10) until it reaches the
  target. Fans react to heat immediately but don't hunt up and down.
  `startup_duty` is a floor on top of this: it can only raise the fans, and
  once every source has reported the duty goes straight to what the
  readings call for. Failsafe instead counts as a real `max_duty` demand, so
  after a sensor recovers the fans hold and ramp down from 100% and a flaky
  sensor can't make them hunt.
- `hysteresis` (°C, per source) ignores small temperature drops, so a disk
  flickering between 40 and 41 °C doesn't move the fans. `smoothing` (moving
  average, default 1 = off) helps with spiky sensors such as the CPU.
- A new duty is sent only when it moves at least `min_change` %, hits min/max,
  reaches the target at the end of a ramp-down, or `reapply` time has passed
  (in case the BMC resets it).
- Disks are read with `smartctl -n standby`, so **sleeping disks are not woken**.
  If all disks sleep, the disk source asks for nothing and `min_duty` applies.
- On SIGTERM/SIGINT it restores the BMC fan mode (`exit_mode`, default
  `optimal`), so the BMC takes over again.

### Failsafes

- Until every source has given a good reading, the zone runs at
  `startup_duty` (default 70%, kept within `min_duty`…`max_duty`; setting it
  outside that range is an error).
- A single failed read keeps the source's last demand. If the disks were all
  asleep and a read then fails, the zone goes back to `startup_duty`.
- A partial reading (some disks unreadable) can only raise demand, never
  lower it or release `startup_duty`: the missing disk may be the hot one.
- A source that fails to read `fail_after` times in a row (default 3), or
  whose read runs longer than it legitimately can (25 s, or 20 s per batch of
  8 disks plus 5 s, whatever the poll interval), demands `max_duty` until it
  recovers. This includes a single disk that stops answering: the fan wall
  goes to 100% and the log says which disk. The first reading after
  recovery isn't averaged with readings from before the failure.
- A failed fan write is retried at most every half tick, so a hung BMC
  doesn't block the zone on every reading.
- `optional: true` on a source skips it at startup if its tool is missing
  (used for the GPU), instead of failing safe.
- On shutdown it waits at most 2 s for running reads, so a hung disk can't
  stop the BMC fan mode being restored.
- If fanctl dies, systemd restarts it within 5 s. If it is stopped, the BMC
  gets control back.

## Install

```bash
sudo install -m 755 fanctl /usr/local/bin/fanctl
sudo install -m 644 fanctl.example.json /etc/fanctl.json
sudo install -m 644 fanctl.service /etc/systemd/system/fanctl.service
```

Lower the BMC's low-RPM alarms first, or quiet fans make the BMC pulse them to
100%:

```bash
sudo ipmitool sensor thresh FAN1 lower 0 100 200
sudo ipmitool sensor thresh FANA lower 0 100 200
```

Check what it sees, without touching the BMC:

```bash
sudo fanctl -config /etc/fanctl.json -once
```

`-once` prints each zone's readings and the duty it would set (the `ipmitool`
commands are logged as `dry-run:`). `-dry-run` runs the full loop the same way.

Then enable it:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now fanctl
journalctl -u fanctl -f
```

Logs show a line whenever a zone's duty changes, e.g.

```
zone fanwall: 60% [disks=40.0°C→60%(hottest /dev/sdc, 2 asleep) gpu=55.0°C→37%]
```

## Configuration

See `fanctl.example.json`. Curve points are `[°C, duty %]` with increasing
temperatures. Starting values:

| Source | Curve | Reasoning |
| --- | --- | --- |
| CPU | 40→25, 55→35, 65→50, 75→75, 85→100 | E3-1230 v6 throttles near 100 °C |
| Disks | 34→30, 37→40, 40→60, 43→85, 45→100 | Keep the hottest disk around 40 °C |
| GPU | 45→30, 60→40, 70→60, 80→100 | The 2060 has its own fans; the wall feeds it fresh air |

Timing in the example:

| Zone | Polling | Ramp down | Startup |
| --- | --- | --- | --- |
| `cpu` | CPU every 2 s, averaged over 3 | after 30 s, 20 %/min | 50% |
| `fanwall` | GPU every 5 s, disks every 60 s, both 1 °C hysteresis | after 2 min, 5 %/min | 70% |

Zone keys: `interval` (default for its sources), `min_duty`, `max_duty`,
`min_change`, `smoothing`, `startup_duty`, `ramp_down_delay`, `ramp_down_rate`.
Source keys: `curve`, `interval`, `hysteresis`, `fail_after`, `optional`,
`devices` (disk list; empty = autodetect non-removable `sd*` and `nvme*`),
`hwmon` (CPU driver names). Top level: `exit_mode`, `reapply` and tool paths
`ipmitool`, `smartctl`, `nvidia_smi`.

Tune the disk curve with your temperature graph: if the hottest disk sits above
42 °C under load, raise the 40 °C and 43 °C points.

Do not run it at the same time as smfc or other tools that set fan duties.

## Releases

Push a version tag to build static linux/amd64 and linux/arm64 binaries and
publish them with `gh release create`, each with a `.sha256` file, plus the
example config and systemd unit:

```bash
git tag v0.1.0 && git push origin v0.1.0
```

## Build

```bash
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=0.1.0" -o fanctl .
go test ./...
```
