// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package token

import (
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync"

	"github.com/klauspost/compress/zstd"

	"github.com/parallelworks/xfer/rsync/internal/wire"
)

// Zstd.
//
// rsync compresses all literal data of a session into one zstd frame that is
// never ended. Each token boundary is a ZSTD_e_flush, which completes the
// current block, so a receiver can decode every literal byte that precedes
// a token without seeing any later input.
//
// klauspost's streaming Decoder is pull-based: once it has returned all
// output it asks its io.Reader for the next block header, and that reader
// cannot answer "nothing yet" without poisoning the decoder. So the decoder
// runs in a dedicated goroutine whose input reader never touches the wire.
// The main goroutine owns the wire and talks to the decoder goroutine in
// strict ping-pong:
//
//	main                         decoder goroutine
//	                         <-  need     (input exhausted, all output delivered)
//	payload               ->
//	                         <-  data     (zero or more decoded chunks, each acked)
//	                         <-  need
//	read next wire flag; data -> send payload, else it is a token/END.
//
// In sync mode (concurrency 1) Decoder.Read never requests input while it
// still holds decoded output, so a `need` event proves that every literal
// byte of the input handed over so far has been delivered. The main
// goroutine reads exactly one flag byte past the data and never reads ahead
// into the protocol stream that follows a file's tokens.

type zstdEnc struct {
	sink recordSink
	out  zstdOut
	enc  *zstd.Encoder
	err  error
}

// zstdOut collects the encoder's output. The encoder runs a pipeline: one
// goroutine finds matches while another entropy-codes the previous block
// and writes it here, so the output is handed to the wire only from the
// goroutine that owns it.
type zstdOut struct {
	mu  sync.Mutex
	buf []byte
}

func (o *zstdOut) Write(p []byte) (int, error) {
	o.mu.Lock()
	o.buf = append(o.buf, p...)
	o.mu.Unlock()
	return len(p), nil
}

// drain moves the collected output to the sink, which emits full records.
func (z *zstdEnc) drain() error {
	z.out.mu.Lock()
	defer z.out.mu.Unlock()
	if len(z.out.buf) == 0 {
		return nil
	}
	_, err := z.sink.Write(z.out.buf)
	z.out.buf = z.out.buf[:0]
	return err
}

func newZstdEnc(w *wire.Writer, level int) *zstdEnc {
	z := &zstdEnc{sink: recordSink{w: w}}
	z.enc, z.err = zstd.NewWriter(&z.out,
		zstd.WithEncoderConcurrency(2),
		zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(level)),
		zstd.WithWindowSize(2<<20),
		zstd.WithEncoderCRC(false),
		zstd.WithZeroFrames(false),
	)
	return z
}

func (z *zstdEnc) write(p []byte) error {
	if z.err != nil {
		return z.err
	}
	if _, err := z.enc.Write(p); err != nil {
		return err
	}
	return z.drain()
}

func (z *zstdEnc) flush() error {
	if z.err != nil {
		return z.err
	}
	if err := z.enc.Flush(); err != nil {
		return err
	}
	if err := z.drain(); err != nil {
		return err
	}
	z.sink.emitAll(0)
	return z.sink.w.Err()
}

func (z *zstdEnc) match([]byte) {}
func (z *zstdEnc) endFile()     {}

type zevKind int

const (
	zevNeed zevKind = iota // decoder wants input; all output delivered
	zevData                // decoded bytes; must be acked
	zevErr                 // decoder failed; goroutine exits
)

type zevent struct {
	kind zevKind
	data []byte
	err  error
}

var errZstdClosed = errors.New("token: zstd reader closed")

// zstdSrc is the decoder goroutine's input.
type zstdSrc struct {
	cur  []byte
	in   <-chan []byte
	ev   chan<- zevent
	quit <-chan struct{}
}

func (s *zstdSrc) Read(p []byte) (int, error) {
	for len(s.cur) == 0 {
		select {
		case s.ev <- zevent{kind: zevNeed}:
		case <-s.quit:
			return 0, errZstdClosed
		}
		select {
		case s.cur = <-s.in:
		case <-s.quit:
			return 0, errZstdClosed
		}
	}
	n := copy(p, s.cur)
	s.cur = s.cur[n:]
	return n, nil
}

func zstdLoop(dec *zstd.Decoder, ev chan<- zevent, ack <-chan struct{}, quit <-chan struct{}) {
	defer dec.Close()
	buf := make([]byte, 64<<10)
	for {
		n, err := dec.Read(buf)
		if n > 0 {
			select {
			case ev <- zevent{kind: zevData, data: buf[:n]}:
			case <-quit:
				return
			}
			select {
			case <-ack:
			case <-quit:
				return
			}
		}
		if err != nil {
			if err == io.EOF {
				err = errors.New("zstd frame ended")
			}
			select {
			case ev <- zevent{kind: zevErr, err: err}:
			case <-quit:
			}
			return
		}
	}
}

type quitter struct {
	once sync.Once
	ch   chan struct{}
}

func (q *quitter) close() { q.once.Do(func() { close(q.ch) }) }

type zstdDec struct {
	r       *wire.Reader
	payload []byte
	started bool
	idle    bool // the decoder goroutine is waiting for input
	needAck bool // a zevData chunk is outstanding
	in      chan []byte
	ev      chan zevent
	ack     chan struct{}
	q       *quitter
	closed  bool
}

func newZstdDec(r *wire.Reader) *zstdDec {
	return &zstdDec{r: r}
}

func (z *zstdDec) start() error {
	q := &quitter{ch: make(chan struct{})}
	z.q = q
	z.in = make(chan []byte)
	z.ev = make(chan zevent)
	z.ack = make(chan struct{})
	src := &zstdSrc{in: z.in, ev: z.ev, quit: q.ch}
	dec, err := zstd.NewReader(src,
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxWindow(1<<30),
	)
	if err != nil {
		return err
	}
	go zstdLoop(dec, z.ev, z.ack, q.ch)
	// Stop the goroutine if the Reader is dropped without Close.
	runtime.AddCleanup(z, func(q *quitter) { q.close() }, q)
	z.started = true
	return z.waitNeed()
}

// waitNeed waits for the decoder goroutine to ask for input; it must not
// produce output in the meantime.
func (z *zstdDec) waitNeed() error {
	e := <-z.ev
	switch e.kind {
	case zevNeed:
		z.idle = true
		return nil
	case zevErr:
		return badf("zstd: %v", e.err)
	}
	return badf("zstd: unexpected output")
}

func (z *zstdDec) begin(flag byte) error {
	if z.closed {
		return errZstdClosed
	}
	if !z.started {
		if err := z.start(); err != nil {
			return err
		}
	}
	return z.feed(flag)
}

func (z *zstdDec) feed(flag byte) error {
	var err error
	if z.payload, err = readPayload(z.r, flag, z.payload); err != nil {
		return err
	}
	if !z.idle {
		return errors.New("token: zstd decoder not idle")
	}
	z.idle = false
	if len(z.payload) == 0 {
		// An empty record: the decoder still wants input.
		z.idle = true
		return nil
	}
	z.in <- z.payload
	return nil
}

func (z *zstdDec) read() ([]byte, byte, bool, error) {
	if z.closed {
		return nil, 0, false, errZstdClosed
	}
	if z.needAck {
		z.needAck = false
		z.ack <- struct{}{}
	}
	for {
		if z.idle {
			flag := z.r.Byte()
			if err := z.r.Err(); err != nil {
				return nil, 0, false, err
			}
			if flag&0xC0 != flagDeflated {
				return nil, flag, true, nil
			}
			if err := z.feed(flag); err != nil {
				return nil, 0, false, err
			}
			continue
		}
		e := <-z.ev
		switch e.kind {
		case zevNeed:
			z.idle = true
		case zevData:
			z.needAck = true
			return e.data, 0, false, nil
		case zevErr:
			return nil, 0, false, fmt.Errorf("%w: zstd: %v", ErrBadToken, e.err)
		}
	}
}

func (z *zstdDec) see([]byte) {}
func (z *zstdDec) endFile()   {}

func (z *zstdDec) close() error {
	z.closed = true
	if z.q != nil {
		z.q.close()
	}
	return nil
}
