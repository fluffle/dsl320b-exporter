package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/golang/glog"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/reiver/go-telnet"
)

var (
	port      = flag.Int("port", 9489, "Port to serve metrics on.")
	modemIP   = flag.String("modem_ip", "", "Internal IP of the modem.")
	modemPort = flag.Int("modem_port", 23, "Port to telnet to.")
	modemPass = flag.String("modem_pass", "", "Admin password for the modem.")
	modemName = flag.String("modem_hostname", "modem", "Hostname for modem.")
	diagFile  = flag.String("diag_file", "", "Dump diag to this file.")
)

type Conn struct {
	Prompt, Pass string
	Server       *http.Server

	w        *telnet.Conn
	r        *Reader
	mu       sync.Mutex
	shutdown bool
	sigch    chan os.Signal
}

func (c *Conn) Dial(hostport string) error {
	conn, err := telnet.DialTo(hostport)
	if err != nil {
		return err
	}
	c.w = conn
	c.r = NewReader(conn)
	return c.Login()
}

func (c *Conn) writeLine(s string, star ...byte) error {
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
		return fmt.Errorf("invalid password")
	}
	if err := c.SeekPrompt(); err != nil {
		return fmt.Errorf("expect prompt: %v", err)
	}
	glog.Infoln("logged in")
	return nil
}

func (c *Conn) SigHandler() {
	c.sigch = make(chan os.Signal, 1)
	signal.Notify(c.sigch, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	for {
		_, ok := <-c.sigch
		if !ok {
			break
		}
		c.Exit()
	}
}

func (c *Conn) Exit() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.shutdown {
		return
	}
	c.shutdown = true
	signal.Stop(c.sigch)
	close(c.sigch)
	if err := c.WriteLine("exit"); err != nil {
		glog.Errorf("exit: %v", err)
	}
	c.w.Close()
	c.Server.Shutdown(context.Background())
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
	out, err = c.r.ReadTo(c.Prompt)
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
	if *modemIP == "" || *modemPass == "" {
		glog.Exit("--modem_ip and --modem_pass are both required.")
	}

	conn := &Conn{
		Prompt: fmt.Sprintf("\r\n%s> ", *modemName),
		Pass:   *modemPass,
		Server: &http.Server{Addr: fmt.Sprintf(":%d", *port)},
	}

	if err := conn.Dial(fmt.Sprintf("%s:%d", *modemIP, *modemPort)); err != nil {
		glog.Exitf("Connection failed: %v", err)
	}

	if *diagFile != "" {
		err := conn.DumpDiags(*diagFile)
		conn.Exit()
		if err != nil {
			glog.Exitf("Dumping diagnostics failed: %v", err)
		}
		glog.Exitf("Dumped diagnostics to %q", *diagFile)
	}

	agg := NewAggregator(
		SysUptime(conn),
		ADSLStatus(conn),
		ADSLMode(conn),
		ADSLErrors(conn),
		NoiseMargin(conn, false),
		NoiseMargin(conn, true),
		SyncRate(conn),
		ATMCells(conn),
		SARCounters(conn),
	)
	prometheus.MustRegister(agg)

	http.Handle("/metrics", promhttp.Handler())
	http.HandleFunc("/quitquitquit", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "Shut down!")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		go conn.Exit()
	})
	go conn.SigHandler()
	glog.Exitf(conn.Server.ListenAndServe().Error())
}
