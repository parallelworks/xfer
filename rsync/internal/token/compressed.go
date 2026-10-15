// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package token

import "github.com/parallelworks/xfer/rsync/internal/wire"

// encCodec compresses literal bytes into DEFLATED_DATA records.
type encCodec interface {
	// write feeds literal bytes. Records may be emitted at any time.
	write(p []byte) error
	// flush ends the current literal segment: everything fed so far must
	// be decodable from the records emitted once flush returns.
	flush() error
	// match records the bytes of a matched block in the history.
	match(data []byte)
	// endFile is called after END_FLAG has been written.
	endFile()
}

// compWriter implements the framing shared by the compressed kinds,
// including run coalescing of consecutive matched blocks.
type compWriter struct {
	w     *wire.Writer
	codec encCodec

	haveRun    bool  // a run [runStart, lastToken] is pending
	runStart   int32 // first block of the pending run
	lastToken  int32 // last block of the pending run
	lastRunEnd int32 // last block of the previously written run
	pending    bool  // literal bytes fed since the last flush
}

func newCompWriter(w *wire.Writer, c encCodec) *compWriter {
	return &compWriter{w: w, codec: c}
}

// CompressFile implements FileCompressor.
func (c *compWriter) CompressFile(on bool) {
	if s, ok := c.codec.(interface{ setStore(bool) }); ok {
		s.setStore(!on)
	}
}

func (c *compWriter) writeRun() {
	r := c.runStart - c.lastRunEnd
	n := c.lastToken - c.runStart
	if r >= 0 && r <= 63 {
		if n == 0 {
			c.w.Byte(flagTokenRel + byte(r))
		} else {
			c.w.Byte(flagTokenRunRel + byte(r))
		}
	} else {
		if n == 0 {
			c.w.Byte(flagTokenLong)
		} else {
			c.w.Byte(flagTokenRunLong)
		}
		c.w.Int32(c.runStart)
	}
	if n != 0 {
		c.w.Int16(uint16(n))
	}
	c.lastRunEnd = c.lastToken
	c.haveRun = false
}

func (c *compWriter) Literal(p []byte) error {
	if len(p) == 0 {
		return c.w.Err()
	}
	if c.haveRun {
		c.writeRun()
	}
	c.pending = true
	if err := c.codec.write(p); err != nil {
		return err
	}
	return c.w.Err()
}

func (c *compWriter) Match(block int32, data []byte) error {
	if block < 0 {
		return errBadBlock
	}
	if c.pending {
		c.pending = false
		if err := c.codec.flush(); err != nil {
			return err
		}
	}
	if c.haveRun && block == c.lastToken+1 && block-c.runStart < 65536 {
		c.lastToken = block
	} else {
		if c.haveRun {
			c.writeRun()
		}
		c.haveRun = true
		c.runStart, c.lastToken = block, block
	}
	c.codec.match(data)
	return c.w.Err()
}

func (c *compWriter) End() error {
	if c.pending {
		c.pending = false
		if err := c.codec.flush(); err != nil {
			return err
		}
	}
	if c.haveRun {
		c.writeRun()
	}
	c.w.Byte(flagEnd)
	c.lastRunEnd = 0
	c.codec.endFile()
	return c.w.Err()
}

// recordSink turns a byte stream into DEFLATED_DATA records, optionally
// holding back the last `hold` bytes (the zlib sync marker) until the
// owner decides what to do with them.
type recordSink struct {
	w    *wire.Writer
	hold int
	buf  []byte
}

func writeRecord(w *wire.Writer, p []byte) {
	w.Byte(flagDeflated | byte(len(p)>>8))
	w.Byte(byte(len(p)))
	w.Write(p)
}

func (s *recordSink) Write(p []byte) (int, error) {
	n := len(p)
	if s.hold == 0 && len(s.buf) == 0 {
		// Fast path: emit full records straight from p.
		for len(p) >= MaxDataCount {
			writeRecord(s.w, p[:MaxDataCount])
			p = p[MaxDataCount:]
		}
	}
	s.buf = append(s.buf, p...)
	off := 0
	for len(s.buf)-off >= MaxDataCount+s.hold {
		writeRecord(s.w, s.buf[off:off+MaxDataCount])
		off += MaxDataCount
	}
	if off > 0 {
		s.buf = s.buf[:copy(s.buf, s.buf[off:])]
	}
	if err := s.w.Err(); err != nil {
		return 0, err
	}
	return n, nil
}

// emitAll writes the buffered bytes, minus the trailing `drop` bytes.
func (s *recordSink) emitAll(drop int) {
	b := s.buf[:len(s.buf)-drop]
	for len(b) > 0 {
		n := min(len(b), MaxDataCount)
		writeRecord(s.w, b[:n])
		b = b[n:]
	}
	s.buf = s.buf[:0]
}

// decCodec decompresses DEFLATED_DATA records.
type decCodec interface {
	// begin is called after a DEFLATED_DATA flag byte was read from the
	// wire (the length's low byte and payload have not been read).
	begin(flag byte) error
	// read returns decoded literal bytes. When the data has ended it
	// returns end=true and the non-data flag byte it read from the wire.
	read() (lit []byte, flag byte, end bool, err error)
	// see records a matched block's bytes in the history.
	see(data []byte)
	// endFile is called after END_FLAG was read.
	endFile()
	close() error
}

type compReader struct {
	r       *wire.Reader
	codec   decCodec
	inData  bool
	rxToken int32
	runLeft int
	err     error
}

func newCompReader(r *wire.Reader, c decCodec) *compReader {
	return &compReader{r: r, codec: c}
}

// readPayload reads the rest of a DEFLATED_DATA record whose flag byte
// was already read.
func readPayload(r *wire.Reader, flag byte, buf []byte) ([]byte, error) {
	n := int(flag&0x3F)<<8 | int(r.Byte())
	if cap(buf) < MaxDataCount {
		buf = make([]byte, MaxDataCount)
	}
	buf = buf[:n]
	r.ReadFull(buf)
	return buf, r.Err()
}

func (c *compReader) Next() ([]byte, int32, bool, error) {
	if c.err != nil {
		return nil, -1, false, c.err
	}
	lit, block, done, err := c.next()
	if err != nil {
		c.err = err
	}
	return lit, block, done, err
}

func (c *compReader) next() ([]byte, int32, bool, error) {
	if c.runLeft > 0 {
		c.runLeft--
		c.rxToken++
		return nil, c.rxToken, false, nil
	}
	for {
		var flag byte
		if c.inData {
			lit, f, end, err := c.codec.read()
			if err != nil {
				return nil, -1, false, err
			}
			if !end {
				return lit, -1, false, nil
			}
			c.inData = false
			flag = f
		} else {
			flag = c.r.Byte()
			if err := c.r.Err(); err != nil {
				return nil, -1, false, err
			}
		}
		if flag&0xC0 == flagDeflated {
			if err := c.codec.begin(flag); err != nil {
				return nil, -1, false, err
			}
			c.inData = true
			continue
		}
		if flag == flagEnd {
			c.rxToken = 0
			c.codec.endFile()
			return nil, -1, true, nil
		}
		var run bool
		switch {
		case flag&0x80 != 0:
			c.rxToken += int32(flag & 0x3F)
			run = flag&0x40 != 0
		case flag == flagTokenLong || flag == flagTokenRunLong:
			c.rxToken = c.r.Int32()
			run = flag&1 != 0
		default:
			return nil, -1, false, badf("invalid flag byte 0x%02x", flag)
		}
		if run {
			c.runLeft = int(c.r.Int16())
			if c.runLeft == 0 && c.r.Err() == nil {
				return nil, -1, false, badf("zero-length token run")
			}
		}
		if err := c.r.Err(); err != nil {
			return nil, -1, false, err
		}
		if c.rxToken < 0 || int64(c.rxToken)+int64(c.runLeft) > maxBlockIndex {
			return nil, -1, false, badf("block index %d (+%d) out of range", c.rxToken, c.runLeft)
		}
		return nil, c.rxToken, false, nil
	}
}

func (c *compReader) SeeMatch(data []byte) { c.codec.see(data) }

func (c *compReader) Close() error { return c.codec.close() }
