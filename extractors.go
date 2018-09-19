package main

import "fmt"

type Extractor interface {
	fmt.Stringer
	Extract(*Reader) (float64, error)
}

type FloatAfter string

func (fa FloatAfter) Extract(r *Reader) (float64, error) {
	if err := r.SeekPast(string(fa)); err != nil {
		return 0, err
	}
	return r.Float64()
}

func (fa FloatAfter) String() string {
	return fmt.Sprintf("float after %q", string(fa))
}

// Specialist extractor for system uptime.
type SystemUptime struct{}

func (_ SystemUptime) Extract(r *Reader) (float64, error) {
	// system up time:   159:32:31 (36c6410 ticks)
	if err := r.SeekPast("system up time:"); err != nil {
		return 0, err
	}
	if err := r.Skip(Not('(')); err != nil {
		return 0, err
	}
	r.Advance(1)
	i, err := r.Hex64()
	// Convert to seconds for sanity's sake.
	return float64(i) / 100, err
}

func (_ SystemUptime) String() string {
	return "system uptime"
}
