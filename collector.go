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

func (agg *Aggregator) Add(coll prometheus.Collector) {
	agg.mu.Lock()
	defer agg.mu.Unlock()
	agg.c = append(agg.c, coll)
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

func makeDesc(stem, help string, labels ...string) *prometheus.Desc {
	return prometheus.NewDesc("dsl320b_"+stem, help, labels, nil)
}

type noiseMargin struct {
	conn *Conn

	// This collector collects noise margin and line attenuation stats.
	margin, atten *prometheus.Desc
}

func NoiseMargin(conn *Conn) *noiseMargin {
	return &noiseMargin{
		conn:   conn,
		margin: makeDesc("noise_margin_db", "SNR margin, in dB", "direction"),
		atten:  makeDesc("line_attenuation_db", "Line attenuation, in dB", "direction"),
	}
}

func (nm *noiseMargin) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{nm.margin, nm.atten} {
		ch <- d
	}
}

func (nm *noiseMargin) Collect(ch chan<- prometheus.Metric) {
	stats := make(map[string]float64)
	recordStat := func(k string) error {
		f, err := nm.conn.r.Float64()
		stats[k] = f
		return err
	}

	convo := []struct {
		f func(string) error
		c string
	}{
		{nm.conn.WriteLine, "wan adsl l n"},
		{nm.conn.r.SeekPast, "noise margin downstream: "},
		{recordStat, "downMargin"},
		{nm.conn.r.SeekPast, "attenuation downstream: "},
		{recordStat, "downAtten"},
		{nm.conn.r.SeekPast, nm.conn.Prompt},
		{nm.conn.WriteLine, "wan adsl l f"},
		{nm.conn.r.SeekPast, "noise margin upstream: "},
		{recordStat, "upMargin"},
		{nm.conn.r.SeekPast, "attenuation upstream: "},
		{recordStat, "upAtten"},
		{nm.conn.r.SeekPast, nm.conn.Prompt},
	}

	failed := false
	for _, s := range convo {
		if err := s.f(s.c); err != nil {
			glog.Errorf("noise margin: c=%q err=%v", s.c, err)
			failed = true
			break
		}
	}
	glog.Infoln("noise margin:", stats)
	if !failed {
		ch <- prometheus.MustNewConstMetric(
			nm.margin,
			prometheus.GaugeValue,
			stats["downMargin"],
			"downstream")
		ch <- prometheus.MustNewConstMetric(
			nm.margin,
			prometheus.GaugeValue,
			stats["upMargin"],
			"upstream")
		ch <- prometheus.MustNewConstMetric(
			nm.atten,
			prometheus.GaugeValue,
			stats["downAtten"],
			"downstream")
		ch <- prometheus.MustNewConstMetric(
			nm.atten,
			prometheus.GaugeValue,
			stats["upAtten"],
			"upstream")
	}
}