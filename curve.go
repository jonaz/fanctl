package main

import "math"

// Interpolate maps a temperature to a duty using linear interpolation
// between curve points, clamped to the first/last point outside the range.
func Interpolate(curve [][2]float64, temp float64) float64 {
	if temp <= curve[0][0] {
		return curve[0][1]
	}
	last := curve[len(curve)-1]
	if temp >= last[0] {
		return last[1]
	}
	for i := 1; i < len(curve); i++ {
		a, b := curve[i-1], curve[i]
		if temp <= b[0] {
			f := (temp - a[0]) / (b[0] - a[0])
			return a[1] + f*(b[1]-a[1])
		}
	}
	return last[1]
}

// window is a fixed-size moving average.
type window struct {
	size int
	vals []float64
}

func (w *window) add(v float64) float64 {
	w.vals = append(w.vals, v)
	if len(w.vals) > w.size {
		w.vals = w.vals[len(w.vals)-w.size:]
	}
	sum := 0.0
	for _, x := range w.vals {
		sum += x
	}
	return sum / float64(len(w.vals))
}

func (w *window) reset() { w.vals = w.vals[:0] }

func clampRound(v float64, lo, hi int) int {
	d := int(math.Round(v))
	if d < lo {
		return lo
	}
	if d > hi {
		return hi
	}
	return d
}
