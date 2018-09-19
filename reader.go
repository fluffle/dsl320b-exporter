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
	buf []byte
	rd  io.Reader // reader provided by the client
	err error
	// buf read, operate and write positions
	// The invariant 0 <= r <= p <= w <= len(buf) should always hold.
	r, p, w int
	// Stack of marker pointers, all markers should be between r and p.
	// Various nested scanning functions need to remember where they
	// started scanning from, and these pointers need to stay correct
	// across calls to fill and grow.
	m []int
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
	r.m = r.m[:0]
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
// subsequent fill() calls to drop the buffered data before that point.
// Code below this point advances the operate pointer as it consumes data
// from the underlying reader, but can also move that pointer backwards
// when it encounters unexpected input if necessary. Fixing the read pointer
// until Done is called ensures that all the data between r.r and r.p will
// be kept in the buffer until it is no longer needed.
// Done will panic if there are any markers on the marker stack.
func (r *Reader) Done() {
	if len(r.m) > 0 {
		panic("done: advancing read pointer with active markers")
	}
	r.r = r.p
}

// PushMark pushes the current position of the operate pointer onto the
// marker stack for later retrieval.
func (r *Reader) PushMark() {
	r.m = append(r.m, r.p)
}

// PopMark removes and returns a previously-pushed marker from the stack.
func (r *Reader) PopMark() (mark int) {
	if len(r.m) == 0 {
		panic("pop mark: no active markers")
	}
	l := len(r.m) - 1
	r.m, mark = r.m[:l], r.m[l]
	return mark
}

// Peek returns the next n bytes without advancing the operate pointer.
// The bytes stop being valid at the next read call. If Peek returns fewer
// than n bytes, it also returns an error explaining why the read is short.
// The error is ErrBufferFull if n is larger than b's buffer size.
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
	for {
		// Search the unsearched parts of the buffer.
		// glog.V(2).Infof("r=%d m=%v p=%d w=%d len=%d", r.r, r.m, r.p, r.w, len(r.buf))
		if i := bytes.Index(r.buf[r.p:r.w], delim); i >= 0 {
			r.p += i
			if consume {
				r.p += len(delim)
			}
			if !skip {
				mark := r.PopMark()
				read = r.buf[mark:r.p]
			}
			break
		}

		// We've searched the current buffer so move our operate pointer up.
		r.p = r.w - len(delim) + 1
		if skip {
			// We don't care about the data so advance read pointer.
			r.Done()
		}

		// Pending error?
		if r.err != nil {
			if !skip {
				mark := r.PopMark()
				// Return everything scanned so far.
				read = r.buf[mark:r.p]
			}
			err = r.readErr()
			break
		}

		r.fill() // more data please!
	}
	return
}

func (r *Reader) scan(f byteScanner, skip bool) ([]byte, error) {
	if !skip {
		r.PushMark()
	}
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
		if skip {
			r.Done()
		}
		b, err = r.Peek(1)
	}
	if skip {
		return nil, err
	}
	mark := r.PopMark()
	return r.buf[mark:r.p], err
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
			mark := r.PopMark()
			f, err := strconv.ParseFloat(string(r.buf[mark:r.p]), 64)
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
		if _, err := r.expectBytes([]byte("0x")); err != nil {
			return rewind(err)
		}
		// We specify the base explicitly, which means strconv chokes
		// on a leading 0x. Maybe we should defer more of the checks
		// to strconv rather than hacking around it.
		r.PopMark()
		r.PushMark()
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
	mark := r.PopMark()
	i, err := strconv.ParseInt(string(r.buf[mark:r.p]), base, bitSize)
	if err != nil {
		r.p = mark
		return 0, err
	}
	return i, nil
}
