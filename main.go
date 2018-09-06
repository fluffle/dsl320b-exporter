package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/golang/glog"
	"github.com/reiver/go-telnet"
)

var (
	modemIP   = flag.String("modem_ip", "", "Internal IP of the modem.")
	modemPort = flag.Int("modem_port", 23, "Port to telnet to.")
	modemPass = flag.String("modem_pass", "", "Admin password for the modem.")
	modemName = flag.String("modem_hostname", "modem", "Hostname for modem.")
	diagFile  = flag.String("diag_file", "diag.txt", "Dump diag to this file.")
)

type Caller struct {
	Prompt string
	Cmds   []Command
	cmd    Command

	w telnet.Writer
	r *Reader
	s *bufio.Scanner
}

func (c *Caller) CallTELNET(ctx telnet.Context, w telnet.Writer, r telnet.Reader) {
	c.w = w
	c.r = NewReader(r)

	for _, cmd := range c.Cmds {
		if err := cmd.Run(c); err != nil {
			glog.Errorf("cmd: %v", err)
			break
		}
	}
}

func (c *Caller) WriteLine(s string, star ...byte) error {
	p := []byte(s + "\r\n")
	glog.V(2).Infof("write: %q", s)
	_, err := c.w.Write(p)
	if err != nil {
		return err
	}
	if len(star) > 0 {
		for i := range s {
			p[i] = star[0]
		}
	}
	// Read the bytes we wrote echoed back to us.
	return c.ExpectBytes(p)
}

func (c *Caller) SeekPrompt() error {
	return c.r.SeekPast(c.Prompt)
}

func (c *Caller) Expect(s string) error {
	return c.ExpectBytes([]byte(s))
}

func (c *Caller) ExpectBytes(want []byte) error {
	b, err := c.r.Peek(len(want))
	if err != nil {
		return err
	}
	if bytes.Equal(b, want) {
		glog.V(2).Infof("expect: %q", want)
		return c.r.Advance(len(want))
	}
	return fmt.Errorf("expected %q, got %q", want, b)
}

type Command interface {
	Run(c *Caller) error
}

type LoginCmd struct {
	Pass string
}

func (lc *LoginCmd) Run(c *Caller) error {
	if err := c.Expect("\r\nPassword: "); err != nil {
		return fmt.Errorf("expect password: %v", err)
	}
	if err := c.WriteLine(lc.Pass, '*'); err != nil {
		return fmt.Errorf("write password: %v", err)
	}
	if err := c.SeekPrompt(); err != nil {
		return fmt.Errorf("expect prompt: %v", err)
	}
	glog.Infoln("logged in")
	return nil
}

type DiagCmd struct {
	Out io.WriteCloser
}

func (dc *DiagCmd) Run(c *Caller) error {
	if err := c.WriteLine("wan adsl diag"); err != nil {
		return err
	}

	out, err := c.r.ReadPast("504~511\r")
	if err != nil {
		return err
	}
	if _, err = dc.Out.Write(out); err != nil {
		return err
	}
	if err = c.WriteLine(""); err != nil {
		return err
	}
	return c.SeekPrompt()
}

func main() {
	flag.Parse()
	if *modemIP == "" || *modemPass == "" {
		glog.Exit("--modem_ip and --modem_pass are both required.")
	}
	fh, err := os.OpenFile(*diagFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		glog.Exitf("Couldn't open diag file: %v")
	}

	caller := &Caller{
		Prompt: fmt.Sprintf("\r\n%s> ", *modemName),
		Cmds: []Command{
			&LoginCmd{Pass: *modemPass},
			&DiagCmd{Out: fh},
		},
	}
	hp := fmt.Sprintf("%s:%d", *modemIP, *modemPort)
	telnet.DialToAndCall(hp, caller)
	fh.Close()
}
