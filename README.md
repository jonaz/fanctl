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
- Each zone runs on its own interval. Every source turns its temperature into a
  duty through a curve (linear between points). The zone uses the **highest**
  demand, clamped to `min_duty`…`max_duty`.
- Readings are smoothed with a moving average (`smoothing` samples).
  A new duty is sent only when it moves at least `min_change` %, hits min/max,
  or `reapply` time has passed (in case the BMC resets it).
- Disks are read with `smartctl -n standby`, so **sleeping disks are not woken**.
  If all disks sleep, the disk source asks for nothing and `min_duty` applies.
- On SIGTERM/SIGINT it restores the BMC fan mode (`exit_mode`, default
  `optimal`), so the BMC takes over again.

### Failsafes

- A source that fails to read `fail_after` times in a row (default 3) demands
  `max_duty` until it recovers. This includes a single disk that stops
  answering: the fan wall goes to 100% and the log says which disk.
- `optional: true` on a source skips it at startup if its tool is missing
  (used for the GPU), instead of failing safe.
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

Other keys: `devices` (disk list; empty = autodetect non-removable `sd*` and
`nvme*`), `hwmon` (CPU driver names), `interval`, `smoothing`, `min_change`,
`fail_after`, `optional`, and tool paths `ipmitool`, `smartctl`, `nvidia_smi`.

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
