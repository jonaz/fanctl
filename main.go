// fanctl drives Supermicro X10/X11 fan zones from CPU, disk and GPU
// temperatures: a single static binary inspired by smfc.
package main

import (
	"context"
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
	ipmi := &IPMI{run: run, bin: cfg.Ipmitool, dryRun: *dryRun || *once}

	zones, err := buildZones(cfg, run, ipmi, exec.LookPath)
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *once {
		for _, z := range zones {
			z.step(ctx, time.Now())
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

// readTimeout bounds each external command (smartctl, nvidia-smi, ipmitool).
const readTimeout = 20 * time.Second

type lookPathFunc func(string) (string, error)

func buildZones(cfg *Config, run Runner, ipmi *IPMI, lookPath lookPathFunc) ([]*zoneCtl, error) {
	var zones []*zoneCtl
	for _, zc := range cfg.Zones {
		z := &zoneCtl{
			cfg:       zc,
			ipmi:      ipmi,
			reapply:   cfg.Reapply.Duration,
			startup:   clampRound(float64(*zc.StartupDuty), zc.MinDuty, zc.MaxDuty),
			rampDelay: zc.RampDownDelay.Duration,
			rampRate:  *zc.RampDownRate,
			duty:      -1,
			stuckMin:  readTimeout + 5*time.Second,
		}
		for _, sc := range zc.Sources {
			var sen Sensor
			switch sc.Type {
			case "cpu":
				sen = &cpuSensor{drivers: sc.Hwmon}
			case "disks":
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
			if z.tick == 0 || sc.Interval.Duration < z.tick {
				z.tick = sc.Interval.Duration
			}
		}
		if len(z.sources) == 0 {
			return nil, fmt.Errorf("zone %s: no usable sources", zc.Name)
		}
		var polls []string
		for _, s := range z.sources {
			polls = append(polls, fmt.Sprintf("%s every %s", s.sensor.Name(), s.cfg.Interval.Duration))
		}
		log.Printf("zone %s: %s; startup %d%%, ramp down after %s at %.0f%%/min",
			zc.Name, strings.Join(polls, ", "), z.startup, z.rampDelay, z.rampRate)
		zones = append(zones, z)
	}
	return zones, nil
}
