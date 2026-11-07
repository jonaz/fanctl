package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"strings"
	"time"
)

// source is a sensor plus the state the controller keeps for it.
type source struct {
	cfg      Source
	sensor   Sensor
	avg      window
	failures int
}

// demand returns the duty this source asks for, and a log fragment.
func (s *source) demand(ctx context.Context, maxDuty int) (float64, string) {
	r, err := s.sensor.Read(ctx)
	name := s.sensor.Name()
	if err != nil {
		s.failures++
		log.Printf("%s: read error (%d/%d): %v", name, s.failures, s.cfg.FailAfter, err)
		if s.failures >= s.cfg.FailAfter {
			return float64(maxDuty), fmt.Sprintf("%s=FAILSAFE", name)
		}
		// Partial readings (e.g. one disk failed) still count.
		if r.Temp == 0 && !r.Idle {
			return 0, fmt.Sprintf("%s=err", name)
		}
	} else {
		s.failures = 0
	}
	if r.Idle {
		s.avg.reset()
		return 0, fmt.Sprintf("%s=idle(%s)", name, r.Detail)
	}
	t := s.avg.add(r.Temp)
	d := Interpolate(s.cfg.Curve, t)
	frag := fmt.Sprintf("%s=%.1f°C→%d%%", name, t, int(math.Round(d)))
	if r.Detail != "" {
		frag += "(" + r.Detail + ")"
	}
	return d, frag
}

type zoneCtl struct {
	cfg     Zone
	sources []*source
	ipmi    *IPMI
	reapply time.Duration

	duty     int // last duty sent, -1 = none yet
	lastSent time.Time
}

// step reads all sources, takes the highest demand and sends it if needed.
func (z *zoneCtl) step(ctx context.Context, now time.Time) {
	want := float64(z.cfg.MinDuty)
	var parts []string
	for _, s := range z.sources {
		d, frag := s.demand(ctx, z.cfg.MaxDuty)
		parts = append(parts, frag)
		if d > want {
			want = d
		}
	}
	duty := clampRound(want, z.cfg.MinDuty, z.cfg.MaxDuty)

	change := duty - z.duty
	if change < 0 {
		change = -change
	}
	send := z.duty < 0 ||
		change >= z.cfg.MinChange ||
		(duty != z.duty && (duty == z.cfg.MaxDuty || duty == z.cfg.MinDuty)) ||
		now.Sub(z.lastSent) >= z.reapply
	if !send {
		return
	}
	if err := z.ipmi.SetDuty(ctx, z.cfg.IpmiZone, duty); err != nil {
		log.Printf("zone %s: %v", z.cfg.Name, err)
		return
	}
	if duty != z.duty {
		log.Printf("zone %s: %d%% [%s]", z.cfg.Name, duty, strings.Join(parts, " "))
	}
	z.duty, z.lastSent = duty, now
}

func (z *zoneCtl) loop(ctx context.Context) {
	z.step(ctx, time.Now())
	t := time.NewTicker(z.cfg.Interval.Duration)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			z.step(ctx, now)
		}
	}
}
