// fanctl drives Supermicro X10/X11 fan zones from CPU, disk and GPU
// temperatures: a single static binary inspired by smfc.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

var version = "dev"

func main() {
	cfgPath := flag.String("config", "/etc/fanctl.json", "config file")
	dryRun := flag.Bool("dry-run", false, "read sensors and log, but never talk to the BMC")
	once := flag.Bool("once", false, "read every source once, print the duty per zone, and exit (no BMC writes)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	log.SetFlags(0) // systemd/journald adds timestamps

	if *showVersion {
		fmt.Println(version)
		return
	}
	cfg, err := LoadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	run := execRunner{timeout: readTimeout}
	ipmi, closeBMC, err := newBMC(cfg, *dryRun || *once)
	if err != nil {
		log.Fatal(err)
	}
	defer closeBMC()

	zones, err := buildZones(cfg, run, ipmi, exec.LookPath)
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *once {
		for _, z := range zones {
			z.step(ctx)
		}
		return
	}

	// Full mode: the BMC stops overriding the duty we set.
	if err := ipmi.SetMode(ctx, fanModes["full"]); err != nil {
		log.Fatalf("set fan mode full: %v", err)
	}
	log.Printf("fanctl %s: fan mode full, %d zones", version, len(zones))

	var wg sync.WaitGroup
	for _, z := range zones {
		wg.Add(1)
		go func(z *zoneCtl) { defer wg.Done(); z.loop(ctx) }(z)
	}
	<-ctx.Done()
	wg.Wait()

	// Hand control back to the BMC so fans never stay at a fixed low duty.
	if cfg.ExitMode != "" {
		bg, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := ipmi.SetMode(bg, fanModes[cfg.ExitMode]); err != nil {
			log.Printf("restore fan mode %s: %v", cfg.ExitMode, err)
			os.Exit(1)
		}
		log.Printf("fan mode restored to %s", cfg.ExitMode)
	}
}

// newBMC sets up the configured IPMI backend. A dry run never talks to the
// BMC, so it doesn't open the device either, but warns if it is missing.
func newBMC(cfg *Config, dryRun bool) (*IPMI, func(), error) {
	switch {
	case cfg.IPMI == "ipmitool":
		return newIPMI(ipmitool{run: execRunner{timeout: ipmiTimeout}, bin: cfg.Ipmitool}, dryRun), func() {}, nil
	case dryRun:
		if _, err := os.Stat(cfg.IpmiDevice); err != nil {
			log.Printf("warning: %v; a real run would fail (modprobe -a ipmi_si ipmi_devintf, or set \"ipmi\": \"ipmitool\")", err)
		}
		return newIPMI(nil, true), func() {}, nil
	}
	d, err := openIPMIDevice(cfg.IpmiDevice)
	if err != nil {
		return nil, nil, err
	}
	return newIPMI(d, false), func() { d.Close() }, nil
}

type lookPathFunc func(string) (string, error)

func buildZones(cfg *Config, run Runner, ipmi BMC, lookPath lookPathFunc) ([]*zoneCtl, error) {
	var zones []*zoneCtl
	for _, zc := range cfg.Zones {
		z := newZoneCtl(zc, ipmi, cfg.Reapply.Duration)
		for _, sc := range zc.Sources {
			var sen Sensor
			switch sc.Type {
			case "cpu":
				sen = &cpuSensor{drivers: sc.Hwmon}
			case "disks":
				if sc.Backend == "hwmon" {
					// Same rules as each read, so a setup that could only
					// fail safe is caught here.
					p := planDisks(sc.Devices)
					var err error
					switch {
					case len(p.failed) > 0:
						err = errors.New(strings.Join(p.failed, "; "))
					case len(p.hw) == 0:
						err = errors.New("no disk with a temperature sensor")
					}
					if err != nil && !drivetempLoaded() {
						err = fmt.Errorf("%w (add drivetemp to /etc/modules-load.d/drivetemp.conf to load it at boot)", err)
					}
					if err != nil {
						if sc.Optional {
							log.Printf("zone %s: disks: %v, skipping disks", zc.Name, err)
							continue
						}
						return nil, fmt.Errorf("zone %s: disks: %w", zc.Name, err)
					}
					var labels []string
					for _, h := range p.hw {
						if l := h.label(); !contains(labels, l) { // multipath: one disk, several hwmons
							labels = append(labels, l)
						}
					}
					log.Printf("zone %s: disk sensors: %s", zc.Name, strings.Join(labels, " "))
					if len(p.noSensor) > 0 {
						log.Printf("zone %s: disks without a temperature sensor, not monitored: %s", zc.Name, strings.Join(p.noSensor, " "))
					}
					sen = &hwmonDiskSensor{devices: sc.Devices}
					break
				}
				if _, err := lookPath(cfg.Smartctl); err != nil {
					if sc.Optional {
						log.Printf("zone %s: smartctl not found, skipping disks", zc.Name)
						continue
					}
					return nil, fmt.Errorf("zone %s: smartctl: %w", zc.Name, err)
				}
				sen = &diskSensor{run: run, bin: cfg.Smartctl, devices: sc.Devices}
				if len(sc.Devices) == 0 {
					log.Printf("zone %s: disks autodetected: %s", zc.Name, strings.Join(detectDisks(), " "))
				}
			case "gpu":
				if _, err := lookPath(cfg.NvidiaSmi); err != nil {
					if sc.Optional {
						log.Printf("zone %s: nvidia-smi not found, skipping gpu", zc.Name)
						continue
					}
					return nil, fmt.Errorf("zone %s: nvidia-smi: %w", zc.Name, err)
				}
				sen = &gpuSensor{run: run, bin: cfg.NvidiaSmi}
			}
			z.addSource(&source{cfg: sc, sensor: sen, avg: window{size: zc.Smoothing}})
		}
		if len(z.sources) == 0 {
			return nil, fmt.Errorf("zone %s: no usable sources", zc.Name)
		}
		var polls []string
		for _, s := range z.sources {
			polls = append(polls, fmt.Sprintf("%s every %s", s.sensor.Name(), s.cfg.Interval.Duration))
		}
		log.Printf("zone %s: %s; startup %d%%, ramp down after %s at %g%%/min",
			zc.Name, strings.Join(polls, ", "), *zc.StartupDuty, zc.RampDownDelay.Duration, *zc.RampDownRate)
		zones = append(zones, z)
	}
	return zones, nil
}
