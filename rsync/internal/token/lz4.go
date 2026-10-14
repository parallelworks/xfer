// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package token

import (
	"encoding/binary"
	"errors"
	"math/bits"

	"github.com/parallelworks/xfer/rsync/internal/wire"
)

// LZ4: every DEFLATED_DATA record is an independent raw LZ4 block (no
// frame, no history between records). The block encoder and decoder below
// are written from the LZ4 block format description.

const (
	// lz4ChunkMax keeps the worst-case block (n + n/255 + 16) within
	// MaxDataCount.
	lz4ChunkMax = 16256
	// lz4OutMax is the receiver's decode buffer: LZ4_compressBound(32768).
	lz4OutMax = 32768 + 32768/255 + 16

	lz4MinMatch   = 4
	lz4LastLits   = 5  // the last 5 bytes are always literals
	lz4MFLimit    = 12 // a match may not start in the last 12 bytes
	lz4HashLog    = 12
	lz4MaxOffset  = 65535
	lz4SkipShift  = 6
	lz4TableEntry = 1 << lz4HashLog
)

type lz4Enc struct {
	w     *wire.Writer
	in    []byte
	out   []byte
	table [lz4TableEntry]int32
}

func newLZ4Enc(w *wire.Writer) *lz4Enc {
	return &lz4Enc{w: w, in: make([]byte, 0, lz4ChunkMax), out: make([]byte, 0, MaxDataCount)}
}

func (l *lz4Enc) write(p []byte) error {
	for len(p) > 0 {
		n := min(len(p), lz4ChunkMax-len(l.in))
		l.in = append(l.in, p[:n]...)
		p = p[n:]
		if len(l.in) == lz4ChunkMax {
			l.emit()
		}
	}
	return l.w.Err()
}

func (l *lz4Enc) emit() {
	if len(l.in) == 0 {
		return
	}
	l.out = lz4Compress(l.out[:0], l.in, &l.table)
	writeRecord(l.w, l.out)
	l.in = l.in[:0]
}

func (l *lz4Enc) flush() error {
	l.emit()
	return l.w.Err()
}

func (l *lz4Enc) match([]byte) {}
func (l *lz4Enc) endFile()     {}

func lz4Hash(u uint32) uint32 { return (u * 2654435761) >> (32 - lz4HashLog) }

func lz4AppendLen(dst []byte, n int) []byte {
	for n >= 255 {
		dst = append(dst, 255)
		n -= 255
	}
	return append(dst, byte(n))
}

func lz4AppendSeq(dst, lits []byte, offset, matchLen int) []byte {
	tok := byte(0)
	if len(lits) >= 15 {
		tok = 15 << 4
	} else {
		tok = byte(len(lits)) << 4
	}
	ml := matchLen - lz4MinMatch
	if matchLen > 0 {
		tok |= byte(min(ml, 15))
	}
	dst = append(dst, tok)
	if len(lits) >= 15 {
		dst = lz4AppendLen(dst, len(lits)-15)
	}
	dst = append(dst, lits...)
	if matchLen == 0 {
		return dst
	}
	dst = append(dst, byte(offset), byte(offset>>8))
	if ml >= 15 {
		dst = lz4AppendLen(dst, ml-15)
	}
	return dst
}

// lz4Compress appends the LZ4 block encoding of src to dst. It is a simple
// greedy single-probe hash matcher.
func lz4Compress(dst, src []byte, table *[lz4TableEntry]int32) []byte {
	clear(table[:])
	n := len(src)
	anchor := 0
	if n >= lz4MFLimit+1 {
		limit := n - lz4MFLimit // match start must be < limit+1
		matchEnd := n - lz4LastLits
		i := 0
		for i <= limit {
			u := binary.LittleEndian.Uint32(src[i:])
			h := lz4Hash(u)
			cand := int(table[h]) - 1
			table[h] = int32(i + 1)
			if cand < 0 || i-cand > lz4MaxOffset || binary.LittleEndian.Uint32(src[cand:]) != u {
				i += 1 + (i-anchor)>>lz4SkipShift
				continue
			}
			// Extend backwards over pending literals.
			for i > anchor && cand > 0 && src[i-1] == src[cand-1] {
				i--
				cand--
			}
			ml := lz4MinMatch
			for i+ml+8 <= matchEnd {
				x := binary.LittleEndian.Uint64(src[i+ml:]) ^ binary.LittleEndian.Uint64(src[cand+ml:])
				if x != 0 {
					ml += bits.TrailingZeros64(x) >> 3
					goto done
				}
				ml += 8
			}
			for i+ml < matchEnd && src[i+ml] == src[cand+ml] {
				ml++
			}
		done:
			dst = lz4AppendSeq(dst, src[anchor:i], i-cand, ml)
			i += ml
			anchor = i
			if i <= limit {
				// Index a position inside the match to help the next search.
				p := i - 2
				table[lz4Hash(binary.LittleEndian.Uint32(src[p:]))] = int32(p + 1)
			}
		}
	}
	return lz4AppendSeq(dst, src[anchor:], 0, 0)
}

var errLZ4Corrupt = errors.New("corrupt lz4 block")

func lz4ReadLen(src []byte, i int, n int) (int, int, error) {
	for {
		if i >= len(src) {
			return 0, i, errLZ4Corrupt
		}
		b := src[i]
		i++
		n += int(b)
		if n > lz4OutMax {
			return 0, i, errLZ4Corrupt
		}
		if b != 255 {
			return n, i, nil
		}
	}
}

// lz4Decompress decodes block src into dst[:0]; the output may be at most
// cap(dst) bytes.
func lz4Decompress(dst, src []byte) ([]byte, error) {
	limit := cap(dst)
	dst = dst[:0]
	i := 0
	for i < len(src) {
		tok := src[i]
		i++
		lit := int(tok >> 4)
		var err error
		if lit == 15 {
			if lit, i, err = lz4ReadLen(src, i, lit); err != nil {
				return nil, err
			}
		}
		if i+lit > len(src) || len(dst)+lit > limit {
			return nil, errLZ4Corrupt
		}
		dst = append(dst, src[i:i+lit]...)
		i += lit
		if i == len(src) {
			return dst, nil // last sequence: literals only
		}
		if i+2 > len(src) {
			return nil, errLZ4Corrupt
		}
		off := int(src[i]) | int(src[i+1])<<8
		i += 2
		ml := int(tok & 15)
		if ml == 15 {
			if ml, i, err = lz4ReadLen(src, i, ml); err != nil {
				return nil, err
			}
		}
		ml += lz4MinMatch
		if off == 0 || off > len(dst) || len(dst)+ml > limit {
			return nil, errLZ4Corrupt
		}
		start := len(dst) - off
		if off >= ml {
			dst = append(dst, dst[start:start+ml]...)
		} else {
			for k := range ml {
				dst = append(dst, dst[start+k])
			}
		}
	}
	return nil, errLZ4Corrupt // a block must end with a literals-only sequence
}

type lz4Dec struct {
	r       *wire.Reader
	payload []byte
	out     []byte
	have    bool
}

func newLZ4Dec(r *wire.Reader) *lz4Dec {
	return &lz4Dec{r: r, out: make([]byte, 0, lz4OutMax)}
}

func (l *lz4Dec) begin(flag byte) error {
	var err error
	if l.payload, err = readPayload(l.r, flag, l.payload); err != nil {
		return err
	}
	if l.out, err = lz4Decompress(l.out, l.payload); err != nil {
		return badf("lz4: %v", err)
	}
	l.have = true
	return nil
}

func (l *lz4Dec) read() ([]byte, byte, bool, error) {
	for {
		if l.have {
			l.have = false
			if len(l.out) > 0 {
				return l.out, 0, false, nil
			}
		}
		flag := l.r.Byte()
		if err := l.r.Err(); err != nil {
			return nil, 0, false, err
		}
		if flag&0xC0 != flagDeflated {
			return nil, flag, true, nil
		}
		if err := l.begin(flag); err != nil {
			return nil, 0, false, err
		}
	}
}

func (l *lz4Dec) see([]byte) {}
func (l *lz4Dec) endFile()   {}
func (l *lz4Dec) close() error {
	return nil
}
