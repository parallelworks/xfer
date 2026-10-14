// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

// Package checksum implements rsync's rolling block checksum and the strong
// checksum algorithms negotiated by the protocol.
package checksum

// Rolling computes rsync's weak "checksum1" of p: an Adler-32 variant over
// the bytes interpreted as signed values, returned as s1 | s2<<16.
func Rolling(p []byte) uint32 {
	s1, s2 := rollingParts(p)
	return s1&0xffff | s2<<16
}

func rollingParts(p []byte) (s1, s2 uint32) {
	if n := rollingVec(p); n > 0 {
		var w uint32
		s1, s2, w = rollingBlocks(p[:n])
		s2 = s2*32 + w
		p = p[n:]
	}
	return rollingTail(p, s1, s2)
}

// rollingTail continues the sums over p.
func rollingTail(p []byte, s1, s2 uint32) (uint32, uint32) {
	n := len(p)
	i := 0
	for ; i+4 < n; i += 4 {
		b0 := uint32(int8(p[i]))
		b1 := uint32(int8(p[i+1]))
		b2 := uint32(int8(p[i+2]))
		b3 := uint32(int8(p[i+3]))
		s2 += 4*(s1+b0) + 3*b1 + 2*b2 + b3
		s1 += b0 + b1 + b2 + b3
	}
	for ; i < n; i++ {
		s1 += uint32(int8(p[i]))
		s2 += s1
	}
	return s1, s2
}

// Roller maintains a rolling checksum over a sliding window.
type Roller struct {
	s1, s2 uint32
	n      uint32 // window length
}

// Reset starts a new window over p.
func (r *Roller) Reset(p []byte) {
	r.s1, r.s2 = rollingParts(p)
	r.n = uint32(len(p))
}

// Roll slides the window one byte: out leaves at the front, in enters at
// the back.
func (r *Roller) Roll(out, in byte) {
	o := uint32(int8(out))
	r.s1 += uint32(int8(in)) - o
	r.s2 += r.s1 - r.n*o
}

// Shrink drops out from the front of the window without adding a byte, as
// happens at the end of a file.
func (r *Roller) Shrink(out byte) {
	o := uint32(int8(out))
	r.s1 -= o
	r.s2 -= r.n * o
	r.n--
}

// Sum returns the checksum of the current window.
func (r *Roller) Sum() uint32 { return r.s1&0xffff | r.s2<<16 }

// Parts returns the window's running sums.
func (r *Roller) Parts() (s1, s2 uint32) { return r.s1, r.s2 }

// SetParts replaces the running sums, keeping the window length.
func (r *Roller) SetParts(s1, s2 uint32) { r.s1, r.s2 = s1, s2 }
