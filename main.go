package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/cenkalti/backoff"
	"github.com/golang/glog"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/reiver/go-telnet"
)

var (
	hostPort  = flag.String("host_port", ":9489", "Port to serve metrics on.")
	modemIP   = flag.String("modem_ip", "", "Internal IP of the modem.")
	modemPort = flag.Int("modem_port", 23, "Port to telnet to.")
	modemPass = flag.String("modem_pass", "", "Admin password for the modem.")
	modemName = flag.String("modem_hostname", "tc", "Hostname for modem.")
	diagFile  = flag.String("diag_file", "", "Dump diag to this file.")

	// An invalid password is misconfiguration that should prevent the
	// reconnection loop from running.
	errInvalidPassword = errors.New("invalid password")
	// Writes will fail with this error when we're not connected.
	errNotConnected = errors.New("not connected")
)

type Conn struct {
	Prompt, Pass, Addr string
	Server             *http.Server

	// mu is used to prevent concurrent calls to writeLine.
	// Because writeLine needs to read back the data it's written,
	// concurrent calls can end up racing between the write and
	// its subsequent read. It also prevents concurrent access
	// to state.
	mu    sync.Mutex
	w     *telnet.Conn
	r     *Reader
	state ConnState

	// sigch is the channel we pass to signal.Notify
	sigch chan os.Signal
	// up is used to gate writes until we're connected
	up chan bool

	// sdMu protects shutdown which is set by Exit.
	sdMu     sync.Mutex
	shutdown bool
}

func NewConn(server *http.Server, addr, pass, prompt string) *Conn {
	c := &Conn{
		Addr:   addr,
		Pass:   pass,
		Prompt: prompt,
		Server: server,
		sigch:  make(chan os.Signal, 1),
		up:     make(chan bool),
		r:      NewReader(),
	}

	signal.Notify(c.sigch, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGHUP)
	c.state = CONNECTING
	return c
}

// Method states are really nice implementation-wise until
// you want to compare them, then it all goes to shit.
// I'll just use a state map like a pleb, then.
type ConnState int

const (
	// SHUTDOWN is first so that reads from
	// a closed chan ConnState cause shutdown.
	SHUTDOWN ConnState = iota
	CONNECTING
	CONNECTED
	DISCONNECTING
)

func (c *Conn) State() ConnState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

func (c *Conn) Transition(state ConnState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = state
}

func (c *Conn) ConnectLoop(ctx context.Context, cancel context.CancelFunc) {
	glog.Infof("ConnectLoop starting.")
	transitions := map[ConnState]func(context.Context) ConnState{
		CONNECTING:    c.Connecting,
		CONNECTED:     c.Connected,
		DISCONNECTING: c.Disconnecting,
	}

	for {
		to, ok := transitions[c.State()]
		if !ok {
			// Handles SHUTDOWN as well as any unknown states.
			break
		}
		c.Transition(to(ctx))
	}
	glog.Infoln("Shutting down.")
	c.Server.Shutdown(ctx)
	signal.Stop(c.sigch)
	cancel()
	glog.Infof("ConnectLoop ending.")
}

func (c *Conn) Connecting(ctx context.Context) ConnState {
	glog.Infof("Connecting to %s.", c.Addr)
	ctx, cancel := context.WithCancel(ctx)

	ch := make(chan ConnState)
	defer close(ch)

	go func() {
		b := backoff.WithContext(&backoff.ExponentialBackOff{
			InitialInterval:     time.Second,
			RandomizationFactor: 0.1,
			Multiplier:          2,
			MaxInterval:         5 * time.Minute,
			MaxElapsedTime:      0,
			Clock:               backoff.SystemClock,
		}, ctx)
		b.Reset()
		if err := backoff.Retry(c.dial, b); err != nil {
			glog.Errorf("Permanent connection error: %v", err)
			ch <- SHUTDOWN
			return
		}
		ch <- CONNECTED
	}()
	for {
		select {
		case state := <-ch:
			return state
		case sig := <-c.sigch:
			// This should result in the backoff failing with a
			// permanent connection error, so loop back and cleanup.
			glog.Errorf("Received %s while connecting, canceling.", sig)
			cancel()
		}
	}
}

func (c *Conn) dial() error {
	conn, err := telnet.DialTo(c.Addr)
	if err != nil {
		return err
	}
	c.w = conn
	return nil
}

func (c *Conn) Connected(ctx context.Context) ConnState {
	glog.Infof("Connected to %s.", c.Addr)
	ctx, cancel := context.WithCancel(ctx)
	c.r.Use(c.w, cancel)
	// We defer closing the underlying telnet connection here, which will
	// implicitly cause cancel to be called in Reader.fill, so we don't
	// need to also explicitly defer cancel here.
	defer c.w.Close()

	ch := make(chan ConnState)
	defer close(ch)

	go func() {
		err := c.Login()
		switch err {
		case nil:
			// Connected and logged in happily!
			glog.Infof("Successfully logged into %s.", c.Addr)
			c.up <- true
		case errInvalidPassword:
			// Invalid password is not retryable.
			c.sdMu.Lock()
			c.shutdown = true
			c.sdMu.Unlock()
			fallthrough
		default:
			// Tear it all down and (maybe) try again.
			glog.Errorf("Error logging in: %v", err)
			ch <- DISCONNECTING
		}
		close(c.up)
	}()

	// Calling Exit should cause the reader to get an EOF so we loop here to
	// clean up. But we might have got to a bad state where we're still
	// connected but at the password prompt, where sending "exit" won't work.
	// If we receive > 1 signal while connected, we assume "exit" hasn't
	// caused the reader to receive EOF, and forcibly disconnect instead.
	for i := 0; i < 2; i++ {
		select {
		case state := <-ch:
			// Something bad happened during login /o\
			return state
		case <-ctx.Done():
			// Reader has called cancel, so it's hit EOF.
			return DISCONNECTING
		case sig := <-c.sigch:
			glog.Errorf("Received %s while connected, exiting.", sig)
			// SIGHUP forces a disconnect/reconnect without shutting down.
			c.Exit(sig != syscall.SIGHUP)
		}
	}
	glog.Error("Received too many signals, forcibly disconnecting.")
	// c.shutdown will have been set by Exit() to the correct value already.
	return DISCONNECTING
}

func (c *Conn) AwaitConnection() bool {
	// Reads of a closed bool channel return false, conveniently.
	return <-c.up
}

func (c *Conn) Disconnecting(ctx context.Context) ConnState {
	// NOTE: no signal handling in this function!
	glog.Infof("Disconnecting from %s.", c.Addr)
	// Drain anything from c.up before resetting the channel, just
	// in case the goroutine from Connected is live and sat waiting to send.
	// We need it to have closed c.up so that any awaitors get notified.
	for <-c.up {
	}

	// Recreate up channel so new awaitors will wait for a reconnection.
	c.up = make(chan bool)

	// Clean out connection state.
	c.r.Reset()
	c.w = nil

	c.sdMu.Lock()
	defer c.sdMu.Unlock()
	if c.shutdown {
		return SHUTDOWN
	}
	return CONNECTING
}

func (c *Conn) writeLine(s string, star ...byte) error {
	if c.State() != CONNECTED {
		// Drop writes on the floor unless we're connected.
		return errNotConnected
	}

	// writeLine can't be called concurrently because that causes
	// races between the write and subsequent read.
	c.mu.Lock()
	defer c.mu.Unlock()
	p := []byte(s + "\r\n")
	_, err := c.w.Write(p)
	if err != nil {
		return err
	}
	if len(star) > 0 {
		for i := range s {
			p[i] = star[0]
		}
	}
	glog.V(2).Infof("write: %q", p)
	// Read the bytes we wrote echoed back to us.
	return c.r.ExpectBytes(p)
}

func (c *Conn) WriteLine(s string) error {
	return c.writeLine(s)
}

func (c *Conn) WritePass() error {
	return c.writeLine(c.Pass, '*')
}

func (c *Conn) SeekPrompt() error {
	return c.r.SeekPast(c.Prompt)
}

func (c *Conn) Expect(s string) error {
	return c.r.ExpectBytes([]byte(s))
}

func (c *Conn) Login() error {
	if err := c.Expect("\r\nPassword: "); err != nil {
		return fmt.Errorf("expect password: %v", err)
	}
	if err := c.WritePass(); err != nil {
		return fmt.Errorf("write password: %v", err)
	}
	// A second password prompt indicates login failure.
	// Expect will rewind the input if it doesn't read these bytes.
	// If the prompt is set to a string beginning with a capital P
	// this will not work, but it's an easy, lazy check.
	// Attempting to read a whole password prompt will cause the
	// reader to block forever if the modem's command prompt is
	// shorter, which it is by default.
	if err := c.Expect("\r\nP"); err == nil {
		return errInvalidPassword
	}
	if err := c.SeekPrompt(); err != nil {
		return fmt.Errorf("expect prompt: %v", err)
	}
	glog.Infoln("logged in")
	return nil
}

func (c *Conn) Exit(shutdown bool) {
	glog.Infoln("Sending exit command.")
	c.sdMu.Lock()
	defer c.sdMu.Unlock()
	if c.shutdown {
		return
	}
	c.shutdown = shutdown
	switch c.State() {
	case CONNECTED:
		// The state machine code is written such that an EOF from the reader
		// triggers everything to shut down nicely.
		if err := c.WriteLine("exit"); err != nil {
			glog.Errorf("exit: %v", err)
		}
	case CONNECTING:
		// Pretend we received a sigint. This will safely cancel any ongoing
		// connection attempts withoout needing to store the CancelFunc.
		c.sigch <- os.Interrupt
	}
}

func (c *Conn) DumpDiags(diagFile string) error {
	w, err := os.OpenFile(diagFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		glog.Exitf("Couldn't open diag file: %v", err)
	}
	defer w.Close()
	if err := c.WriteLine("wan adsl diag"); err != nil {
		return err
	}
	out, err := c.r.ReadPast("504~511\r\r\n")
	if err != nil {
		return err
	}
	if _, err = w.Write(out); err != nil {
		return err
	}
	if err = c.WriteLine("sys diag"); err != nil {
		return err
	}
	out, err = c.r.ReadPast(c.Prompt)
	if err != nil {
		return err
	}
	if _, err = w.Write(out); err != nil {
		return err
	}
	return nil
}

func main() {
	flag.Parse()
	if *modemIP == "" {
		glog.Exit("--modem_ip is a required flag.")
	}
	pass := *modemPass
	if pass == "" {
		pass = os.Getenv("MODEM_PASS")
		if pass == "" {
			glog.Exit("You must provide the admin password via --modem_pass or MODEM_PASS env var.")
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	conn := NewConn(
		&http.Server{Addr: *hostPort},
		fmt.Sprintf("%s:%d", *modemIP, *modemPort),
		pass,
		fmt.Sprintf("\r\n%s> ", *modemName))

	go conn.ConnectLoop(ctx, cancel)

	if *diagFile != "" {
		if ok := conn.AwaitConnection(); !ok {
			glog.Exit("Something went wrong with initial connection.")
		}
		err := conn.DumpDiags(*diagFile)
		if err != nil {
			glog.Exitf("Dumping diagnostics failed: %v", err)

		}
		conn.Exit(true)
		<-ctx.Done()
		glog.Exitf("Dumped diagnostics to %q", *diagFile)
	}

	agg := NewAggregator(conn,
		SysUptime(conn),
		CPUStats(conn),
		HeapStats(conn),
		MBufStats(conn),
		ADSLStatus(conn),
		ADSLMode(conn),
		ADSLErrors(conn),
		NoiseMargin(conn, false),
		NoiseMargin(conn, true),
		SyncRate(conn),
		ATMCells(conn),
		SARCounters(conn),
		EthCounters(conn),
	)
	prometheus.MustRegister(agg)

	http.Handle("/metrics", promhttp.Handler())
	http.HandleFunc("/quitquitquit", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "Shut down!")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		conn.Exit(true)
	})
	go conn.Server.ListenAndServe()
	<-ctx.Done()
	glog.Exitln("Shut down, bye!")
}
