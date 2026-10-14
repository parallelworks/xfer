// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

// Package delta implements the rsync algorithm: describing a file as a
// sequence of literal data and references to blocks of a basis file the
// other side already has.
package delta

import (
	"bytes"
	"errors"
	"io"
	"sync"

	"github.com/parallelworks/xfer/rsync/internal/checksum"
)

// Sums are the block checksums of a basis file, as sent by the receiver.
type Sums struct {
	BlockLen  int // length of every block but possibly the last
	Remainder int // length of the last block if shorter than BlockLen, else 0
	StrongLen int // bytes of each strong checksum kept

	Weak   []uint32
	Strong []byte // len(Weak)*StrongLen bytes
}

// Count is the number of blocks.
func (s *Sums) Count() int { return len(s.Weak) }

// Len returns the length of block i.
func (s *Sums) Len(i int) int {
	if i == len(s.Weak)-1 && s.Remainder != 0 {
		return s.Remainder
	}
	return s.BlockLen
}

// StrongSum returns the strong checksum of block i.
func (s *Sums) StrongSum(i int) []byte {
	return s.Strong[i*s.StrongLen : (i+1)*s.StrongLen]
}

// StrongFunc appends the strong block checksum of p to dst.
type StrongFunc func(dst, p []byte) []byte

// Generate computes the block checksums of the basis read from r, which is
// size bytes long.
func Generate(r io.Reader, size int64, blockLen, strongLen int, strong StrongFunc) (*Sums, error) {
	if blockLen <= 0 {
		return nil, errors.New("delta: block length must be positive")
	}
	count := int((size + int64(blockLen) - 1) / int64(blockLen))
	s := &Sums{
		BlockLen:  blockLen,
		Remainder: int(size % int64(blockLen)),
		StrongLen: strongLen,
		Weak:      make([]uint32, count),
		Strong:    make([]byte, 0, count*strongLen),
	}
	buf := make([]byte, blockLen)
	var digest []byte
	for i := range count {
		n := s.Len(i)
		if _, err := io.ReadFull(r, buf[:n]); err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return nil, err
		}
		s.Weak[i] = checksum.Rolling(buf[:n])
		digest = strong(digest[:0], buf[:n])
		s.Strong = append(s.Strong, digest[:strongLen]...)
	}
	return s, nil
}

// Emitter receives the delta. The slices passed to it are only valid for
// the duration of the call.
type Emitter interface {
	// Literal is called with data that must be sent verbatim.
	Literal(p []byte) error
	// Match is called when data equals basis block i.
	Match(i int, data []byte) error
}

const (
	readSize = 256 << 10
	// maxPending bounds how much unmatched data is buffered before it is
	// handed to the emitter as a literal.
	maxPending = 256 << 10
)

// index is an open hash of weak checksums to block numbers.
type index struct {
	mask  uint32
	heads []int32 // per bucket, first block or -1
	next  []int32 // per block, next block in the same bucket or -1
}

func newIndex(s *Sums) *index {
	size := uint32(1 << 16)
	for size < uint32(len(s.Weak))*16 {
		size <<= 1
	}
	ix := &index{mask: size - 1, heads: make([]int32, size), next: make([]int32, len(s.Weak))}
	for i := range ix.heads {
		ix.heads[i] = -1
	}
	// Insert in reverse so chains list blocks in ascending order.
	for i := len(s.Weak) - 1; i >= 0; i-- {
		b := hashWeak(s.Weak[i]) & ix.mask
		ix.next[i] = ix.heads[b]
		ix.heads[b] = int32(i)
	}
	return ix
}

func hashWeak(w uint32) uint32 { return (w ^ w>>16) * 0x9e3779b1 >> 8 }

// Options adjust matching.
type Options struct {
	// Inplace is set when the receiver updates the basis file in place:
	// a block may then only be referenced at or before its own offset,
	// whose data the receiver has not yet overwritten, and a block at the
	// identical offset is preferred so the receiver can skip writing it.
	Inplace bool
}

// Match reads the new file from r and describes it to e in terms of the
// basis blocks summarized by s.
func Match(r io.Reader, s *Sums, strong StrongFunc, e Emitter) error {
	return MatchWith(r, s, strong, e, Options{})
}

// MatchWith is Match with options.
func MatchWith(r io.Reader, s *Sums, strong StrongFunc, e Emitter, opts Options) error {
	m := matcher{r: r, s: s, strong: strong, e: e, inplace: opts.Inplace}
	if s.Count() == 0 {
		return m.allLiteral()
	}
	return m.run()
}

type matcher struct {
	r      io.Reader
	s      *Sums
	strong StrongFunc
	e      Emitter

	inplace bool
	base    int64 // file offset of buf[0]

	buf []byte
	eof bool
	lit int // start of pending literal data in buf
	pos int // start of the current window in buf

	digest []byte
}

// bufPool recycles the matcher's buffers, which are large next to the
// small files that often make up most of a transfer.
var bufPool sync.Pool

func getBuf(n int) []byte {
	if b, ok := bufPool.Get().(*[]byte); ok && cap(*b) >= n {
		return (*b)[:n]
	}
	return make([]byte, n)
}

func putBuf(b []byte) { bufPool.Put(&b) }

func (m *matcher) allLiteral() error {
	buf := getBuf(readSize)
	defer putBuf(buf)
	for {
		n, err := io.ReadFull(m.r, buf)
		if n > 0 {
			if err := m.e.Literal(buf[:n]); err != nil {
				return err
			}
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// fill reads until buf holds at least need bytes or the input is exhausted.
// It may move data within buf, adjusting lit and pos.
func (m *matcher) fill(need int) error {
	for len(m.buf) < need && !m.eof {
		if cap(m.buf)-len(m.buf) < readSize {
			if m.lit > 0 {
				n := copy(m.buf, m.buf[m.lit:])
				m.buf = m.buf[:n]
				m.base += int64(m.lit)
				m.pos -= m.lit
				need -= m.lit
				m.lit = 0
			}
			if cap(m.buf)-len(m.buf) < readSize {
				nb := make([]byte, len(m.buf), 2*cap(m.buf)+readSize)
				copy(nb, m.buf)
				m.buf = nb
			}
		}
		// Read at most readSize so the lookahead stays bounded and
		// compaction above always makes room without growing the buffer.
		n, err := m.r.Read(m.buf[len(m.buf) : len(m.buf)+readSize])
		m.buf = m.buf[:len(m.buf)+n]
		if err == io.EOF {
			m.eof = true
		} else if err != nil {
			return err
		}
	}
	return nil
}

func (m *matcher) run() error {
	s := m.s
	bl := s.BlockLen
	ix := newIndex(s)
	m.buf = getBuf(bl + maxPending + 2*readSize)[:0]
	defer func() { putBuf(m.buf) }()

	var roll checksum.Roller
	want := 0 // the block after the last match; preferred to keep runs adjacent

	if err := m.fill(bl); err != nil {
		return err
	}
	k := min(bl, len(m.buf))
	roll.Reset(m.buf[:k])

	for k > 0 {
		sum := roll.Sum()
		if i := m.lookup(ix, sum, k, want); i >= 0 {
			if m.pos > m.lit {
				if err := m.e.Literal(m.buf[m.lit:m.pos]); err != nil {
					return err
				}
			}
			if err := m.e.Match(i, m.buf[m.pos:m.pos+k]); err != nil {
				return err
			}
			m.pos += k
			m.lit = m.pos
			want = i + 1
			if err := m.fill(m.pos + bl); err != nil {
				return err
			}
			k = min(bl, len(m.buf)-m.pos)
			roll.Reset(m.buf[m.pos : m.pos+k])
			continue
		}

		// Slide the window. While the full window fits in the buffer, roll
		// in a tight loop that only stops at a possible weak match.
		if m.pos-m.lit >= maxPending {
			if err := m.e.Literal(m.buf[m.lit:m.pos]); err != nil {
				return err
			}
			m.lit = m.pos
		}
		if err := m.fill(m.pos + k + 1); err != nil {
			return err
		}
		if m.pos+k >= len(m.buf) {
			roll.Shrink(m.buf[m.pos])
			k--
			m.pos++
			continue
		}
		m.pos = scan(&roll, m.buf, m.pos, k, min(len(m.buf)-k, m.lit+maxPending), ix)
	}
	if m.lit < len(m.buf) {
		return m.e.Literal(m.buf[m.lit:])
	}
	return nil
}

// scan rolls the k-byte window starting at pos forward one byte at a time
// until the window starts at end or its weak checksum hits a non-empty
// bucket of ix, and returns the new window start. pos+k < len(buf) on entry.
func scan(roll *checksum.Roller, buf []byte, pos, k, end int, ix *index) int {
	s1, s2 := roll.Parts()
	n := uint32(k)
	heads, mask := ix.heads, ix.mask
	for {
		o := uint32(int8(buf[pos]))
		s1 += uint32(int8(buf[pos+k])) - o
		s2 += s1 - n*o
		pos++
		if pos >= end || heads[hashWeak(s1&0xffff|s2<<16)&mask] >= 0 {
			break
		}
	}
	roll.SetParts(s1, s2)
	return pos
}

// lookup returns the block matching the k-byte window at pos, or -1.
func (m *matcher) lookup(ix *index, sum uint32, k, want int) int {
	s := m.s
	head := ix.heads[hashWeak(sum)&ix.mask]
	if head < 0 {
		return -1
	}
	var strong []byte
	check := func(i int) bool {
		if s.Weak[i] != sum || s.Len(i) != k {
			return false
		}
		if strong == nil {
			m.digest = m.strong(m.digest[:0], m.buf[m.pos:m.pos+k])
			strong = m.digest[:s.StrongLen]
		}
		return bytes.Equal(strong, s.StrongSum(i))
	}
	if m.inplace {
		// Blocks before the current offset may already be overwritten.
		pos := m.base + int64(m.pos)
		usable := func(i int) bool { return int64(i)*int64(s.BlockLen) >= pos }
		if pos%int64(s.BlockLen) == 0 {
			if aligned := int(pos / int64(s.BlockLen)); aligned < s.Count() && check(aligned) {
				return aligned
			}
		}
		if want < s.Count() && usable(want) && check(want) {
			return want
		}
		for i := head; i >= 0; i = ix.next[i] {
			if int(i) != want && usable(int(i)) && check(int(i)) {
				return int(i)
			}
		}
		return -1
	}
	if want < s.Count() && check(want) {
		return want
	}
	for i := head; i >= 0; i = ix.next[i] {
		if int(i) != want && check(int(i)) {
			return int(i)
		}
	}
	return -1
}
