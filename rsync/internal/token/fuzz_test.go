// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package token

import (
	"bytes"
	"testing"

	"github.com/parallelworks/xfer/rsync/internal/wire"
)

// FuzzReader decodes arbitrary token streams with every encoding. It
// checks that malformed input fails cleanly rather than panicking,
// looping or growing without bound.
func FuzzReader(f *testing.F) {
	basis := bytes.Repeat([]byte("basis block "), 64)
	for k := Simple; k <= LZ4; k++ {
		var buf bytes.Buffer
		w := wire.NewWriter(&buf)
		tw := NewWriter(k, w, 31, DefaultLevel(k))
		tw.Literal([]byte("hello, hello, hello world"))
		tw.Match(0, basis)
		tw.Literal(bytes.Repeat([]byte{0}, 5000))
		tw.Match(3, basis)
		tw.End()
		tw.Literal([]byte("second file"))
		tw.End()
		w.Flush()
		if c, ok := tw.(interface{ Close() error }); ok {
			c.Close()
		}
		f.Add(uint8(k), buf.Bytes())
	}
	f.Fuzz(func(t *testing.T, kind uint8, data []byte) {
		k := Kind(kind % uint8(LZ4+1))
		r := NewReader(k, wire.NewReader(bytes.NewReader(data)), 31)
		defer r.Close()
		total := 0
		for range 10000 {
			lit, block, _, err := r.Next()
			if err != nil {
				return
			}
			if lit == nil && block >= 0 {
				r.SeeMatch(basis)
			}
			// Decompression can expand input enormously; stop at a bound
			// well above what any seed produces.
			if total += len(lit); total > 64<<20 {
				return
			}
		}
	})
}
