// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package token

import (
	"bufio"
	"bytes"
	"errors"
	"io"

	"github.com/klauspost/compress/flate"

	"github.com/parallelworks/xfer/rsync/internal/wire"
)

// Zlib and zlibx.
//
// rsync runs one raw deflate stream per file and ends every literal segment
// (the bytes between two tokens) with Z_SYNC_FLUSH, stripping the trailing
// empty stored block's LEN/NLEN (00 00 ff ff). For zlib the matched blocks'
// bytes are also inserted into the compressor's and decompressor's windows
// without being transmitted.
//
// Go's flate packages cannot insert history mid-stream, but every segment
// boundary is a byte-aligned block boundary, so each segment is an
// independent deflate fragment that may reference the previous 32 KiB of
// history. We keep that history explicitly on both sides:
//
//   - The encoder starts every segment with a fresh flate writer whose
//     preset dictionary is the tail of the receiver-visible history. A
//     shorter dictionary is still valid (the encoder then references less
//     of the window), which we use to keep the per-segment setup cost
//     proportional to the segment size.
//   - The decoder resets a flate reader with the last 32 KiB of history as
//     dictionary at the start of every segment and feeds it the segment's
//     records followed by the stripped 00 00 ff ff; the reader then fails
//     with io.ErrUnexpectedEOF when it looks for the next block header,
//     which marks the clean end of the segment.
//
// This reuses a well-tested, fast inflater instead of a custom one; the cost
// is one 32 KiB dictionary copy per segment on the receiver.

const histSize = 32 << 10

var syncMarker = [4]byte{0x00, 0x00, 0xff, 0xff}

// history is the last histSize bytes of a deflate window.
type history struct{ buf []byte }

func (h *history) reset() { h.buf = h.buf[:0] }

func (h *history) write(p []byte) {
	if len(p) >= histSize {
		h.buf = append(h.buf[:0], p[len(p)-histSize:]...)
		return
	}
	if h.buf == nil {
		h.buf = make([]byte, 0, 4*histSize)
	}
	if len(h.buf)+len(p) > cap(h.buf) {
		keep := min(histSize-len(p), len(h.buf))
		h.buf = h.buf[:copy(h.buf, h.buf[len(h.buf)-keep:])]
	}
	h.buf = append(h.buf, p...)
}

// writeMatch adds a matched block. Before protocol 31 rsync fed blocks
// longer than 0xFFFF bytes in 0xFFFF-byte pieces without advancing the
// source pointer, so the window received D[0:0xFFFF] repeatedly and then
// D[0:len%0xFFFF]; only the last two pieces can affect the 32 KiB window.
func (h *history) writeMatch(d []byte, quirk bool) {
	if !quirk || len(d) <= 0xFFFF {
		h.write(d)
		return
	}
	h.write(d[:0xFFFF])
	if r := len(d) % 0xFFFF; r > 0 {
		h.write(d[:r])
	}
}

func (h *history) tail(n int) []byte {
	n = min(n, len(h.buf))
	return h.buf[len(h.buf)-n:]
}

// segBufMax is how much literal data the encoder buffers before it commits
// to a full-size dictionary and starts streaming.
const segBufMax = 64 << 10

type zlibEnc struct {
	sink       recordSink
	fw         *flate.Writer
	fws        [2]*flate.Writer // by store
	store      bool             // the current file is stored, not compressed
	level      int
	hist       history
	seeMatches bool
	quirk      bool
	seg        []byte // literal bytes of the current segment, not yet compressed
	streaming  bool   // fw has been started for the current segment
}

func newZlibEnc(w *wire.Writer, seeMatches, quirk bool, level int) *zlibEnc {
	level = max(1, min(level, 9))
	return &zlibEnc{sink: recordSink{w: w, hold: len(syncMarker)}, level: level, seeMatches: seeMatches, quirk: quirk}
}

// setStore turns compression off (on) for the next segments, for files
// --skip-compress names. Every segment starts a new deflate fragment, so
// its level is free to change.
func (z *zlibEnc) setStore(store bool) { z.store = store }

func (z *zlibEnc) start(dictLen int) error {
	dict := z.hist.tail(dictLen)
	i, level := 0, z.level
	if z.store {
		i, level = 1, flate.NoCompression
	}
	if z.fws[i] == nil {
		fw, err := flate.NewWriterDict(&z.sink, level, dict)
		if err != nil {
			return err
		}
		z.fws[i] = fw
	} else {
		z.fws[i].ResetDict(&z.sink, dict)
	}
	z.fw = z.fws[i]
	z.streaming = true
	return nil
}

func (z *zlibEnc) compress(p []byte) error {
	if _, err := z.fw.Write(p); err != nil {
		return err
	}
	z.hist.write(p)
	return nil
}

func (z *zlibEnc) write(p []byte) error {
	if !z.streaming {
		if len(z.seg)+len(p) <= segBufMax {
			z.seg = append(z.seg, p...)
			return nil
		}
		if err := z.start(histSize); err != nil {
			return err
		}
		if err := z.compress(z.seg); err != nil {
			return err
		}
		z.seg = z.seg[:0]
	}
	return z.compress(p)
}

func (z *zlibEnc) flush() error {
	if !z.streaming {
		// Dictionary setup costs time proportional to its size; scale it
		// with the segment so many tiny segments stay cheap.
		if err := z.start(min(histSize, max(4<<10, 4*len(z.seg)))); err != nil {
			return err
		}
		if err := z.compress(z.seg); err != nil {
			return err
		}
		z.seg = z.seg[:0]
	}
	z.streaming = false
	if err := z.fw.Flush(); err != nil {
		return err
	}
	if !bytes.HasSuffix(z.sink.buf, syncMarker[:]) {
		return errors.New("token: deflate flush did not end with a sync marker")
	}
	z.sink.emitAll(len(syncMarker))
	return z.sink.w.Err()
}

func (z *zlibEnc) match(data []byte) {
	if z.seeMatches {
		z.hist.writeMatch(data, z.quirk)
	}
}

func (z *zlibEnc) endFile() { z.hist.reset() }

// segReader presents one literal segment (consecutive DEFLATED_DATA
// records) to the flate reader. It pulls records from the wire on demand;
// at the first non-data flag it stashes the flag, supplies the stripped
// sync marker and then reports io.EOF.
type segReader struct {
	r      *wire.Reader
	rec    []byte // current record payload (or the sync marker)
	buf    []byte // payload buffer
	pos    int
	ended  bool // rec is the sync marker
	hitEOF bool // io.EOF was returned
	flag   byte // the non-data flag that ended the segment
}

func (s *segReader) fill() error {
	for s.pos == len(s.rec) {
		if s.ended {
			s.hitEOF = true
			return io.EOF
		}
		flag := s.r.Byte()
		if err := s.r.Err(); err != nil {
			return err
		}
		if flag&0xC0 == flagDeflated {
			var err error
			if s.buf, err = readPayload(s.r, flag, s.buf); err != nil {
				return err
			}
			s.rec, s.pos = s.buf, 0
			continue
		}
		s.flag = flag
		s.ended = true
		s.rec, s.pos = syncMarker[:], 0
	}
	return nil
}

func (s *segReader) ReadByte() (byte, error) {
	if s.pos == len(s.rec) {
		if err := s.fill(); err != nil {
			return 0, err
		}
	}
	b := s.rec[s.pos]
	s.pos++
	return b, nil
}

func (s *segReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if s.pos == len(s.rec) {
		if err := s.fill(); err != nil {
			return 0, err
		}
	}
	n := copy(p, s.rec[s.pos:])
	s.pos += n
	return n, nil
}

type zlibDec struct {
	r          *wire.Reader
	fr         io.ReadCloser
	seg        segReader
	br         *bufio.Reader
	hist       history
	seeMatches bool
	quirk      bool
	out        []byte
	segDone    bool
}

func newZlibDec(r *wire.Reader, seeMatches, quirk bool) *zlibDec {
	return &zlibDec{r: r, seg: segReader{r: r}, seeMatches: seeMatches, quirk: quirk}
}

func (z *zlibDec) begin(flag byte) error {
	buf, err := readPayload(z.r, flag, z.seg.buf)
	if err != nil {
		return err
	}
	z.seg = segReader{r: z.r, buf: buf, rec: buf}
	dict := z.hist.tail(histSize)
	// flate has a fast path for *bufio.Reader. The bufio layer cannot read
	// past the segment: segReader never returns bytes beyond the sync
	// marker and reads at most the one flag byte that ends the segment.
	if z.br == nil {
		z.br = bufio.NewReaderSize(&z.seg, MaxDataCount+1)
	} else {
		z.br.Reset(&z.seg)
	}
	if z.fr == nil {
		z.fr = flate.NewReaderDict(z.br, dict)
		z.out = make([]byte, ChunkSize)
	} else if err := z.fr.(flate.Resetter).Reset(z.br, dict); err != nil {
		return err
	}
	z.segDone = false
	return nil
}

func (z *zlibDec) read() ([]byte, byte, bool, error) {
	for !z.segDone {
		n, err := z.fr.Read(z.out)
		if err != nil {
			switch {
			case z.seg.hitEOF && errors.Is(err, io.ErrUnexpectedEOF):
				z.segDone = true
			case err == io.EOF:
				return nil, 0, false, badf("deflate stream ended with a final block")
			case z.r.Err() != nil:
				return nil, 0, false, z.r.Err()
			default:
				return nil, 0, false, badf("inflate: %v", err)
			}
		}
		if n > 0 {
			z.hist.write(z.out[:n])
			return z.out[:n], 0, false, nil
		}
	}
	return nil, z.seg.flag, true, nil
}

func (z *zlibDec) see(data []byte) {
	if z.seeMatches {
		z.hist.writeMatch(data, z.quirk)
	}
}

func (z *zlibDec) endFile()     { z.hist.reset() }
func (z *zlibDec) close() error { return nil }
