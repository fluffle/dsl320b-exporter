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
	conn    *Conn
	Cmd     string
	Metrics []Metric
}

type Metric struct {
	Ext    Extractor
	Desc   *prometheus.Desc
	Type   prometheus.ValueType
	Labels []string
}

// Purely for variadic syntax construction.
func NewMetric(ext Extractor, desc *prometheus.Desc, typ prometheus.ValueType, labels ...string) Metric {
	return Metric{
		Ext:    ext,
		Desc:   desc,
		Type:   typ,
		Labels: labels,
	}
}

func (m Metric) ReadMetric(r *Reader) (prometheus.Metric, error) {
	f, err := m.Ext.Extract(r)
	if err != nil {
		return nil, err
	}
	return prometheus.NewConstMetric(m.Desc, m.Type, f, m.Labels...)
}

func (c *Command) Describe(ch chan<- *prometheus.Desc) {
	seen := make(map[*prometheus.Desc]bool)
	for _, m := range c.Metrics {
		if !seen[m.Desc] {
			seen[m.Desc] = true
			ch <- m.Desc
		}
	}
}

func (c *Command) Collect(ch chan<- prometheus.Metric) {
	if err := c.conn.WriteLine(c.Cmd); err != nil {
		glog.Errorf("collect: write command %q failed: %v", c.Cmd, err)
		return
	}
	for _, m := range c.Metrics {
		metric, err := m.ReadMetric(c.conn.r)
		if err != nil {
			glog.Errorf("collect: extract %s failed: %v", m.Ext, err)
			continue
		}
		ch <- metric
	}
	if err := c.conn.SeekPrompt(); err != nil {
		glog.Errorf("collect: seek prompt failed: %v", err)
	}
}
