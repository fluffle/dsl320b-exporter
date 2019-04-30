package main

// Parts of this file are modified from the Go standard library
// packages "bufio" and "bytes", which are Copyright (c) The Go Authors
// and redistributed under LICENSE.golang.
//
// I really didn't want to have to copy a whole pile of bufio into
// this code, but ugh I appear to have exhausted other options.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"time"

	"github.com/golang/glog"
)

type Reader struct {
	// Since we are now reading and writing (and growing and indexing)
	// the buffer from two different goroutines, we need to ensure that
	// accesses of the readers and grow are mutually exclusive. Fffuuuu.
	mu  sync.Mutex
	rd  io.Reader // reader provided by the client
	err error

	buf []byte
	// buf read, operate and write positions
	// The invariant 0 <= r <= p <= w <= len(buf) should always hold.
	r, p, w int
	// Stack of marker pointers, all markers should be between r and p.
	// Various nested scanning functions need to remember where they
	// started scanning from, and these pointers need to stay correct
	// across calls to fill and grow.
	m []int

	// Provided by the thing that created the reader, so that the
	// reader can signal it's done reading.
	cancel context.CancelFunc

	// Map of channels for reader code to signify that it is interested
	// in knowing that more data has arrived, or to block on until it
	// does, if it has not already arrived.
	await map[chan bool]bool
}

const (
	bufSize                  = 32 * 1024
	maxConsecutiveEmptyReads = 100
	maxInt                   = int(^uint(0) >> 1)
)

var (
	ErrTimeout       = errors.New("reader: timeout while waiting for more data")
	ErrTooLarge      = errors.New("reader: tried to grow buffer too large")
	ErrNegativeCount = errors.New("reader: negative read/peek size")
	ErrAdvanceTooFar = errors.New("reader: tried to advance past write pointer")
	errNegativeRead  = errors.New("reader: source returned negative count from Read")
	errShortRead     = errors.New("reader: source returned too few bytes")
)

// NewReader returns a new Reader whose buffer has the default size.
// It is not useful without a call to Use() to set an underlying reader.
func NewReader() *Reader {
	return &Reader{
		buf:   make([]byte, bufSize),
		m:     make([]int, 0),
		await: make(map[chan bool]bool),
	}
}

// Reset resets the reader so it is ready to read
// from a new underlying io.Reader when Use is called.
func (r *Reader) Reset() {
	// No need to lock because fill() shouldn't be running at this
	// point -- and we want to panic if it is!
	r.err = nil
	r.rd = nil
	r.cancel = nil
	r.clearBuffer()
}

// clearBuffer resets the buffer to be empty,
// but it retains the underlying storage for use by future writes.
func (r *Reader) clearBuffer() {
	r.buf = r.buf[:0]
	r.m = r.m[:0]
	r.r, r.p, r.w = 0, 0, 0
}

func (r *Reader) Use(rd io.Reader, cancel context.CancelFunc) {
	// Ditto no lock because this starts fill up again.
	r.rd = rd
	r.cancel = cancel
	go r.fill()
}

// tryGrowByReslice is a inlineable version of grow for the fast-case where the
// internal buffer only needs to be resliced. It assumes the lock is held.
// It returns the index where bytes should be written and whether it succeeded.
func (r *Reader) tryGrowByReslice(n int) (int, bool) {
	// Assumes lock is held by fill().
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
	// Assumes lock is held by fill().
	glog.V(2).Infoln("grow:", n)
	// If we've read all the data, clear buffer to recover space.
	if r.r != 0 && r.w <= r.r {
		r.clearBuffer()
	}
	// Try to grow by means of a reslice.
	if i, ok := r.tryGrowByReslice(n); ok {
		return i
	}
	c := cap(r.buf)
	if n <= c/2-(r.w-r.p) {
		// We can slide things down instead of allocating a new
		// slice. We only need (r.w - r.p) + n <= c to slide, but
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
	// We slid the read pointer back to the beginning of the buffer,
	// so we need to pull all our other pointers backwards.
	r.w -= r.r
	r.p -= r.r
	for i := range r.m {
		r.m[i] -= r.r
	}
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

// We want to be notified eagerly of an EOF due to the server dropping
// our connection, but the "only" way to achieve this is to have something
// constantly listening for data. So we run the body of fill() in a loop
// and notify other goroutines of new data via AwaitData channels.
func (r *Reader) fill() {
	for {
		if r.rd == nil {
			panic("fill: still running at reset!")
		}
		// Hold write lock across all buffer resize ops.
		r.mu.Lock()
		if r.w == len(r.buf) {
			// Write pointer has hit end of current buffer,
			r.grow(bufSize / 2)
		}
		r.mu.Unlock()

		// We can't hold the write lock here because we're expecting fill
		// to be blocked in this read all the time. Fortunately we're
		// reading into the remaining buffer after r.w, so nothing else
		// should be touching that space.
		n, err := r.rd.Read(r.buf[r.w:])
		if n < 0 {
			panic(errNegativeRead)
		}

		r.mu.Lock()
		r.w += n
		for ch := range r.await {
			ch <- true
			close(ch)
			delete(r.await, ch)
		}
		r.mu.Unlock()

		if err != nil {
			r.mu.Lock()
			defer r.mu.Unlock()
			// On errors we cancel the context.CancelFunc we were given and
			// close the channel to signal that we've been disconnected.
			if r.rd == nil {
				// We can have multiple fill() backed up on the lock.
				// If we get here and rd is nil the reader has been reset
				// while this fill was waiting on the lock, so bail out now.
				return
			}
			for ch := range r.await {
				close(ch)
				delete(r.await, ch)
			}
			r.cancel()
			r.err = err
			glog.Errorf("fill error: %v", err)
			return
		}
	}
}

func (r *Reader) readErr() error {
	err := r.err
	r.err = nil
	return err
}

func (r *Reader) AwaitData() <-chan bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	ch := make(chan bool, 1)
	r.await[ch] = true
	return ch
}

// Drain polls and returns when no new data is read for 500ms.
// If drop is true, it will call SeekEnd() to throw away any unread data.
func (r *Reader) Drain(drop bool) {
	consecutive := 0
	for {
		ch := r.AwaitData()
		select {
		case read := <-ch:
			if !read {
				// fill is exiting, we should too!
				return
			}
			if consecutive > 0 {
				consecutive = 0
			}
		case <-time.After(100 * time.Millisecond):
			consecutive++
		}
		if consecutive >= 5 {
			if drop {
				r.SeekEnd()
			}
			return
		}
	}
}

// SeekEnd moves both read and operate pointers to the write pointer.
// It's for use when an error results in these pointers being in an
// unexpected place in the stream. Throw it all away and start again,
// without blocking for more data.
// Note: We don't call clearBuffer here because that would move the
// write pointer while fill is blocked on it, so we'd write to the
// old location but read from the new. This would not work well...
func (r *Reader) SeekEnd() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.m) > 0 {
		glog.Errorf("seek end: advancing read pointer with active markers, these will be dropped")
		r.m = r.m[:0]
	}
	if r.w-r.p > 0 {
		glog.Warningf("seek end: dumping %d bytes of data", r.w-r.p)
		glog.V(2).Infof("buffer contents:\n\n%s\n\n", r.buf[r.p:r.w])
	}
	r.r = r.w
	r.p = r.w
}

// Done advances the read pointer to the operate pointer, allowing
// subsequent fill() calls to drop the buffered data before that point.
// Code below this point advances the operate pointer as it consumes data
// from the underlying reader, but can also move that pointer backwards
// when it encounters unexpected input if necessary. Fixing the read pointer
// until Done is called ensures that all the data between r.r and r.p will
// be kept in the buffer until it is no longer needed.
// Done will panic if there are any markers on the marker stack.
func (r *Reader) Done() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.m) > 0 {
		panic("done: advancing read pointer with active markers")
	}
	r.r = r.p
}

// PushMark pushes the current position of the operate pointer onto the
// marker stack for later retrieval.
func (r *Reader) PushMark() {
	r.mu.Lock()
	r.m = append(r.m, r.p)
	r.mu.Unlock()
}

// PopMark removes and returns a previously-pushed marker from the stack.
func (r *Reader) PopMark() (mark int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.m) == 0 {
		panic("pop mark: no active markers")
	}
	l := len(r.m) - 1
	r.m, mark = r.m[:l], r.m[l]
	return mark
}

// MarkedBytes returns the bytes between the marker at the top of the stack and r.p.
func (r *Reader) MarkedBytes(pop bool) []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.m) == 0 {
		panic("pop mark: no active markers")
	}
	l := len(r.m) - 1
	mark := r.m[l]
	if pop {
		r.m = r.m[:l]
	}
	return r.buf[mark:r.p]
}

// Len returns the number of bytes between the operate pointer and the write
// pointer.
func (r *Reader) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.w - r.p
}

// Peek returns the next n bytes without advancing the operate pointer.
// The bytes stop being valid at the next read call. If Peek returns fewer
// than n bytes, it also returns an error explaining why the read is short.
func (r *Reader) Peek(n int) ([]byte, error) {
	if n < 0 {
		return nil, ErrNegativeCount
	}

	// Don't hold the lock while waiting for reads.
	for {
		ch := r.AwaitData()
		if r.Len() >= n || !<-ch {
			// Channel closed without data received => fill exiting.
			break
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	var err error

	if r.w-r.p < n {
		// not enough data in buffer
		n = r.w - r.p
		err = r.readErr()
	}
	return r.buf[r.p : r.p+n], err
}

func (r *Reader) Advance(n int) error {
	if n < 0 {
		return ErrNegativeCount
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if n > r.w-r.p {
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
	r.Advance(n)
	r.Done()
	return b, nil
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
	if !skip {
		r.PushMark()
	}
	var found bool
	for {
		// We create our notification channel before entering findDelim to
		// prevent a race condition where new data arrives while findDelim
		// is holding the lock. If this occurs, a call to AwaitData after
		// findDelim will sometimes lose the race with fill() to get the lock,
		// and the channel will not be notified that more data can be read.
		ch := r.AwaitData()
		found, err = r.findDelim(delim, consume, skip)

		if found || err != nil {
			if !skip {
				// Return everything scanned so far.
				read = r.MarkedBytes(true)
			}
			return
		}

		if ok := <-ch; !ok {
			// Oh noes we've hit EOF or similar
			err = r.readErr()
			break
		}
	}
	if !skip {
		r.p = r.PopMark()
	}
	return
}

func (r *Reader) findDelim(delim []byte, consume bool, skip bool) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Search the unsearched parts of the buffer.
	// glog.V(2).Infof("r=%d m=%v p=%d w=%d len=%d", r.r, r.m, r.p, r.w, len(r.buf))
	if i := bytes.Index(r.buf[r.p:r.w], delim); i >= 0 {
		r.p += i
		if consume {
			r.p += len(delim)
		}
		return true, nil
	}

	// We've searched the current buffer so move our operate pointer up.
	r.p = r.w - len(delim) + 1
	if skip {
		// We don't care about the data so advance read pointer.
		r.r = r.p
	}

	// Pending error?
	return false, r.readErr()
}

func (r *Reader) scan(f byteScanner, skip bool) ([]byte, error) {
	if !skip {
		r.PushMark()
	}

	var err error
	for {
		if r.Len() == 0 {
			// Try to avoid creating lots of garbage channels by only
			// awaiting when len is already 0.
			ch := r.AwaitData()
			if r.Len() > 0 || <-ch {
				continue
			}
			// EOF or something similar
			err = r.readErr()
			break
		}
		if ok := r.scanByteLocked(f, skip); !ok {
			break
		}
	}
	if !skip {
		return r.MarkedBytes(true), err
	}
	return nil, err
}

func (r *Reader) scanByteLocked(f byteScanner, skip bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !f(r.buf[r.p]) {
		return false
	}
	r.p++
	if skip {
		r.r++
	}
	return true
}

func (r *Reader) Scan(f byteScanner) ([]byte, error) {
	b, err := r.scan(f, false)
	r.Done()
	return b, err
}

func (r *Reader) Skip(f byteScanner) error {
	_, err := r.scan(f, true)
	return err
}

type byteScanner func(byte) bool

func IsDigit(b byte) bool {
	return b >= '0' && b <= '9'
}

func IsHexDigit(b byte) bool {
	return IsDigit(b) ||
		(b >= 'a' && b <= 'f') ||
		(b >= 'A' && b <= 'F')
}

func Is(compare byte) byteScanner {
	return func(b byte) bool {
		return compare == b
	}
}

func Not(compare byte) byteScanner {
	return func(b byte) bool {
		return compare != b
	}
}

// Float64 consumes numbers matching the following regex:
//   -?[0-9]+(.[0-9]+)?([eE][+-]?[0-9]+)?
func (r *Reader) Float64() (float64, error) {
	r.PushMark()
	rewind := func(err error) (float64, error) {
		r.p = r.PopMark()
		return 0, err
	}
	// At a number of points in this function we look for optional further input.
	// If the data from the underlying socket ends here we'll get io.EOF and
	// short reads, but we might still have a valid float. This function
	// handles this case, I hope.
	tryParseAtEOF := func(err error) (float64, error) {
		if err == io.EOF {
			f, err := strconv.ParseFloat(string(r.MarkedBytes(false)), 64)
			mark := r.PopMark()
			if err != nil {
				r.p = mark
				return 0, err
			}
			r.Done()
			return f, nil
		}
		return rewind(err)
	}
	scanNumbers := func() error {
		b, err := r.scan(IsDigit, false)
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

// Int64 consumes numbers matching the following regex:
//   -?[0-9]+
func (r *Reader) Int64() (int64, error) {
	i, err := r.parseInt(10, 64)
	if err != nil {
		return 0, err
	}
	r.Done()
	return i, nil
}

func (r *Reader) Hex64() (int64, error) {
	i, err := r.parseInt(16, 64)
	if err != nil {
		return 0, err
	}
	r.Done()
	return i, nil
}

func (r *Reader) parseInt(base int, bitSize int) (int64, error) {
	r.PushMark()
	rewind := func(err error) (int64, error) {
		r.p = r.PopMark()
		return 0, err
	}

	var scanFunc byteScanner
	switch base {
	case 10:
		scanFunc = IsDigit
		// Leading -
		if _, err := r.expectBytes([]byte{'-'}); err != nil {
			return rewind(err)
		}
	case 16:
		scanFunc = IsHexDigit
		// Leading 0x
		if found, err := r.expectBytes([]byte("0x")); err != nil {
			return rewind(err)
		} else if found {
			// If we specify the base explicitly, strconv chokes
			// on a leading 0x. So, if there is one, set base to 0.
			// This causes strconv to eat the 0x and set base to 16.
			base = 0
		}
	default:
		return 0, fmt.Errorf("invalid base: %d", base)
	}

	// Numbers!
	b, err := r.scan(scanFunc, false)
	if err != nil && err != io.EOF {
		return rewind(err)
	}
	if len(b) == 0 {
		// Expected >0 digits for valid int, so mimic strconv's error.
		return rewind(strconv.ErrSyntax)
	}
	i, err := strconv.ParseInt(string(r.MarkedBytes(false)), base, bitSize)
	mark := r.PopMark()
	if err != nil {
		r.p = mark
		return 0, err
	}
	return i, nil
}
