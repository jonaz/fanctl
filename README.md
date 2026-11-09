# fanctl

A single static Go binary that drives Supermicro X10/X11 fan zones from
temperatures, in the spirit of [smfc](https://github.com/petersulyok/smfc).
Built for an X11SSM-F in a SilverStone RM420:

| Zone | IPMI zone | Headers | Follows |
| --- | --- | --- | --- |
| `cpu` | 0 | FAN1–FAN4 | CPU package temperature (hwmon `coretemp`) |
| `fanwall` | 1 | FANA | Hottest disk (smartctl), GPU (nvidia-smi) or sustained CPU load, whichever demands more |

It talks to the BMC directly through the kernel's IPMI device `/dev/ipmi0`, so
at runtime it only needs `smartctl` (smartmontools, or not even that with
the `hwmon` disk backend) and, for the GPU, `nvidia-smi`. `ipmitool` can be used instead (`"ipmi": "ipmitool"`), and is
handy for the one-off threshold setup below.

## How it works

- At start it sets the BMC fan mode to **Full**, so the BMC stops overriding
  the duty cycles it sets.
- Every source is polled on its own `interval` (the GPU every few seconds, the
  disks every minute) and turns its temperature into a duty through a curve
  (linear between points). The zone re-evaluates every second and as soon
  as any reading arrives (and only then, or when a read turns stuck),
  using each source's latest value; it takes the
  **highest** demand, clamped to `min_duty`…`max_duty`. Each read runs in
  the background, at most one per source at a time, so a slow or hung
  `smartctl` never delays the GPU or the fan writes, and disks are
  queried in parallel (up to 8 at once). Fan writes run on their own too
  (always the latest duty), so a hung BMC never holds up polling; each
  BMC command is given up on after 5 s.
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
- Disks can instead be read from sysfs with `"backend": "hwmon"` on the
  `disks` source; see [Disk temperatures without smartctl](#disk-temperatures-without-smartctl).
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
  One hotter than the current reading also enters `smoothing` and
  `hysteresis`, so the next full reading carries on from it. It counts as
  a failed read towards `fail_after`.
- Output from a command that was killed (timeout) or whose stdout was held
  open by something it left behind is never used, even if it parses.
- A source that fails to read `fail_after` times in a row (default 3), or
  whose read runs longer than it legitimately can (25 s, or 20 s per batch of
  8 disks plus 5 s, checked on time whatever the poll interval), demands `max_duty` until it
  recovers. A stuck read is given up on, so the next poll still runs and
  can recover the source; while it keeps hanging, at most one more read is
  started. This includes a single disk that stops answering: the fan wall
  goes to 100% and the log says which disk. The first reading after
  recovery isn't averaged with readings from before the failure.
- A failed fan write is retried after 1 s, doubling up to 30 s, so a hung
  BMC isn't hammered. Only a jump to `max_duty` (say, a failsafe) is tried
  at once.
- `optional: true` on a source skips it at startup if its tool is missing
  (used for the GPU), instead of failing safe.
- On shutdown it waits at most 2 s for running reads, so a hung disk can't
  stop the BMC fan mode being restored.
- If fanctl dies, systemd restarts it within 5 s. If it is stopped, the BMC
  gets control back.

## Install

The IPMI device needs the `ipmi_si` and `ipmi_devintf` kernel modules (the
same ones `ipmitool` uses). Load them now and at every boot:

```bash
sudo modprobe -a ipmi_si ipmi_devintf
printf 'ipmi_si\nipmi_devintf\n' | sudo tee /etc/modules-load.d/ipmi.conf
```

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

`-once` prints each zone's readings and the duty it would set (the BMC
commands are logged as `dry-run: raw 0x30 0x70 …`, the same bytes for either
backend, and it warns if `/dev/ipmi0` is missing). `-dry-run` runs the full
loop the same way.

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
| CPU (fan wall) | 75→0, 80→50, 90→100 | The wall supplies the CPU cooler's intake air |

Timing in the example:

| Zone | Polling | Ramp down | Startup |
| --- | --- | --- | --- |
| `cpu` | CPU every 2 s, averaged over 3 | after 30 s, 20 %/min | 50% |
| `fanwall` | GPU and CPU every 5 s, disks every 60 s (GPU and disks with 1 °C hysteresis) | after 2 min, 5 %/min | 70% |

The `fanwall` zone also has a `cpu` source, because the fan wall supplies the
intake air for the CPU cooler. Below 75 °C it asks for 0%, so the disks and GPU
decide as before; under sustained load above that, the wall helps the CPU fan.
The zone's 2-minute ramp-down delay keeps a short spike from leaving the wall
running high for long. A source type can be used in several zones; each zone
polls it on its own.

### Disk temperatures without smartctl

`"backend": "hwmon"` on the `disks` source reads `temp1_input` from the
kernel's hwmon devices instead of running `smartctl`, so smartmontools isn't
needed:

- **NVMe**: built in (Linux 5.5+), one hwmon `nvme` per controller.
- **SATA**: the `drivetemp` driver (Linux 5.6+), one hwmon per disk. It does
  not support SAS drives (use `smartctl` for those), and SATA disks behind a
  SAS HBA only show up if the HBA passes ATA commands through.

```bash
sudo modprobe drivetemp
echo drivetemp | sudo tee /etc/modules-load.d/drivetemp.conf
```

With `devices` empty it reads the same disks smartctl would (non-removable
`sd*` and `nvme*`). While `drivetemp` is loaded, an autodetected SATA disk
that it found no sensor on (some older SSDs have none; SAS disks) is left
out and logged at startup as not monitored, and the zone log says e.g.
`hottest /dev/sdb, 1 without sensor`. Without `drivetemp` loaded, every
SATA disk counts as a failed read, so a missing module fails safe instead
of letting an NVMe decide alone. A disk listed in `devices` must have a
sensor, or it counts as a failed read. fanctl checks all this at startup
and refuses to start (or skips the source if it is `optional`) if a read
could only fail. Each read is given up on after 10 s. A sysfs read can't be
cancelled, so a disk whose read still hangs is not read again until it
answers; until then it counts as a failed read, logged as `read hung since
<time>`. With NVMe multipath, every controller's sensor is read.

**Spin-down: not yet verified.** Unlike `smartctl -n standby`, the hwmon
backend has no way to skip a sleeping disk, and it is not yet known whether
reading `drivetemp` wakes one (see issue #2). Until that is tested, keep
the default `smartctl` backend on systems where disks spin down.

Zone keys: `interval` (default for its sources; at least 1 s), `min_duty`, `max_duty`,
`min_change`, `smoothing`, `startup_duty`, `ramp_down_delay`, `ramp_down_rate`.
Source keys: `curve`, `interval`, `hysteresis`, `fail_after`, `optional`,
`devices` (disk list; empty = autodetect non-removable `sd*` and `nvme*`),
`backend` (disks: `smartctl`, the default, or `hwmon`),
`hwmon` (CPU driver names). Top level: `exit_mode`, `reapply` (at least 1 s),
`ipmi` (`device`, the default, or `ipmitool`), `ipmi_device` (default
`/dev/ipmi0`) and tool paths `ipmitool`, `smartctl`, `nvidia_smi`.

With `"ipmi": "device"` a BMC error shows its completion code, e.g.
`/dev/ipmi0 raw 0x30 0x70 0x66 0x01 0x01 0x32: completion code 0xc1 (invalid
command)`. If the device backend misbehaves, set `"ipmi": "ipmitool"` and
restart; no rebuild needed.

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

Needs Go 1.25 or later (the tests use `testing/synctest`).

```bash
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=0.1.0" -o fanctl .
go test ./...
```
