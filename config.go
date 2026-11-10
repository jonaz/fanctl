package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"time"
)

// Duration lets durations be written as "3s" or "1m" in JSON.
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"30s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

type Config struct {
	// How to talk to the BMC: "device" (the kernel's /dev/ipmi0, default)
	// or "ipmitool".
	IPMI       string `json:"ipmi"`
	IpmiDevice string `json:"ipmi_device"` // default /dev/ipmi0
	Ipmitool   string `json:"ipmitool"`    // path to ipmitool
	Smartctl   string `json:"smartctl"`    // path to smartctl
	NvidiaSmi  string `json:"nvidia_smi"`  // path to nvidia-smi
	// Fan mode to restore on exit so the BMC takes over again:
	// "standard", "full", "optimal", "heavyio" or "" to leave as is.
	ExitMode string `json:"exit_mode"`
	// Re-send the current duty at least this often, in case the BMC resets it.
	Reapply Duration `json:"reapply"`
	// Serve Prometheus metrics at http://<this>/metrics, e.g. ":9101" or
	// "127.0.0.1:9101". Empty = off.
	MetricsListen string `json:"metrics_listen,omitempty"`
	Zones         []Zone `json:"zones"`
}

type Zone struct {
	Name     string `json:"name"`
	IpmiZone int    `json:"ipmi_zone"` // 0 = CPU/system (FAN1-4), 1 = peripheral (FANA)
	// Default poll interval for the zone's sources. The zone re-evaluates at
	// the shortest interval among its sources.
	Interval Duration `json:"interval"`
	MinDuty  int      `json:"min_duty"`
	MaxDuty  int      `json:"max_duty"`
	// Smallest duty change worth sending; avoids constant small adjustments.
	MinChange int `json:"min_change"`
	// Readings averaged per source (moving average) before applying the
	// curve. 1 = no averaging. Useful for spiky sensors such as the CPU.
	Smoothing int `json:"smoothing"`
	// Duty used until every source has given a good reading. Default 70.
	StartupDuty *int `json:"startup_duty,omitempty"`
	// After demand drops, hold the current duty this long before ramping
	// down. Default "2m"; "0s" ramps down at once.
	RampDownDelay *Duration `json:"ramp_down_delay,omitempty"`
	// Then lower the duty by at most this many percent per minute. Default 10.
	RampDownRate *float64 `json:"ramp_down_rate,omitempty"`
	Sources      []Source `json:"sources"`
}

type Source struct {
	Type string `json:"type"` // "cpu", "disks" or "gpu"
	// Curve points as [temp °C, duty %], sorted by temperature.
	Curve [][2]float64 `json:"curve"`
	// Poll interval for this source; defaults to the zone's interval.
	Interval Duration `json:"interval,omitzero"`
	// A falling temperature is only followed once it has dropped more than
	// this many °C. 1 stops whole-degree sensors flickering 40↔41.
	Hysteresis float64 `json:"hysteresis,omitempty"`
	// cpu: hwmon driver names to read, default ["coretemp", "k10temp"].
	Hwmon []string `json:"hwmon,omitempty"`
	// disks: device paths; empty = autodetect /dev/sd* and /dev/nvme*n1.
	Devices []string `json:"devices,omitempty"`
	// disks: "smartctl" (default) or "hwmon" (drivetemp / nvme in sysfs).
	Backend string `json:"backend,omitempty"`
	// After this many failed reads in a row the source demands max duty.
	FailAfter int `json:"fail_after,omitempty"`
	// Optional: skip this source if its tool or hardware is missing,
	// instead of failing safe to max duty.
	Optional bool `json:"optional,omitempty"`
}

// minInterval is the shortest poll interval: anything shorter just spawns
// sensor commands back to back.
const minInterval = time.Second

var fanModes = map[string]int{"standard": 0, "full": 1, "optimal": 2, "heavyio": 4}

func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	c.applyDefaults()
	return &c, c.Validate()
}

func (c *Config) applyDefaults() {
	if c.IPMI == "" {
		c.IPMI = "device"
	}
	if c.IpmiDevice == "" {
		c.IpmiDevice = "/dev/ipmi0"
	}
	if c.Ipmitool == "" {
		c.Ipmitool = "ipmitool"
	}
	if c.Smartctl == "" {
		c.Smartctl = "smartctl"
	}
	if c.NvidiaSmi == "" {
		c.NvidiaSmi = "nvidia-smi"
	}
	if c.Reapply.Duration == 0 {
		c.Reapply.Duration = 5 * time.Minute
	}
	for i := range c.Zones {
		z := &c.Zones[i]
		if z.Interval.Duration == 0 {
			z.Interval.Duration = 5 * time.Second
		}
		if z.MaxDuty == 0 {
			z.MaxDuty = 100
		}
		if z.MinChange == 0 {
			z.MinChange = 2
		}
		if z.Smoothing == 0 {
			z.Smoothing = 1
		}
		if z.StartupDuty == nil {
			v := min(max(70, z.MinDuty), z.MaxDuty)
			z.StartupDuty = &v
		}
		if z.RampDownDelay == nil {
			z.RampDownDelay = &Duration{2 * time.Minute}
		}
		if z.RampDownRate == nil {
			v := 10.0
			z.RampDownRate = &v
		}
		for j := range z.Sources {
			s := &z.Sources[j]
			if s.Interval.Duration == 0 {
				s.Interval = z.Interval
			}
			if s.FailAfter == 0 {
				s.FailAfter = 3
			}
			if s.Type == "cpu" && len(s.Hwmon) == 0 {
				s.Hwmon = []string{"coretemp", "k10temp"}
			}
			if s.Type == "disks" && s.Backend == "" {
				s.Backend = "smartctl"
			}
		}
	}
}

// Validate checks a config that has been through applyDefaults (LoadConfig
// does both); the defaulted pointer fields are assumed set.
func (c *Config) Validate() error {
	if c.IPMI != "device" && c.IPMI != "ipmitool" {
		return fmt.Errorf("ipmi %q: want device or ipmitool", c.IPMI)
	}
	if c.ExitMode != "" {
		if _, ok := fanModes[c.ExitMode]; !ok {
			return fmt.Errorf("exit_mode %q: want standard, full, optimal, heavyio or empty", c.ExitMode)
		}
	}
	if c.Reapply.Duration < minInterval {
		return fmt.Errorf("reapply must be at least %s", minInterval)
	}
	if c.MetricsListen != "" {
		if _, _, err := net.SplitHostPort(c.MetricsListen); err != nil {
			return fmt.Errorf("metrics_listen %q: %w", c.MetricsListen, err)
		}
	}
	if len(c.Zones) == 0 {
		return errors.New("no zones configured")
	}
	seen := map[int]string{}
	names := map[string]bool{} // logs and metrics tell zones apart by name
	for _, z := range c.Zones {
		if prev, ok := seen[z.IpmiZone]; ok {
			return fmt.Errorf("zones %q and %q both use ipmi_zone %d", prev, z.Name, z.IpmiZone)
		}
		seen[z.IpmiZone] = z.Name
		if names[z.Name] {
			return fmt.Errorf("two zones named %q: zone names must be unique", z.Name)
		}
		names[z.Name] = true
		if z.IpmiZone < 0 || z.IpmiZone > 255 {
			return fmt.Errorf("zone %q: ipmi_zone out of range", z.Name)
		}
		if z.MinDuty < 0 || z.MaxDuty > 100 || z.MinDuty > z.MaxDuty {
			return fmt.Errorf("zone %q: need 0 <= min_duty <= max_duty <= 100", z.Name)
		}
		if z.Interval.Duration < minInterval || z.Smoothing < 1 || z.MinChange < 1 {
			return fmt.Errorf("zone %q: interval must be at least %s, smoothing and min_change at least 1", z.Name, minInterval)
		}
		if *z.StartupDuty < z.MinDuty || *z.StartupDuty > z.MaxDuty {
			return fmt.Errorf("zone %q: startup_duty must be between min_duty and max_duty", z.Name)
		}
		if z.RampDownDelay.Duration < 0 {
			return fmt.Errorf("zone %q: ramp_down_delay must not be negative", z.Name)
		}
		if *z.RampDownRate <= 0 {
			return fmt.Errorf("zone %q: ramp_down_rate must be positive", z.Name)
		}
		if len(z.Sources) == 0 {
			return fmt.Errorf("zone %q: no sources", z.Name)
		}
		for _, s := range z.Sources {
			if s.Interval.Duration < minInterval {
				return fmt.Errorf("zone %q source %s: interval must be at least %s", z.Name, s.Type, minInterval)
			}
			if s.Hysteresis < 0 {
				return fmt.Errorf("zone %q source %s: hysteresis must not be negative", z.Name, s.Type)
			}
			if s.FailAfter < 1 {
				return fmt.Errorf("zone %q source %s: fail_after must be at least 1", z.Name, s.Type)
			}
			switch s.Type {
			case "cpu", "gpu":
				if s.Backend != "" {
					return fmt.Errorf("zone %q source %s: backend is only for disks", z.Name, s.Type)
				}
			case "disks":
				if s.Backend != "smartctl" && s.Backend != "hwmon" {
					return fmt.Errorf("zone %q source disks: backend %q: want smartctl or hwmon", z.Name, s.Backend)
				}
			default:
				return fmt.Errorf("zone %q: unknown source type %q", z.Name, s.Type)
			}
			if err := validateCurve(s.Curve); err != nil {
				return fmt.Errorf("zone %q source %s: %w", z.Name, s.Type, err)
			}
		}
	}
	return nil
}

func validateCurve(c [][2]float64) error {
	if len(c) < 2 {
		return errors.New("curve needs at least 2 points")
	}
	for i, p := range c {
		if p[1] < 0 || p[1] > 100 {
			return fmt.Errorf("curve point %d: duty %.0f outside 0-100", i, p[1])
		}
		if i > 0 && p[0] <= c[i-1][0] {
			return fmt.Errorf("curve temperatures must increase (point %d)", i)
		}
	}
	return nil
}
