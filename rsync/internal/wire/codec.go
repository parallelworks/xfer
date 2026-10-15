// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

// Package wire implements the primitive encodings and the message
// multiplexing of the rsync protocol.
//
// Readers and Writers keep the first error they encounter; later calls are
// no-ops that return zero values, and Err reports the error. Callers check
// Err at natural boundaries (the end of a file-list entry, a message, …)
// instead of after every field.
package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Special file-list indexes.
const (
	NdxDone        = -1
	NdxFlistEOF    = -2
	NdxDelStats    = -3
	NdxFlistOffset = -101
)

// ErrProtocol wraps errors caused by a peer violating the protocol.
var ErrProtocol = errors.New("protocol error")

func protoErrorf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrProtocol, fmt.Sprintf(format, args...))
}

// Reader decodes protocol primitives.
type Reader struct {
	r   io.Reader
	err error
	buf [16]byte

	// Protocol is the negotiated protocol version; it selects encodings
	// that changed over time (ndx, longint vs varlong).
	Protocol int

	ndxPrevPos, ndxPrevNeg int32
}

// NewReader returns a Reader decoding from r.
func NewReader(r io.Reader) *Reader {
	return &Reader{r: r, ndxPrevPos: -1, ndxPrevNeg: 1}
}

// Err returns the first error encountered.
func (r *Reader) Err() error { return r.err }

// SetErr records err unless an error is already recorded.
func (r *Reader) SetErr(err error) {
	if r.err == nil {
		r.err = err
	}
}

// Underlying returns the io.Reader that r decodes from.
func (r *Reader) Underlying() io.Reader { return r.r }

// ReadFull fills p. It is a no-op after an error.
func (r *Reader) ReadFull(p []byte) {
	if r.err != nil {
		clear(p)
		return
	}
	if _, err := io.ReadFull(r.r, p); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		r.err = err
		clear(p)
	}
}

// Read implements io.Reader for bulk data; it does not use the sticky error.
func (r *Reader) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	return r.r.Read(p)
}

// Byte reads one byte.
func (r *Reader) Byte() byte {
	r.ReadFull(r.buf[:1])
	return r.buf[0]
}

// Int16 reads a little-endian 16-bit value.
func (r *Reader) Int16() uint16 {
	r.ReadFull(r.buf[:2])
	return binary.LittleEndian.Uint16(r.buf[:2])
}

// Int32 reads a little-endian 32-bit integer.
func (r *Reader) Int32() int32 {
	r.ReadFull(r.buf[:4])
	return int32(binary.LittleEndian.Uint32(r.buf[:4]))
}

// Longint reads rsync's pre-protocol-30 64-bit encoding: an int32, or -1
// followed by 8 bytes.
func (r *Reader) Longint() int64 {
	v := r.Int32()
	if v != -1 {
		return int64(v)
	}
	r.ReadFull(r.buf[:8])
	return int64(binary.LittleEndian.Uint64(r.buf[:8]))
}

// extraBytes maps the top six bits of a varint's first byte to the number
// of bytes that follow it.
var extraBytes = [64]uint8{
	0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1,
	2, 2, 2, 2, 2, 2, 2, 2, 3, 3, 3, 3, 4, 4, 5, 6,
}

// Varint reads rsync's variable-length int32.
func (r *Reader) Varint() int32 {
	first := r.Byte()
	extra := int(extraBytes[first/4])
	if extra == 0 {
		return int32(first)
	}
	if extra > 4 {
		r.SetErr(protoErrorf("varint overflow"))
		return 0
	}
	var b [5]byte
	r.ReadFull(b[:extra])
	b[extra] = first & (1<<(8-extra) - 1)
	return int32(binary.LittleEndian.Uint32(b[:4]))
}

// Varlong reads rsync's variable-length int64 with at least minBytes bytes.
func (r *Reader) Varlong(minBytes int) int64 {
	var head [8]byte
	r.ReadFull(head[:minBytes])
	var b [9]byte
	copy(b[:], head[1:minBytes])
	extra := int(extraBytes[head[0]/4])
	if minBytes+extra > 9 {
		r.SetErr(protoErrorf("varlong overflow"))
		return 0
	}
	if extra > 0 {
		r.ReadFull(b[minBytes-1 : minBytes-1+extra])
		b[minBytes+extra-1] = head[0] & (1<<(8-extra) - 1)
	} else {
		b[minBytes-1] = head[0]
	}
	return int64(binary.LittleEndian.Uint64(b[:8]))
}

// Vstring reads a string with a one- or two-byte length prefix, rejecting
// strings longer than max.
func (r *Reader) Vstring(max int) string {
	n := int(r.Byte())
	if n&0x80 != 0 {
		n = (n&0x7f)<<8 | int(r.Byte())
	}
	if n > max {
		r.SetErr(protoErrorf("over-long vstring received (%d > %d)", n, max))
		return ""
	}
	p := make([]byte, n)
	r.ReadFull(p)
	return string(p)
}

// Ndx reads a file-list index.
func (r *Reader) Ndx() int32 {
	if r.Protocol < 30 {
		return r.Int32()
	}
	b := r.Byte()
	prev := &r.ndxPrevPos
	if b == 0xff {
		b = r.Byte()
		prev = &r.ndxPrevNeg
	} else if b == 0 {
		return NdxDone
	}
	var u uint32
	if b == 0xfe {
		var x [4]byte
		r.ReadFull(x[:2])
		if x[0]&0x80 != 0 {
			x[3] = x[0] &^ 0x80
			x[0] = x[1]
			r.ReadFull(x[1:3])
			u = binary.LittleEndian.Uint32(x[:])
		} else {
			u = uint32(x[0])<<8 + uint32(x[1]) + uint32(*prev)
		}
	} else {
		u = uint32(b) + uint32(*prev)
	}
	if u > 1<<31-1 {
		r.SetErr(protoErrorf("invalid file index %d", u))
		return 0
	}
	*prev = int32(u)
	if prev == &r.ndxPrevNeg {
		return -int32(u)
	}
	return int32(u)
}

// Writer encodes protocol primitives into a buffer and writes it to the
// underlying writer when flushed or full.
type Writer struct {
	w   io.Writer
	err error
	buf []byte

	Protocol int // protocol version, which selects some encodings

	ndxPrevPos, ndxPrevNeg int32
}

const writeBufSize = 64 << 10

// NewWriter returns a Writer encoding to w.
func NewWriter(w io.Writer) *Writer {
	return &Writer{w: w, buf: make([]byte, 0, writeBufSize), ndxPrevPos: -1, ndxPrevNeg: 1}
}

// Err returns the first error encountered.
func (w *Writer) Err() error { return w.err }

// Flush writes buffered data to the underlying writer.
func (w *Writer) Flush() error {
	if w.err != nil {
		return w.err
	}
	if len(w.buf) > 0 {
		_, w.err = w.w.Write(w.buf)
		w.buf = w.buf[:0]
	}
	if f, ok := w.w.(interface{ Flush() error }); ok && w.err == nil {
		w.err = f.Flush()
	}
	return w.err
}

// Buffered returns the number of bytes not yet flushed.
func (w *Writer) Buffered() int { return len(w.buf) }

func (w *Writer) reserve(n int) {
	if len(w.buf)+n > cap(w.buf) && len(w.buf) > 0 {
		if w.err == nil {
			_, w.err = w.w.Write(w.buf)
		}
		w.buf = w.buf[:0]
	}
}

// Write implements io.Writer. Large writes bypass the buffer.
func (w *Writer) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if len(p) >= cap(w.buf) {
		w.Flush()
		if w.err != nil {
			return 0, w.err
		}
		_, w.err = w.w.Write(p)
		if w.err != nil {
			return 0, w.err
		}
		return len(p), nil
	}
	w.reserve(len(p))
	w.buf = append(w.buf, p...)
	return len(p), w.err
}

// Bytes writes p unchanged.
func (w *Writer) Bytes(p []byte) { w.Write(p) }

// Byte writes one byte.
func (w *Writer) Byte(b byte) {
	w.reserve(1)
	w.buf = append(w.buf, b)
}

// Int16 writes v as a little-endian 16-bit value.
func (w *Writer) Int16(v uint16) {
	w.reserve(2)
	w.buf = binary.LittleEndian.AppendUint16(w.buf, v)
}

// Int32 writes v as a little-endian 32-bit integer.
func (w *Writer) Int32(v int32) {
	w.reserve(4)
	w.buf = binary.LittleEndian.AppendUint32(w.buf, uint32(v))
}

// Longint writes rsync's pre-protocol-30 64-bit encoding.
func (w *Writer) Longint(v int64) {
	if v >= 0 && v <= 1<<31-1 {
		w.Int32(int32(v))
		return
	}
	w.reserve(12)
	w.buf = binary.LittleEndian.AppendUint32(w.buf, 0xffffffff)
	w.buf = binary.LittleEndian.AppendUint64(w.buf, uint64(v))
}

// AppendVarlong appends the variable-length encoding of the low width
// bytes of x, using at least minBytes bytes.
func AppendVarlong(dst []byte, x uint64, width, minBytes int) []byte {
	var b [10]byte
	binary.LittleEndian.PutUint64(b[1:9], x)
	cnt := width
	for cnt > minBytes && b[cnt] == 0 {
		cnt--
	}
	bit := byte(1) << (7 - cnt + minBytes)
	switch {
	case b[cnt] >= bit:
		cnt++
		b[0] = ^(bit - 1)
	case cnt > minBytes:
		b[0] = b[cnt] | ^(bit*2 - 1)
	default:
		b[0] = b[cnt]
	}
	return append(dst, b[:cnt]...)
}

// Varint writes rsync's variable-length int32.
func (w *Writer) Varint(v int32) {
	w.reserve(5)
	w.buf = AppendVarlong(w.buf, uint64(uint32(v)), 4, 1)
}

// Varlong writes rsync's variable-length int64 using at least minBytes
// bytes.
func (w *Writer) Varlong(v int64, minBytes int) {
	w.reserve(9)
	w.buf = AppendVarlong(w.buf, uint64(v), 8, minBytes)
}

// Vstring writes s with a one- or two-byte length prefix.
func (w *Writer) Vstring(s string) {
	if len(s) > 0x7fff {
		if w.err == nil {
			w.err = fmt.Errorf("attempting to send over-long vstring (%d > %d)", len(s), 0x7fff)
		}
		return
	}
	w.reserve(2)
	if len(s) > 0x7f {
		w.buf = append(w.buf, byte(len(s)>>8|0x80))
	}
	w.buf = append(w.buf, byte(len(s)))
	w.Write([]byte(s))
}

// Ndx writes a file-list index.
func (w *Writer) Ndx(ndx int32) {
	if w.Protocol < 30 {
		w.Int32(ndx)
		return
	}
	w.reserve(6)
	var diff int32
	switch {
	case ndx >= 0:
		diff = ndx - w.ndxPrevPos
		w.ndxPrevPos = ndx
	case ndx == NdxDone:
		w.buf = append(w.buf, 0)
		return
	default:
		w.buf = append(w.buf, 0xff)
		ndx = -ndx
		diff = ndx - w.ndxPrevNeg
		w.ndxPrevNeg = ndx
	}
	switch {
	case diff > 0 && diff < 0xfe:
		w.buf = append(w.buf, byte(diff))
	case diff < 0 || diff > 0x7fff:
		w.buf = append(w.buf, 0xfe, byte(ndx>>24)|0x80, byte(ndx), byte(ndx>>8), byte(ndx>>16))
	default:
		w.buf = append(w.buf, 0xfe, byte(diff>>8), byte(diff))
	}
}
