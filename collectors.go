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
	cmd, dir := "wan adsl linedata near", "downstream"
	if up {
		cmd, dir = "wan adsl linedata far", "upstream"
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
		Cmd:  "wan adsl chandata",
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
	errorDesc := NewDesc("adsl_framing_error_count", "ADSL HEC/FEC/CRC error counts", "direction", "channel_type", "error_type")
	errSecDesc := NewDesc("adsl_error_seconds_count", "ADSL error-seconds")
	adslUpDesc := NewDesc("adsl_uptime_seconds", "How long the ADSL connection has been up, in seconds")

	return &Command{
		conn: conn,
		Cmd:  "wan adsl perfdata",
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

func ATMCells(conn *Conn) *Command {
	cellsDesc := NewDesc("atm_cell_count", "The number of ATM cells received or transmitted", "direction", "channel_type")

	return &Command{
		conn: conn,
		Cmd:  "wan adsl cellcnt",
		Metrics: []Metric{
			NewMetric(FloatAfter("ActiveRxCellsFast        = "),
				cellsDesc, Counter, "downstream", "fast"),
			NewMetric(FloatAfter("ActiveRxCellsInterleaved = "),
				cellsDesc, Counter, "downstream", "interleaved"),
			NewMetric(FloatAfter("ActiveTxCellsFast        = "),
				cellsDesc, Counter, "upstream", "fast"),
			NewMetric(FloatAfter("ActiveTxCellsInterleaved = "),
				cellsDesc, Counter, "upstream", "interleaved"),
		},
	}
}

func SARCounters(conn *Conn) *Command {
	packetsDesc := NewDesc("adsl_packet_count", "The number of packets received or transmitted over the ADSL interface", "direction")
	discardsDesc := NewDesc("adsl_packet_discard_count", "The number of packets discarded by the ADSL interface", "direction")
	errsDesc := NewDesc("adsl_packet_error_count", "The number of packet errors observed by the ADSL interface", "direction", "error_type")
	resetsDesc := NewDesc("adsl_soft_reset_count", "The number of soft resets")
	mpoaErrsDesc := NewDesc("adsl_mpoa_error_count", "The number of MPoA errors")

	return &Command{
		conn: conn,
		Cmd:  "wan hwsar disp",
		Metrics: []Metric{
			NewMetric(HexAfter("inPkts         = "),
				packetsDesc, Counter, "downstream"),
			NewMetric(HexAfter("inDiscards     = "),
				discardsDesc, Counter, "downstream"),
			NewMetric(HexAfter("inBufErr       = "),
				errsDesc, Counter, "downstream", "buffer"),
			NewMetric(HexAfter("inCrcErr       = "),
				errsDesc, Counter, "downstream", "CRC"),
			NewMetric(HexAfter("inBufOverflow  = "),
				errsDesc, Counter, "downstream", "buffer_overflow"),
			NewMetric(HexAfter("inBufMaxLenErr = "),
				errsDesc, Counter, "downstream", "buffer_max_len"),
			NewMetric(HexAfter("inBufLenErr    = "),
				errsDesc, Counter, "downstream", "buffer_len"),
			NewMetric(HexAfter("outPkts        = "),
				packetsDesc, Counter, "upstream"),
			NewMetric(HexAfter("outDiscards    = "),
				discardsDesc, Counter, "upstream"),
			NewMetric(HexAfter("softRstCnt     = "),
				resetsDesc, Counter),
			NewMetric(HexAfter("inMpoaErr      = "),
				mpoaErrsDesc, Counter),
		},
	}
}
