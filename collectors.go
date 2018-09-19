package main

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// For my personal sanity.
const (
	Counter prometheus.ValueType = prometheus.CounterValue
	Gauge   prometheus.ValueType = prometheus.GaugeValue
)

// The Prometheus client library performs collection concurrently.
// This won't play well with our telnet interface, so we register
// a single Aggregator collector which serializes collection.
type Aggregator struct {
	c  []prometheus.Collector
	mu sync.Mutex
}

func NewAggregator(coll ...prometheus.Collector) *Aggregator {
	return &Aggregator{c: coll}
}

func (agg *Aggregator) Describe(ch chan<- *prometheus.Desc) {
	agg.mu.Lock()
	defer agg.mu.Unlock()
	for _, coll := range agg.c {
		coll.Describe(ch)
	}
}

func (agg *Aggregator) Collect(ch chan<- prometheus.Metric) {
	agg.mu.Lock()
	defer agg.mu.Unlock()
	for _, coll := range agg.c {
		coll.Collect(ch)
	}
}

// The NoiseMargin collector collects noise margin and line attenuation stats.
// Because we need to run different commands to collect upstream and
// downstream stats, this constructor has an additional "up" parameter.
// If this is true, we collect upstream stats, if false, downstream.
func NoiseMargin(conn *Conn, up bool) *Command {
	cmd, dir := "wan adsl l n", "downstream"
	if up {
		cmd, dir = "wan adsl l f", "upstream"
	}
	marginDesc := NewDesc("noise_margin_db", "SNR margin, in dB", "direction")
	attenDesc := NewDesc("line_attenuation_db", "Line attenuation, in dB", "direction")
	return &Command{
		conn: conn,
		Cmd:  cmd,
		Metrics: []Metric{
			NewMetric(FloatAfter("noise margin "+dir+": "), marginDesc, Gauge, dir),
			NewMetric(FloatAfter("attenuation "+dir+": "), attenDesc, Gauge, dir),
		},
	}
}

// The SyncRate collector collects line sync rate stats.
func SyncRate(conn *Conn) *Command {
	syncDesc := NewDesc("line_sync_rate_kbps", "Line sync rate, in kbps", "direction", "channel_type")
	return &Command{
		conn: conn,
		Cmd:  "wan adsl c",
		Metrics: []Metric{
			NewMetric(FloatAfter("near-end interleaved channel bit rate: "),
				syncDesc, Gauge, "downstream", "interleaved"),
			NewMetric(FloatAfter("near-end fast channel bit rate: "),
				syncDesc, Gauge, "downstream", "fast"),
			NewMetric(FloatAfter("far-end interleaved channel bit rate: "),
				syncDesc, Gauge, "upstream", "interleaved"),
			NewMetric(FloatAfter("far-end fast channel bit rate: "),
				syncDesc, Gauge, "upstream", "fast"),
		},
	}
}

// The SysUptime collector collects the current system uptime.
func SysUptime(conn *Conn) *Command {
	uptimeDesc := NewDesc("system_uptime_seconds", "System uptime, in seconds")
	return &Command{
		conn: conn,
		Cmd:  "sys version",
		Metrics: []Metric{
			NewMetric(SystemUptime{}, uptimeDesc, Gauge),
		},
	}
}
