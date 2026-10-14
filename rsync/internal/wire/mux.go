// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"bufio"
	"encoding/binary"
	"io"
	"sync"
	"sync/atomic"
)

// Message tags carried in multiplexed frames.
const (
	MsgData        = 0
	MsgErrorXfer   = 1
	MsgInfo        = 2
	MsgError       = 3
	MsgWarning     = 4
	MsgErrorSocket = 5
	MsgLog         = 6
	MsgClient      = 7
	MsgRedo        = 9
	MsgStats       = 10
	MsgBlockStats  = 11
	MsgIOError     = 22
	MsgIOTimeout   = 33
	MsgNoop        = 42
	MsgErrorExit   = 86
	MsgSuccess     = 100
	MsgDeleted     = 101
	MsgNoSend      = 102
)

const (
	mplexBase   = 7
	maxFrameLen = 0xffffff
)

// MuxWriter frames writes as MSG_DATA once multiplexing is enabled, and
// passes them through unchanged before that.
type MuxWriter struct {
	w       io.Writer
	enabled bool
	frame   []byte
}

// NewMuxWriter returns a MuxWriter over w, initially not multiplexing.
func NewMuxWriter(w io.Writer) *MuxWriter { return &MuxWriter{w: w} }

// Enable starts multiplexing.
func (m *MuxWriter) Enable() { m.enabled = true }

// Enabled reports whether multiplexing has started.
func (m *MuxWriter) Enabled() bool { return m.enabled }

func (m *MuxWriter) Write(p []byte) (int, error) {
	if !m.enabled {
		return m.w.Write(p)
	}
	total := len(p)
	for len(p) > 0 {
		n := min(len(p), maxFrameLen)
		if err := m.writeFrame(MsgData, p[:n]); err != nil {
			return total - len(p), err
		}
		p = p[n:]
	}
	return total, nil
}

func (m *MuxWriter) writeFrame(tag byte, payload []byte) error {
	m.frame = binary.LittleEndian.AppendUint32(m.frame[:0], uint32(mplexBase+int(tag))<<24|uint32(len(payload)))
	m.frame = append(m.frame, payload...)
	_, err := m.w.Write(m.frame)
	if cap(m.frame) > 1<<20 {
		m.frame = nil
	}
	return err
}

// WriteMsg writes a message frame. Messages require multiplexing; before it
// is enabled they are dropped, as rsync does for informational messages.
func (m *MuxWriter) WriteMsg(tag byte, payload []byte) error {
	if !m.enabled {
		return nil
	}
	return m.writeFrame(tag, payload)
}

// Demux reads a possibly multiplexed stream, returning MSG_DATA payloads
// from Read and passing every other message to a handler.
type Demux struct {
	r         *bufio.Reader
	enabled   bool
	remaining int

	// Handle is called for each non-data message. A non-nil error ends
	// reading with that error.
	Handle func(tag byte, payload []byte) error

	// BeforeBlock, if set, is called before a read that may block on the
	// underlying reader. It typically flushes buffered output so the peer
	// is not left waiting for data we have not sent.
	BeforeBlock func()
}

// NewDemux returns a Demux over r, initially not multiplexed.
func NewDemux(r io.Reader) *Demux {
	return &Demux{r: bufio.NewReaderSize(r, 256<<10)}
}

// Enable starts demultiplexing.
func (d *Demux) Enable() { d.enabled = true }

// Enabled reports whether demultiplexing has started.
func (d *Demux) Enabled() bool { return d.enabled }

func (d *Demux) maybeBlock() {
	if d.BeforeBlock != nil && d.r.Buffered() == 0 {
		d.BeforeBlock()
	}
}

func (d *Demux) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if !d.enabled {
		d.maybeBlock()
		return d.r.Read(p)
	}
	for d.remaining == 0 {
		if err := d.readFrame(); err != nil {
			return 0, err
		}
	}
	d.maybeBlock()
	n, err := d.r.Read(p[:min(len(p), d.remaining)])
	d.remaining -= n
	if err == io.EOF && d.remaining > 0 {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

// readFrame reads one frame header, handling non-data messages entirely.
func (d *Demux) readFrame() error {
	var hdr [4]byte
	d.maybeBlock()
	if _, err := io.ReadFull(d.r, hdr[:]); err != nil {
		return err
	}
	v := binary.LittleEndian.Uint32(hdr[:])
	tag := int(v>>24) - mplexBase
	n := int(v & maxFrameLen)
	if tag < 0 {
		return protoErrorf("unexpected tag %d", tag)
	}
	if tag == MsgData {
		d.remaining = n
		return nil
	}
	payload := make([]byte, n)
	d.maybeBlock()
	if _, err := io.ReadFull(d.r, payload); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return err
	}
	if d.Handle == nil {
		return protoErrorf("unexpected message tag %d", tag)
	}
	return d.Handle(byte(tag), payload)
}

// Out is the outgoing half of a connection, shared by the goroutines of a
// role. Holders of the lock write protocol data through W; messages may be
// sent concurrently with SendMsg.
type Out struct {
	mu       sync.Mutex
	flushing atomic.Bool
	Mux      *MuxWriter
	W        *Writer
}

// NewOut returns an Out writing to w.
func NewOut(w io.Writer) *Out {
	mux := NewMuxWriter(w)
	return &Out{Mux: mux, W: NewWriter(mux)}
}

func (o *Out) Lock()   { o.mu.Lock() }
func (o *Out) Unlock() { o.mu.Unlock() }

// SendMsg writes a message after any data already buffered.
func (o *Out) SendMsg(tag byte, payload []byte) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.sendMsgLocked(tag, payload)
}

func (o *Out) sendMsgLocked(tag byte, payload []byte) error {
	if err := o.W.Flush(); err != nil {
		return err
	}
	if err := o.Mux.WriteMsg(tag, payload); err != nil {
		o.W.err = err
		return err
	}
	return nil
}

// TrySendMsg sends a message unless another goroutine is writing, and
// reports whether it did.
func (o *Out) TrySendMsg(tag byte, payload []byte) bool {
	if !o.Mux.Enabled() || !o.mu.TryLock() {
		return false
	}
	defer o.mu.Unlock()
	return o.sendMsgLocked(tag, payload) == nil
}

// SendMsgInt sends a message whose payload is a little-endian int32.
func (o *Out) SendMsgInt(tag byte, v int32) error {
	return o.SendMsg(tag, binary.LittleEndian.AppendUint32(nil, uint32(v)))
}

// Flush flushes buffered data.
func (o *Out) Flush() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.W.Flush()
}

// TryFlush arranges for buffered data to be flushed without blocking the
// caller. It is meant for Demux's BeforeBlock hook: a reader must never
// wait on a write, since the peer may itself be blocked writing to us and
// only our reads can unblock it.
func (o *Out) TryFlush() {
	if o.mu.TryLock() {
		idle := o.W.Buffered() == 0
		o.mu.Unlock()
		if idle {
			return
		}
	}
	// A flusher that has not yet taken the lock will also write anything
	// buffered before this call.
	if o.flushing.CompareAndSwap(false, true) {
		go func() {
			o.mu.Lock()
			o.flushing.Store(false)
			o.W.Flush()
			o.mu.Unlock()
		}()
	}
}
