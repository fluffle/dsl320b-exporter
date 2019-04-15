package main

import (
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/golang/glog"
	"github.com/prometheus/client_golang/prometheus"
)

// For my personal sanity.
const (
	Counter prometheus.ValueType = prometheus.CounterValue
	Gauge   prometheus.ValueType = prometheus.GaugeValue
)

type latencyMeasure struct {
	sum, cnt, err float64
}

type latencyTracker struct {
	// Total* and Collect are called outside of the semaphore
	// that prevents concurrent collections, so we need a lock.
	mu                           *sync.Mutex
	totalSum, totalCnt, totalErr *prometheus.Desc
	cmdSum, cmdCnt, cmdErr       *prometheus.Desc
	total                        latencyMeasure
	perCmd                       map[string]latencyMeasure
}

func newLatencyTracker() latencyTracker {
	return latencyTracker{
		mu:       &sync.Mutex{},
		totalSum: NewDesc("collect_latency_seconds_overall_sum", "Overall count of seconds spent collecting."),
		totalCnt: NewDesc("collect_latency_seconds_total_count", "Overall count of collections."),
		totalErr: NewDesc("collect_error_count", "Overall count of collection errors."),
		cmdSum:   NewDesc("command_collect_latency_seconds_overall_sum", "Count of seconds spent collecting per-command.", "command"),
		cmdCnt:   NewDesc("command_collect_latency_seconds_total_count", "Count of collections per-command", "command"),
		cmdErr:   NewDesc("command_collect_error_count", "Count of collection errors per-command", "command"),
		perCmd:   make(map[string]latencyMeasure),
	}
}

func (lt latencyTracker) Total(secs float64) {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	lt.total.sum += secs
	lt.total.cnt += 1
}

func (lt latencyTracker) TotalErr() {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	lt.total.err += 1
}

func (lt latencyTracker) Cmd(cmd string, secs float64) {
	m := lt.perCmd[cmd]
	m.sum += secs
	m.cnt += 1
	lt.perCmd[cmd] = m
}

func (lt latencyTracker) CmdErr(cmd string) {
	m := lt.perCmd[cmd]
	m.err += 1
	lt.perCmd[cmd] = m
}

func (lt latencyTracker) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{
		lt.totalSum, lt.totalCnt, lt.totalErr, lt.cmdSum, lt.cmdCnt, lt.cmdErr,
	} {
		ch <- d
	}
}

func (lt latencyTracker) Collect(ch chan<- prometheus.Metric) {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	ch <- prometheus.MustNewConstMetric(lt.totalSum, Counter, lt.total.sum)
	ch <- prometheus.MustNewConstMetric(lt.totalCnt, Counter, lt.total.cnt)
	ch <- prometheus.MustNewConstMetric(lt.totalErr, Counter, lt.total.err)
	for cmd, m := range lt.perCmd {
		ch <- prometheus.MustNewConstMetric(lt.cmdSum, Counter, m.sum, cmd)
		ch <- prometheus.MustNewConstMetric(lt.cmdCnt, Counter, m.cnt, cmd)
		ch <- prometheus.MustNewConstMetric(lt.cmdErr, Counter, m.err, cmd)
	}
}

// The Prometheus client library performs collection concurrently.
// This won't play well with our telnet interface, so we register
// a single Aggregator collector which serializes collection.
type Aggregator struct {
	conn *Conn
	c    []Collector
	sem  chan struct{}
	lt   latencyTracker
}

func NewAggregator(conn *Conn, coll ...Collector) *Aggregator {
	return &Aggregator{
		conn: conn,
		c:    coll,
		sem:  make(chan struct{}, 1),
		lt:   newLatencyTracker(),
	}
}

func (agg *Aggregator) Describe(ch chan<- *prometheus.Desc) {
	for _, coll := range agg.c {
		coll.Describe(ch)
	}
	agg.lt.Describe(ch)
}

func (agg *Aggregator) Collect(ch chan<- prometheus.Metric) {
	totalStart := time.Now()
	defer func() {
		agg.lt.Total(time.Since(totalStart).Seconds())
		agg.lt.Collect(ch)
	}()

	if agg.conn.State() != CONNECTED {
		glog.Warningln("agg: rejecting collect request while disconnected")
		agg.lt.TotalErr()
		return
	}
	select {
	case agg.sem <- struct{}{}:
		agg.collect(ch)
		<-agg.sem
	default:
		glog.Warningln("agg: rejecting collect request because one is already in-flight")
		agg.lt.TotalErr()
	}
}

func (agg *Aggregator) collect(ch chan<- prometheus.Metric) {
	for _, coll := range agg.c {
		cmdStart := time.Now()
		err := coll.Collect(ch)
		if err == nil {
			if err = agg.conn.SeekPrompt(); err != nil {
				err = fmt.Errorf("command %q seek prompt failed: %v", coll, err)
			}
		}
		if err != nil {
			// If we got an error from collection, or can't find the prompt
			// after a collection completes, then the stream has become
			// desynchronized from where the code expects it to be. To
			// resynchronize, we wait for the modem to stop sending us data,
			// then drop the contents of the buffer on the floor. The next
			// collector then starts with a clean slate.
			glog.Errorf("collect: %v", err)
			agg.lt.CmdErr(coll.String())
			// Sometimes, it looks like the modem simply doesn't flush some
			// of the data we're waiting for to us until we send some more
			// data. The pathology looks like:
			//   - Command a times out while waiting for expected data.
			//   - Drain dumps a few (e.g. 8) bytes that are not expected.
			//   - Writing the next command fails because instead of e.g.
			//     "wan hwsar disp\r\n" being echoed back to us, we get
			//     "wmodem> an hwsar disp\r\n".
			// So, we send \r\n before draining the buffer.
			agg.conn.WriteLine("")
			agg.conn.r.Drain(true)
		}
		agg.lt.Cmd(coll.String(), time.Since(cmdStart).Seconds())
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

	metrics := make([]Metric, 0, 14)
	dirMap := map[string]string{
		"downstream": "near-end",
		"upstream":   "far-end",
	}

	for _, dir := range []string{"downstream", "upstream"} {
		for _, errt := range []string{"FEC", "CRC", "HEC"} {
			for _, cht := range []string{"fast", "interleaved"} {
				identifier := fmt.Sprintf("%s %s error %s: ", dirMap[dir], errt, cht)
				metrics = append(metrics,
					NewMetric(FloatAfter(identifier), errorDesc, Counter, dir, cht, errt))
			}
		}
	}

	return &Command{
		conn: conn,
		Cmd:  "wan adsl perfdata",
		Metrics: append(metrics,
			NewMetric(FloatAfter("Error second after power-up\t: "),
				errSecDesc, Counter),
			NewMetric(ADSLUptime{}, adslUpDesc, Gauge)),
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
	atmErrsDesc := NewDesc("adsl_atm_error_count", "The number of ATM errors", "error_type")

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
			NewMetric(HexAfter("inCrc10Err     = "),
				atmErrsDesc, Counter, "hec_crc10"),
			NewMetric(HexAfter("inMpoaErr      = "),
				atmErrsDesc, Counter, "mpoa"),
		},
	}
}

func MBufStats(conn *Conn) *Command {
	allocDesc := NewDesc("mbuf_alloc_count", "The number of allocations from the memory buffer pool.", "pool", "type")
	failDesc := NewDesc("mbuf_alloc_fail_count", "The number of failed allocations from the memory buffer pool.", "pool", "type")
	freeDesc := NewDesc("mbuf_free_count", "The number of frees from the memory buffer pool.", "pool", "type")
	availDesc := NewDesc("mbuf_blocks_avail", "The number of buffer blocks available in the pool.", "pool", "type")
	totalDesc := NewDesc("mbuf_blocks_total", "The total number of buffer blocks in the pool.", "pool", "type")
	blockSizeDesc := NewDesc("mbuf_block_size_bytes", "The size, in bytes, of each buffer block in the pool.", "pool", "type")
	dataSizeDesc := NewDesc("mbuf_pool_data_size_bytes", "The size, in bytes, of the buffer pool data area.", "pool", "type")
	hdrSizeDesc := NewDesc("mbuf_pool_header_size_bytes", "The size, in bytes, of the buffer pool data area.", "pool", "type")

	metrics := make([]Metric, 0, 8*2*3) // 8 descs * 2 pools * 3 types

	// The hoops we jump through to avoid having to write a custom Extractor.
	// TODO(fluffle): this is horribly fragile.
	typeSize := map[int]string{
		0: "size=(80/",
		1: "size=(200/",
		2: "size=(640/",
	}

	// This is horribly dependent on the ordering of the output never changing.
	// The alternative is much more stateful parsing of the output, which is
	// way less convenient given the current Extractor interface.
	for pool := 0; pool < 2; pool++ {
		pstr := strconv.Itoa(pool)
		for typ := 0; typ < 3; typ++ {
			tstr := strconv.Itoa(typ)
			metrics = append(metrics,
				NewMetric(HexAfter(typeSize[typ]), blockSizeDesc, Gauge, pstr, tstr),
				NewMetric(HexAfter("num="), totalDesc, Gauge, pstr, tstr),
				NewMetric(HexAfter("alloc="), allocDesc, Counter, pstr, tstr),
				NewMetric(HexAfter("fail="), failDesc, Counter, pstr, tstr),
				NewMetric(HexAfter("free="), freeDesc, Counter, pstr, tstr),
				NewMetric(MBufSize("d"), dataSizeDesc, Gauge, pstr, tstr),
				NewMetric(MBufSize("h"), hdrSizeDesc, Gauge, pstr, tstr),
				NewMetric(HexAfter("cm:"), availDesc, Gauge, pstr, tstr),
			)
		}
	}

	return &Command{
		conn:    conn,
		Cmd:     "sys mbuf status",
		Metrics: metrics,
	}
}

func HeapStats(conn *Conn) *Command {
	heapSizeDesc := NewDesc("heap_size_bytes", "The total heap size, in bytes.")
	heapUsedDesc := NewDesc("heap_used_bytes", "The amount of heap used, in bytes.")
	heapMaxSizeDesc := NewDesc("heap_max_contiguous_bytes", "The maximum available contiguous heap free space.")
	heapAllocDesc := NewDesc("heap_alloc_count", "The count of allocations from the heap.")
	heapFreeDesc := NewDesc("heap_free_count", "The count of freed allocations from the heap.")

	return &Command{
		conn: conn,
		Cmd:  "sys mbufc disp",
		Metrics: []Metric{
			NewMetric(FloatAfter("heap size: "), heapSizeDesc, Gauge),
			NewMetric(FloatAfter("Heap usage: "), heapUsedDesc, Gauge),
			NewMetric(FloatAfter(" size: "), heapMaxSizeDesc, Gauge),
			NewMetric(FloatAfter("alloc count: "), heapAllocDesc, Counter),
			NewMetric(FloatAfter("free count: "), heapFreeDesc, Counter),
		},
	}
}

func pad(dir string) string {
	b := make([]byte, 24)
	for i := range b {
		b[i] = ' '
	}
	copy(b, dir)
	b[22] = '='
	return string(b)
}

func EthCounters(conn *Conn) *Command {
	bytesDesc := NewDesc("ethernet_byte_count", "The number of bytes recvd/xmitd on the ethernet interface.", "direction")
	packetsDesc := NewDesc("ethernet_packet_count", "The number of packets recvd/xmitd on the ethernet interface.", "direction")
	errsDesc := NewDesc("ethernet_error_count", "The number of errors recvd/xmitd on the ethernet interface.", "direction")

	metrics := make([]Metric, 0, 6)

	for _, dir := range []string{"in", "out"} {
		metrics = append(metrics,
			NewMetric(HexAfter(pad(dir+"Octets")), bytesDesc, Counter, dir),
			NewMetric(HexAfter(pad(dir+"UnicastPkts")), packetsDesc, Counter, dir),
			NewMetric(HexAfter(pad(dir+"Errors")), errsDesc, Counter, dir),
		)
	}

	return &Command{
		conn:    conn,
		Cmd:     "ether driver cnt disp enet0",
		Metrics: metrics,
	}
}

func CPUStats(conn *Conn) *Command {
	// It would be nice to be able to export two tick counters,
	// but that would mean maintaining state across scrapes.
	usageDesc := NewDesc("cpu_utilization", "The fractional cpu utilization, averaged over the past 63 seconds.")

	return &Command{
		conn: conn,
		Cmd:  "sys cpu disp",
		Metrics: []Metric{
			NewMetric(CPUUsage{}, usageDesc, Gauge),
		},
	}
}
