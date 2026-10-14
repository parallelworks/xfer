// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package token

import "github.com/parallelworks/xfer/rsync/internal/wire"

// Simple (uncompressed) token stream: a sequence of int32 values. t > 0 is
// a literal of t bytes that follow, t < 0 matches block -(t+1), 0 ends the
// file.

type simpleWriter struct {
	w *wire.Writer
}

func (s *simpleWriter) Literal(p []byte) error {
	for len(p) > 0 {
		n := min(len(p), ChunkSize)
		s.w.Int32(int32(n))
		s.w.Write(p[:n])
		p = p[n:]
	}
	return s.w.Err()
}

func (s *simpleWriter) Match(block int32, _ []byte) error {
	if block < 0 {
		return errBadBlock
	}
	s.w.Int32(-(block + 1))
	return s.w.Err()
}

func (s *simpleWriter) End() error {
	s.w.Int32(0)
	return s.w.Err()
}

type simpleReader struct {
	r    *wire.Reader
	left int // literal bytes of the current record still to deliver
	buf  []byte
}

func (s *simpleReader) Next() ([]byte, int32, bool, error) {
	if s.left == 0 {
		t := s.r.Int32()
		if err := s.r.Err(); err != nil {
			return nil, -1, false, err
		}
		switch {
		case t == 0:
			return nil, -1, true, nil
		case t < 0:
			return nil, -(t + 1), false, nil
		}
		s.left = int(t)
	}
	// Receivers must accept records longer than ChunkSize; deliver them
	// in ChunkSize pieces.
	n := min(s.left, ChunkSize)
	if cap(s.buf) < n {
		s.buf = make([]byte, ChunkSize)
	}
	b := s.buf[:n]
	s.r.ReadFull(b)
	if err := s.r.Err(); err != nil {
		return nil, -1, false, err
	}
	s.left -= n
	return b, -1, false, nil
}

func (s *simpleReader) SeeMatch([]byte) {}
func (s *simpleReader) Close() error    { return nil }
