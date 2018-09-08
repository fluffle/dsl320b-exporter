package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"

	"github.com/golang/glog"
)

// I really didn't want to have to copy a whole pile of bufio into
// this code, but ugh I appear to have exhausted other options.

type Reader struct {
	buf     []byte
	rd      io.Reader // reader provided by the client
	r, p, w int       // buf read, operate and write positions
	err     error
}

const (
	bufSize                  = 32 * 1024
	maxConsecutiveEmptyReads = 100
	maxInt                   = int(^uint(0) >> 1)
)

var (
	ErrTooLarge      = errors.New("Reader: tried to grow buffer too large")
	ErrBufferFull    = errors.New("Reader: buffer full")
	ErrNegativeCount = errors.New("Reader: negative read/peek size")
	ErrAdvanceTooFar = errors.New("Reader: tried to advance past write pointer")
	errNegativeRead  = errors.New("Reader: source returned negative count from Read")
	errShortRead     = errors.New("Reader: source returned too few bytes")
)

// NewReader returns a new Reader whose buffer has the default size.
func NewReader(rd io.Reader) *Reader {
	return &Reader{
		buf: make([]byte, bufSize),
		rd:  rd,
	}
}

// empty returns whether the unread portion of the buffer is empty.
func (r *Reader) empty() bool { return r.w <= r.r }

// Len returns the number of bytes of the unread portion of the buffer.
func (r *Reader) Len() int { return r.w - r.p }

// Cap returns the capacity of the buffer's underlying byte slice, that is, the
// total space allocated for the buffer's data.
func (r *Reader) Cap() int { return cap(r.buf) }

// Reset resets the buffer to be empty,
// but it retains the underlying storage for use by future writes.
func (r *Reader) Reset() {
	r.buf = r.buf[:0]
	r.r, r.p, r.w = 0, 0, 0
}

// tryGrowByReslice is a inlineable version of grow for the fast-case where the
// internal buffer only needs to be resliced.
// It returns the index where bytes should be written and whether it succeeded.
func (r *Reader) tryGrowByReslice(n int) (int, bool) {
	if n <= cap(r.buf)-r.w {
		r.buf = r.buf[:r.w+n]
		return r.w, true
	}
	return 0, false
}

// grow grows the buffer to guarantee space for n more bytes.
// It returns the index where bytes should be written.
// If the buffer can't grow it will panic with ErrTooLarge.
func (r *Reader) grow(n int) int {
	glog.V(2).Infoln("grow:", n)
	// If we've read all the data, reset to recover space.
	if r.r != 0 && r.empty() {
		r.Reset()
	}
	// Try to grow by means of a reslice.
	if i, ok := r.tryGrowByReslice(n); ok {
		return i
	}
	c := cap(r.buf)
	if n <= c/2-r.Len() {
		// We can slide things down instead of allocating a new
		// slice. We only need Len+n <= c to slide, but
		// we instead let capacity get twice as large so we
		// don't spend all our time copying.
		copy(r.buf, r.buf[r.r:r.w])
	} else if c > maxInt-c-n {
		panic(ErrTooLarge)
	} else {
		// Not enough space anywhere, we need to allocate.
		buf := makeSlice(2*c + n)
		copy(buf, r.buf[r.r:r.w])
		r.buf = buf
	}
	r.w -= r.r
	r.p -= r.r
	r.r = 0
	r.buf = r.buf[:r.w+n]
	return r.w
}

// makeSlice allocates a slice of size n. If the allocation fails, it panics
// with ErrTooLarge.
func makeSlice(n int) []byte {
	// If the make fails, give a known error.
	defer func() {
		if recover() != nil {
			panic(ErrTooLarge)
		}
	}()
	return make([]byte, n)
}

// fill reads a new chunk into the buffer.
func (r *Reader) fill() {
	if r.w == len(r.buf) {
		// Write pointer has hit end of current buffer,
		r.grow(bufSize / 2)
	}

	// Read new data: try a limited number of times.
	for i := maxConsecutiveEmptyReads; i > 0; i-- {
		n, err := r.rd.Read(r.buf[r.w:])
		if n < 0 {
			panic(errNegativeRead)
		}
		r.w += n
		if err != nil {
			r.err = err
			return
		}
		if n > 0 {
			return
		}
	}
	r.err = io.ErrNoProgress
}

func (r *Reader) readErr() error {
	err := r.err
	r.err = nil
	return err
}

// Done advances the read pointer to the operate pointer, allowing
// any subsequent fill() calls to drop the data betwen those two points.
// Code below this point advances the operate pointer as it consumes data
// from the underlying reader, but can also move that pointer backwards
// when it encounters unexpected input if necessary. Fixing the read pointer
// until Done is called ensures that all the data between r.r and r.p will
// be kept in the buffer until it is no longer needed.
func (r *Reader) Done() {
	r.r = r.p
}

// Peek returns the next n bytes without advancing the reader. The bytes stop
// being valid at the next read call. If Peek returns fewer than n bytes, it
// also returns an error explaining why the read is short. The error is
// ErrBufferFull if n is larger than b's buffer size.
func (r *Reader) Peek(n int) ([]byte, error) {
	if n < 0 {
		return nil, ErrNegativeCount
	}

	for r.Len() < n && r.err == nil {
		r.fill()
	}

	var err error
	if avail := r.Len(); avail < n {
		// not enough data in buffer
		n = avail
		err = r.readErr()
	}
	return r.buf[r.p : r.p+n], err
}

func (r *Reader) Advance(n int) error {
	if n < 0 {
		return ErrNegativeCount
	}
	if n > r.Len() {
		return ErrAdvanceTooFar
	}
	r.p += n
	return nil
}

func (r *Reader) Consume(n int) ([]byte, error) {
	b, err := r.Peek(n)
	if err != nil {
		return b, err
	}
	return b, r.Advance(n)
}

func (r *Reader) expectBytes(want []byte) (bool, error) {
	b, err := r.Peek(len(want))
	if err != nil {
		return false, err
	}
	if bytes.Equal(b, want) {
		return true, r.Advance(len(want))
	}
	return false, nil
}

func (r *Reader) ExpectBytes(want []byte) error {
	ok, err := r.expectBytes(want)
	if ok {
		r.Done()
		return nil
	}
	if err == nil {
		// This peek must succeed otherwise err wouldn't be nil.
		got, _ := r.Peek(len(want))
		err = fmt.Errorf("expect: expected %q, got %q", want, got)
	}
	return err
}

func (r *Reader) ReadTo(delim string) ([]byte, error) {
	b, err := r.until([]byte(delim), false, false)
	r.Done()
	return b, err
}

func (r *Reader) ReadPast(delim string) ([]byte, error) {
	b, err := r.until([]byte(delim), true, false)
	r.Done()
	return b, err
}

func (r *Reader) SeekTo(delim string) error {
	_, err := r.until([]byte(delim), false, true)
	r.Done()
	return err
}

func (r *Reader) SeekPast(delim string) error {
	_, err := r.until([]byte(delim), true, true)
	r.Done()
	return err
}

func (r *Reader) until(delim []byte, consume bool, skip bool) (read []byte, err error) {
	mark := r.p
	for {
		// Search the unsearched parts of the buffer.
		if i := bytes.Index(r.buf[mark:r.w], delim); i >= 0 {
			if consume {
				read = r.buf[r.p : mark+i+len(delim)]
				r.p = mark + i + len(delim)
			} else {
				read = r.buf[r.p : mark+i]
				r.p = mark + i
			}
			break
		}

		// Pending error?
		if r.err != nil {
			read = r.buf[r.p:r.w]
			r.p = r.w
			err = r.readErr()
			break
		}

		mark = r.w - len(delim) + 1
		if skip {
			r.p = mark
			r.Done()
		}
		r.fill() // more data please!
	}
	return
}

func (r *Reader) scan(f func(byte) bool) ([]byte, error) {
	start := r.p
	b, err := r.Peek(1)
	for err == nil {
		if len(b) == 0 {
			err = errShortRead
			break
		}
		if !f(b[0]) {
			break
		}
		r.Advance(1)
		b, err = r.Peek(1)
	}
	return r.buf[start:r.p], err
}

func (r *Reader) Scan(f func(byte) bool) ([]byte, error) {
	b, err := r.scan(f)
	r.Done()
	return b, err
}

func IsDigit(b byte) bool {
	return b >= '0' && b <= '9'
}

func IsHexDigit(b byte) bool {
	return IsDigit(b) ||
		(b >= 'a' && b <= 'f') ||
		(b >= 'A' && b <= 'F')
}

// Float64 consumes numbers matching the following regex:
//   -?[0-9]+(.[0-9]+)?([eE][+-]?[0-9]+)?
func (r *Reader) Float64() (float64, error) {
	start := r.p
	rewind := func(err error) (float64, error) {
		r.p = start
		return 0, err
	}
	// At a number of points in this function we look for optional further input.
	// If the data from the underlying socket ends here we'll get io.EOF and
	// short reads, but we might still have a valid float. This function
	// handles this case, I hope.
	tryParseAtEOF := func(err error) (float64, error) {
		if err == io.EOF {
			f, err := strconv.ParseFloat(string(r.buf[start:r.p]), 64)
			if err != nil {
				return rewind(err)
			}
			r.Done()
			return f, nil
		}
		return rewind(err)
	}
	scanNumbers := func() error {
		b, err := r.scan(IsDigit)
		if err == nil && len(b) == 0 {
			// Expected >0 digits for valid float, so mimic strconv's error.
			return strconv.ErrSyntax
		}
		return err
	}

	// Leading -
	if _, err := r.expectBytes([]byte{'-'}); err != nil {
		return rewind(err)
	}
	// First set of numbers.
	if err := scanNumbers(); err != nil {
		return tryParseAtEOF(err)
	}
	// Decimal point and optional second set of numbers.
	if ok, err := r.expectBytes([]byte{'.'}); err != nil {
		return tryParseAtEOF(err)
	} else if ok {
		if err := scanNumbers(); err != nil {
			return tryParseAtEOF(err)
		}
	}
	// Optional exponent.
	if b, err := r.Peek(1); err != nil {
		return tryParseAtEOF(err)
	} else if b[0] == 'e' || b[0] == 'E' {
		r.Advance(1)
		b, err = r.Peek(1)
		if err != nil {
			// ParseFloat would fail here because we have exponent at EOF
			return rewind(err)
		}
		if b[0] == '+' || b[0] == '-' {
			r.Advance(1)
		}
		if err = scanNumbers(); err != nil {
			return tryParseAtEOF(err)
		}
	}
	// Got here, so we should have advanced r.p past a valid-looking float.
	return tryParseAtEOF(io.EOF)
}
