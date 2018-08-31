package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/glycerine/rbuf"
	"github.com/golang/glog"
	"github.com/reiver/go-telnet"
)

var (
	modemIP   = flag.String("modem_ip", "", "Internal IP of the modem.")
	modemPort = flag.Int("modem_port", 23, "Port to telnet to.")
	modemPass = flag.String("modem_pass", "", "Admin password for the modem.")
	modemName = flag.String("modem_hostname", "modem", "Hostname for modem.")
	diagFile  = flag.String("diag_file", "diag.txt", "Dump diag to this file.")
	deadline  = flag.Duration("read_deadline", time.Second, "Time out expects after...")
)

type Caller struct {
	Cmds []Command
	cmd  Command

	w        telnet.Writer
	buf      *rbuf.FixedSizeRingBuf
	bufMu    sync.RWMutex
	Deadline time.Duration
	Prompt   string
}

func (c *Caller) CallTELNET(ctx telnet.Context, w telnet.Writer, r telnet.Reader) {
	c.w = w
	c.buf = rbuf.NewFixedSizeRingBuf(128 * 1024)
	go c.recv(r)

	for _, cmd := range c.Cmds {
		if err := cmd.Run(c); err != nil {
			glog.Errorf("cmd: %v", err)
			break
		}
	}
}

func (c *Caller) recv(r telnet.Reader) {
	p := make([]byte, 1)
	for {
		_, err := r.Read(p)
		if err != nil {
			if err != io.EOF {
				glog.Errorf("recv read: %v", err)
			}
			return
		}
		c.bufMu.Lock()
		if _, err = c.buf.Write(p); err != nil {
			glog.Errorf("recv write: %v", err)
		}
		c.bufMu.Unlock()
	}
}

func (c *Caller) Read(p []byte) (int, error) {
	c.bufMu.RLock()
	defer c.bufMu.RUnlock()
	return c.buf.Read(p)
}

func (c *Caller) Peek(p []byte) (int, error) {
	c.bufMu.RLock()
	defer c.bufMu.RUnlock()
	return c.buf.ReadWithoutAdvance(p)
}

func (c *Caller) ReadByte() (byte, error) {
	p := make([]byte, 1)
	_, err := c.Read(p)
	return p[0], err
}

func (c *Caller) PeekByte() (byte, error) {
	p := make([]byte, 1)
	_, err := c.Peek(p)
	return p[0], err
}

func (c *Caller) WriteLine(s string, star ...byte) error {
	p := []byte(s + "\r\n")
	glog.V(2).Infoln("write:", s)
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

func (c *Caller) Expect(s string) error {
	return c.ExpectBytes([]byte(s))
}

func (c *Caller) ExpectBytes(b []byte) error {
	p := make([]byte, len(b))
	t := time.After(c.Deadline)
	for {
		select {
		case <-t:
			return fmt.Errorf("expect: timed out after %s", c.Deadline)
		default:
			n, err := c.Peek(p)
			switch {
			case err != nil && err != io.EOF:
				return err
			case bytes.Equal(p, b):
				// Consume expected input from buffer before returning.
				c.Read(p)
				return nil
			case n == len(b):
				return fmt.Errorf("expected %q, got %q", b, p)
			default:
				// Buffer doesn't have enough data in it yet.
				time.Sleep(time.Millisecond)
			}
		}
	}
}

func (c *Caller) ExpectPrompt() error {
	return c.Expect("\r\n" + c.Prompt)
}

func (c *Caller) SeekBytes(b []byte) error {
	// Seek ~4-5x len(b) at a time, in multiples of 256 bytes
	size := 256 * (64 + len(b)) / 64
	buf := make([]byte, size)
	// Advance less than a full buffer so b is not over boundary
	advance := size - (len(b) - 1)

	t := time.After(c.Deadline)
	for {
		select {
		case <-t:
			return fmt.Errorf("seek: timed out after %s", c.Deadline)
		default:
			n, err := c.Peek(buf)
			if err != nil && err != io.EOF {
				return err
			}
			if n == 0 {
				time.Sleep(time.Millisecond)
				continue
			}
			switch idx := bytes.Index(buf[:n], b); idx {
			case -1:
				// Not found, advance!
				c.Read(buf[:advance])
			case 0:
				// Found at start of buffer.
				return nil
			default:
				// Found somewhere in the buffer.
				_, err = c.Read(buf[:idx])
				return err
			}
		}
	}
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
	if err := c.ExpectPrompt(); err != nil {
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

	// Avoid the prompt falling on a read boundary.
	const size = 256
	buf := [size]byte{}
	spare := len("504~511") - 1
	seenPrompt := false
	for {
		n, err := c.Read(buf[:size-spare])
		if err == io.EOF && seenPrompt {
			return nil
		}
		if err != nil && err != io.EOF {
			return err
		}
		if n == 0 {
			time.Sleep(time.Millisecond)
			continue
		}
		if _, err = dc.Out.Write(buf[:n]); err != nil {
			return err
		}
		_, err = c.Peek(buf[n : n+spare])
		if bytes.Contains(buf[:n+spare], []byte("504~511")) {
			glog.Infoln("Saw Prompt")
			seenPrompt = true
		}
	}
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
		Prompt: fmt.Sprintf("%s> ", *modemName),
		Cmds: []Command{
			&LoginCmd{Pass: *modemPass},
			&DiagCmd{Out: fh},
		},
		Deadline: *deadline,
	}
	hp := fmt.Sprintf("%s:%d", *modemIP, *modemPort)
	telnet.DialToAndCall(hp, caller)
	fh.Close()
}
