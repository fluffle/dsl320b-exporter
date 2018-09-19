package main

import (
	"fmt"
	"strconv"
	"strings"
)

// An extractor is responsible for scanning telnet command responses and
// extracting numbers and labels from them.
type Extractor interface {
	fmt.Stringer
	Extract(*Reader) (float64, []string, error)
}

// The FloatAfter extractor scans the response for the string and
// extracts the number directly after it.
type FloatAfter string

func (fa FloatAfter) Extract(r *Reader) (float64, []string, error) {
	if err := r.SeekPast(string(fa)); err != nil {
		return 0, nil, err
	}
	f, err := r.Float64()
	return f, nil, err
}

func (fa FloatAfter) String() string {
	return fmt.Sprintf("float after %q", string(fa))
}

// The HexAfter extractor scans the response for the string and
// extracts the hexadecimal integer directly after it.
type HexAfter string

func (ha HexAfter) Extract(r *Reader) (float64, []string, error) {
	if err := r.SeekPast(string(ha)); err != nil {
		return 0, nil, err
	}
	f, err := r.Hex64()
	return float64(f), nil, err
}

func (ha HexAfter) String() string {
	return fmt.Sprintf("hex int after %q", string(ha))
}

// The StringAfter extractor scans the response for the string and
// extracts the remainder of the line to a label.
type StringAfter string

func (sa StringAfter) Extract(r *Reader) (float64, []string, error) {
	if err := r.SeekPast(string(sa)); err != nil {
		return 0, nil, err
	}
	// Lines from the modem's telnet interface are \r\n terminated.
	b, err := r.Scan(Not('\r'))
	if err != nil {
		return 0, nil, err
	}
	return 1, []string{string(b)}, nil
}

func (sa StringAfter) String() string {
	return fmt.Sprintf("string after %q", string(sa))
}

// The SystemUptime extractor is a specialist extractor that converts
// the uptime ticks (expressed as a hexadecimal unsigned int) to seconds.
type SystemUptime struct{}

func (_ SystemUptime) Extract(r *Reader) (float64, []string, error) {
	// system up time:   159:32:31 (36c6410 ticks)
	if err := r.SeekPast("system up time:"); err != nil {
		return 0, nil, err
	}
	if err := r.Skip(Not('(')); err != nil {
		return 0, nil, err
	}
	r.Advance(1)
	i, err := r.Hex64()
	// Convert to seconds for sanity's sake.
	return float64(i) / 100, nil, err
}

func (_ SystemUptime) String() string {
	return "system uptime"
}

type ADSLUptime struct{}

func (_ ADSLUptime) Extract(r *Reader) (float64, []string, error) {
	// ADSL uptime    32:26:23
	if err := r.SeekPast("ADSL uptime"); err != nil {
		return 0, nil, err
	}
	if err := r.Skip(Is(' ')); err != nil {
		return 0, nil, err
	}
	b, err := r.Scan(Not('\r'))
	if err != nil {
		return 0, nil, err
	}
	parts := make([]float64, 0, 3)
	for _, s := range strings.Split(string(b), ":") {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0, nil, fmt.Errorf("unable to parse uptime from %q: strconv: %v", err)
		}
		parts = append(parts, f)
	}
	switch len(parts) {
	case 3:
		return parts[0]*3600 + parts[1]*60 + parts[2], nil, nil
	case 2:
		return parts[0]*60 + parts[1], nil, nil
	case 1:
		return parts[0], nil, nil
	}
	return 0, nil, fmt.Errorf("unable to parse uptime from %q: too many parts", b)
}

func (_ ADSLUptime) String() string {
	return "adsl uptime"
}
