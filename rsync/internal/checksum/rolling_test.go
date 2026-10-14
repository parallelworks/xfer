// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package checksum

import (
	"fmt"
	"math/rand/v2"
	"testing"
)

// rollingRef is the textbook definition: s1 = Σ b[i], s2 = Σ (n-i)·b[i].
func rollingRef(p []byte) uint32 {
	var s1, s2 uint32
	n := uint32(len(p))
	for i, b := range p {
		v := uint32(int8(b))
		s1 += v
		s2 += (n - uint32(i)) * v
	}
	return s1&0xffff | s2<<16
}

func TestRollingMatchesDefinition(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for n := range 70 {
		p := make([]byte, n)
		for i := range p {
			p[i] = byte(r.UintN(256))
		}
		if got, want := Rolling(p), rollingRef(p); got != want {
			t.Fatalf("len %d: Rolling = %#x, want %#x", n, got, want)
		}
	}
}

func TestRollerRollAndShrink(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	data := make([]byte, 4096)
	for i := range data {
		data[i] = byte(r.UintN(256))
	}
	const w = 700
	var ro Roller
	ro.Reset(data[:w])
	for i := 1; i+w <= len(data); i++ {
		ro.Roll(data[i-1], data[i+w-1])
		if got, want := ro.Sum(), Rolling(data[i:i+w]); got != want {
			t.Fatalf("offset %d: rolled %#x, want %#x", i, got, want)
		}
	}
	start := len(data) - w
	for i := start; i < len(data)-1; i++ {
		ro.Shrink(data[i])
		if got, want := ro.Sum(), Rolling(data[i+1:]); got != want {
			t.Fatalf("shrink at %d: %#x, want %#x", i, got, want)
		}
	}
}

func BenchmarkRolling(b *testing.B) {
	p := make([]byte, 64<<10)
	b.SetBytes(int64(len(p)))
	for b.Loop() {
		Rolling(p)
	}
}

func TestMD4OldFileSumKeepsSeedAcrossReset(t *testing.T) {
	// From the transfer spec's worked example: protocol 29 file sum is
	// MD4(seedLE || data) with seed 1234567890.
	h := NewFile(MD4Old, 1234567890)
	h.Write([]byte("garbage"))
	h.Reset()
	h.Write([]byte("ABCDxyEFGHIJ"))
	if got := fmt.Sprintf("%x", h.Sum(nil)); got != "16b339cbe32a50a884466e28743da864" {
		t.Errorf("file sum = %s", got)
	}
}
