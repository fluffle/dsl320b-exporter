package main

import (
	"sync"

	"github.com/golang/glog"
	"github.com/prometheus/client_golang/prometheus"
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

// For my personal sanity.
const (
	Counter prometheus.ValueType = prometheus.CounterValue
	Gauge   prometheus.ValueType = prometheus.GaugeValue
)

func NewDesc(stem, help string, labels ...string) *prometheus.Desc {
	return prometheus.NewDesc("dsl320b_"+stem, help, labels, nil)
}

type Command struct {
	conn *Conn
	Cmd  string
	Ext  []Extractor
}

type Extractor struct {
	Identifier string
	Desc       *prometheus.Desc
	Type       prometheus.ValueType
	Labels     []string
}

// Purely for variadic syntax construction.
func NewExtractor(id string, desc *prometheus.Desc, typ prometheus.ValueType, labels ...string) Extractor {
	return Extractor{
		Identifier: id,
		Desc:       desc,
		Type:       typ,
		Labels:     labels,
	}
}

func (c *Command) Describe(ch chan<- *prometheus.Desc) {
	seen := make(map[*prometheus.Desc]bool)
	for _, ext := range c.Ext {
		if !seen[ext.Desc] {
			seen[ext.Desc] = true
			ch <- ext.Desc
		}
	}
}

func (c *Command) Collect(ch chan<- prometheus.Metric) {
	if err := c.conn.WriteLine(c.Cmd); err != nil {
		glog.Errorf("collect: write command %q failed: %v", c.Cmd, err)
		return
	}
	for _, ext := range c.Ext {
		metric, err := c.conn.ExtractNumber(ext)
		if err != nil {
			glog.Errorf("collect: extract number after %q failed: %v", ext.Identifier, err)
			continue
		}
		ch <- metric
	}
	if err := c.conn.SeekPrompt(); err != nil {
		glog.Errorf("collect: seek prompt failed: %v", err)
	}
}

// The NoiseMargin collector collects upstream noise margin and line attenuation stats.
func NoiseMargin(conn *Conn, dir string) *Command {
	cmd := "wan adsl l n"
	if dir == "upstream" {
		cmd = "wan adsl l f"
	}
	marginDesc := NewDesc("noise_margin_db", "SNR margin, in dB", "direction")
	attenDesc := NewDesc("line_attenuation_db", "Line attenuation, in dB", "direction")
	return &Command{
		conn: conn,
		Cmd:  cmd,
		Ext: []Extractor{
			NewExtractor("noise margin "+dir+": ", marginDesc, Gauge, dir),
			NewExtractor("attenuation "+dir+": ", attenDesc, Gauge, dir),
		},
	}
}

func SyncRate(conn *Conn) *Command {
	syncDesc := NewDesc("line_sync_rate_kbps", "Line sync rate, in kbps", "direction", "channel_type")
	return &Command{
		conn: conn,
		Cmd:  "wan adsl c",
		Ext: []Extractor{
			NewExtractor("near-end interleaved channel bit rate: ",
				syncDesc, Gauge, "downstream", "interleaved"),
			NewExtractor("near-end fast channel bit rate: ",
				syncDesc, Gauge, "downstream", "fast"),
			NewExtractor("far-end interleaved channel bit rate: ",
				syncDesc, Gauge, "upstream", "interleaved"),
			NewExtractor("far-end fast channel bit rate: ",
				syncDesc, Gauge, "upstream", "fast"),
		},
	}
}

func SysUptime(conn *Conn) *Command {
	uptimeDesc := NewDesc("system_uptime_ticks", "System uptime, in ticks (1/100 secs)")
	return &Command{
		conn: conn,
		Cmd:  "sys version",
		Ext: []Extractor{
			NewExtractor("system up time: ", uptimeDesc, Gauge),
		},
	}
}
