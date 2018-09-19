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

func ADSLStatus(conn *Conn) *Command {
	statusDesc := NewDesc("adsl_modem_status", "Current ADSL modem status", "status")
	return &Command{
		conn: conn,
		Cmd:  "wan adsl status",
		Metrics: []Metric{
			NewMetric(StringAfter("current modem status: "), statusDesc, Gauge),
		},
	}
}

func ADSLMode(conn *Conn) *Command {
	modeDesc := NewDesc("adsl_modem_operating_mode", "Current ADSL modem operating mode", "mode")
	return &Command{
		conn: conn,
		Cmd:  "wan adsl opmode",
		Metrics: []Metric{
			NewMetric(StringAfter("operational mode: "), modeDesc, Gauge),
		},
	}
}

func ADSLErrors(conn *Conn) *Command {
	errorDesc := NewDesc("adsl_error_count", "ADSL HEC/FEC/CRC error counts", "direction", "channel_type", "error_type")
	errSecDesc := NewDesc("adsl_error_seconds_count", "ADSL error-seconds")
	adslUpDesc := NewDesc("adsl_uptime_seconds", "How long the ADSL connection has been up, in seconds")

	return &Command{
		conn: conn,
		Cmd:  "wan adsl p",
		Metrics: []Metric{
			// TODO(fluffle): There must be a nicer way to extract these.
			NewMetric(FloatAfter("near-end FEC error fast: "),
				errorDesc, Counter, "downstream", "fast", "FEC"),
			NewMetric(FloatAfter("near-end FEC error interleaved: "),
				errorDesc, Counter, "downstream", "interleaved", "FEC"),
			NewMetric(FloatAfter("near-end CRC error fast: "),
				errorDesc, Counter, "downstream", "fast", "CRC"),
			NewMetric(FloatAfter("near-end CRC error interleaved: "),
				errorDesc, Counter, "downstream", "interleaved", "CRC"),
			NewMetric(FloatAfter("near-end HEC error fast: "),
				errorDesc, Counter, "downstream", "fast", "HEC"),
			NewMetric(FloatAfter("near-end HEC error interleaved: "),
				errorDesc, Counter, "downstream", "interleaved", "HEC"),
			NewMetric(FloatAfter("far-end FEC error fast: "),
				errorDesc, Counter, "upstream", "fast", "FEC"),
			NewMetric(FloatAfter("far-end FEC error interleaved: "),
				errorDesc, Counter, "upstream", "interleaved", "FEC"),
			NewMetric(FloatAfter("far-end CRC error fast: "),
				errorDesc, Counter, "upstream", "fast", "CRC"),
			NewMetric(FloatAfter("far-end CRC error interleaved: "),
				errorDesc, Counter, "upstream", "interleaved", "CRC"),
			NewMetric(FloatAfter("far-end HEC error fast: "),
				errorDesc, Counter, "upstream", "fast", "HEC"),
			NewMetric(FloatAfter("far-end HEC error interleaved: "),
				errorDesc, Counter, "upstream", "interleaved", "HEC"),
			NewMetric(FloatAfter("Error second after power-up\t: "),
				errSecDesc, Counter),
			NewMetric(ADSLUptime{}, adslUpDesc, Gauge),
		},
	}
}
