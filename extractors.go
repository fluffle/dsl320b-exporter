package main

import "fmt"

// An extractor is responsible for scanning telnet command responses and
// extracting floats from them. Extractors should not consume from the
// reader once they have extracted the float.
type Extractor interface {
	fmt.Stringer
	Extract(*Reader) (float64, error)
}

// The FloatAfter extractor scans the response for the string and
// extracts the number directly after it.
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

// The SystemUptime extractor is a specialist extractor that converts
// the uptime ticks (expressed as a hexadecimal unsigned int) to seconds.
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
