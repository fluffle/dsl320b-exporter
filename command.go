package main

import (
	"fmt"

	"github.com/golang/glog"
	"github.com/prometheus/client_golang/prometheus"
)

// NewDesc is a helper function to create a prometheus metric descriptor.
// It enforces that all metrics begin with the exporter name "dsl320b".
func NewDesc(stem, help string, labels ...string) *prometheus.Desc {
	return prometheus.NewDesc("dsl320b_"+stem, help, labels, nil)
}

// A Metric ties an Extractor to the metadata required to turn the extracted
// float into a prometheus Metric.
type Metric struct {
	Ext    Extractor
	Desc   *prometheus.Desc
	Type   prometheus.ValueType
	Labels []string
}

// NewMetric makes creating new Metrics nicer, thanks to variadic labels.
func NewMetric(ext Extractor, desc *prometheus.Desc, typ prometheus.ValueType, labels ...string) Metric {
	return Metric{
		Ext:    ext,
		Desc:   desc,
		Type:   typ,
		Labels: labels,
	}
}

// ReadMetric extracts a float from the Reader and combines it with Metric
// metadata to create a prometheus Metric ready for collection.
func (m Metric) ReadMetric(r *Reader) (prometheus.Metric, error) {
	f, labels, err := m.Ext.Extract(r)
	if err != nil {
		return nil, err
	}
	glog.V(2).Infof("extract %s = %g %v", m.Ext, f, labels)
	return prometheus.NewConstMetric(m.Desc, m.Type, f, append(m.Labels, labels...)...)
}

// We want to propagate errors back up from our collectors.
type Collector interface {
	fmt.Stringer
	Describe(chan<- *prometheus.Desc)
	Collect(chan<- prometheus.Metric) error
}

// A Command executes a single command via the telnet connection, then collects
// each of its Metrics in order. If the order of Metrics and their Extractors
// doesn't match the output of the command, you're gonna have a bad time!
type Command struct {
	conn    *Conn
	Cmd     string
	WhenUp  bool // Command returns "adsl modem not up" when ADSL is down.
	Metrics []Metric
}

// Describe writes prometheus descriptors to the provided channel.
func (c *Command) Describe(ch chan<- *prometheus.Desc) {
	seen := make(map[*prometheus.Desc]bool)
	for _, m := range c.Metrics {
		if !seen[m.Desc] {
			seen[m.Desc] = true
			ch <- m.Desc
		}
	}
}

// Collect executes the command and reads metric values from the response.
func (c *Command) Collect(ch chan<- prometheus.Metric) error {
	if err := c.conn.WriteLine(c.Cmd); err != nil {
		return fmt.Errorf("command %q write line failed: %v", c, err)
	}
	if c.WhenUp {
		if err := c.conn.Expect("adsl modem not up"); err == nil {
			// ADSL is down, this command will not produce data.
			return nil
		}
	}
	for _, m := range c.Metrics {
		metric, err := m.ReadMetric(c.conn.r)
		if err != nil {
			// If we're in a bad position for one of the metrics, trying to seek
			// forward to another will probably not go well, so bail out now.
			return fmt.Errorf("command %q extract %s failed: %v", c, m.Ext, err)
		}
		ch <- metric
	}
	return nil
}

func (c *Command) String() string {
	return c.Cmd
}
