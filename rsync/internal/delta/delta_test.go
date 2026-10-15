// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package delta

import (
	"bytes"
	"crypto/sha256"
	"io"
	"math/rand/v2"
	"testing"
)

// strongSum is the strong checksum for these tests. Any hash will do; a
// FIPS-approved one lets the package pass under GODEBUG=fips140=only.
func strongSum(dst, p []byte) []byte {
	h := sha256.Sum256(p)
	return append(dst, h[:16]...)
}

// rebuild applies a delta to basis.
type rebuild struct {
	basis   []byte
	s       *Sums
	out     bytes.Buffer
	matched int
	literal int
}

func (r *rebuild) Literal(p []byte) error {
	r.literal += len(p)
	r.out.Write(p)
	return nil
}

func (r *rebuild) Match(i int, data []byte) error {
	off := i * r.s.BlockLen
	blk := r.basis[off : off+r.s.Len(i)]
	if !bytes.Equal(blk, data) {
		panic("match data differs from block")
	}
	r.matched += len(blk)
	r.out.Write(blk)
	return nil
}

func roundTrip(t *testing.T, basis, target []byte, bl int) *rebuild {
	t.Helper()
	s, err := Generate(bytes.NewReader(basis), int64(len(basis)), bl, 16, strongSum)
	if err != nil {
		t.Fatal(err)
	}
	rb := &rebuild{basis: basis, s: s}
	// A reader that returns short reads exercises buffer management.
	if err := Match(&choppy{data: target}, s, strongSum, rb); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rb.out.Bytes(), target) {
		t.Fatalf("rebuilt %d bytes, differs from %d-byte target", rb.out.Len(), len(target))
	}
	return rb
}

type choppy struct {
	data []byte
	n    int
}

func (c *choppy) Read(p []byte) (int, error) {
	if len(c.data) == 0 {
		return 0, io.EOF
	}
	c.n++
	m := min(len(p), len(c.data), 1+c.n*7919%100000)
	copy(p, c.data[:m])
	c.data = c.data[m:]
	return m, nil
}

func randBytes(r *rand.Rand, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.UintN(256))
	}
	return b
}

func TestMatchIdentical(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 1))
	data := randBytes(r, 1<<20+123)
	rb := roundTrip(t, data, data, 700)
	if rb.literal != 0 {
		t.Errorf("identical file sent %d literal bytes", rb.literal)
	}
}

func TestMatchEdits(t *testing.T) {
	r := rand.New(rand.NewPCG(2, 2))
	basis := randBytes(r, 3<<20)
	target := append([]byte(nil), basis...)
	// Insert, delete and overwrite a few regions.
	target = append(target[:1000], append(randBytes(r, 333), target[1000:]...)...)
	target = append(target[:500000], target[520000:]...)
	copy(target[2<<20:], randBytes(r, 5000))
	target = append(randBytes(r, 17), target...)
	rb := roundTrip(t, basis, target, 2048)
	if rb.matched < len(basis)-60000 {
		t.Errorf("matched only %d of %d bytes", rb.matched, len(basis))
	}
}

func TestMatchSmallAndEmpty(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 3))
	for _, tc := range []struct{ basis, target int }{
		{0, 0}, {0, 10}, {10, 0}, {1, 1}, {699, 699}, {700, 700}, {701, 1401}, {5000, 3},
	} {
		basis := randBytes(r, tc.basis)
		target := randBytes(r, tc.target)
		if tc.target <= tc.basis {
			target = basis[:tc.target]
		}
		roundTrip(t, basis, target, 700)
	}
}

func TestMatchShortLastBlock(t *testing.T) {
	r := rand.New(rand.NewPCG(4, 4))
	basis := randBytes(r, 10*700+300)
	target := append(randBytes(r, 50), basis...)
	rb := roundTrip(t, basis, target, 700)
	if rb.literal != 50 {
		t.Errorf("literal = %d, want 50", rb.literal)
	}
}

func BenchmarkMatchIdentical(b *testing.B) {
	r := rand.New(rand.NewPCG(5, 5))
	data := randBytes(r, 16<<20)
	s, _ := Generate(bytes.NewReader(data), int64(len(data)), 4096, 16, strongSum)
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		Match(bytes.NewReader(data), s, strongSum, nopEmitter{})
	}
}

func BenchmarkMatchNoCommon(b *testing.B) {
	r := rand.New(rand.NewPCG(6, 6))
	basis := randBytes(r, 16<<20)
	data := randBytes(r, 16<<20)
	s, _ := Generate(bytes.NewReader(basis), int64(len(basis)), 4096, 16, strongSum)
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		Match(bytes.NewReader(data), s, strongSum, nopEmitter{})
	}
}

type nopEmitter struct{}

func (nopEmitter) Literal([]byte) error    { return nil }
func (nopEmitter) Match(int, []byte) error { return nil }

// TestMatchInplace applies the delta to the basis in place, the way an
// --inplace receiver does, so a reference to already-overwritten data
// corrupts the result.
func TestMatchInplace(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 7))
	basis := randBytes(r, 200*1000)
	for _, edit := range []func([]byte) []byte{
		func(b []byte) []byte { return append(randBytes(r, 1500), b...) },                     // shift forward
		func(b []byte) []byte { return b[3000:] },                                             // shift back
		func(b []byte) []byte { return append(append([]byte{}, b[50000:]...), b[:50000]...) }, // rotate
	} {
		target := edit(append([]byte(nil), basis...))
		s, _ := Generate(bytes.NewReader(basis), int64(len(basis)), 1000, 16, strongSum)
		file := append([]byte(nil), basis...)
		ip := &inplaceApply{file: file, s: s}
		if err := MatchWith(bytes.NewReader(target), s, strongSum, ip, Options{Inplace: true}); err != nil {
			t.Fatal(err)
		}
		if got := ip.file[:ip.off]; !bytes.Equal(got, target) {
			t.Fatalf("in-place result differs from target (matched %d bytes)", ip.matched)
		}
	}
}

type inplaceApply struct {
	file    []byte
	s       *Sums
	off     int
	matched int
}

func (a *inplaceApply) write(p []byte) {
	for a.off+len(p) > len(a.file) {
		a.file = append(a.file, 0)
	}
	copy(a.file[a.off:], p)
	a.off += len(p)
}

func (a *inplaceApply) Literal(p []byte) error { a.write(p); return nil }

func (a *inplaceApply) Match(i int, _ []byte) error {
	start := i * a.s.BlockLen
	blk := append([]byte(nil), a.file[start:start+a.s.Len(i)]...) // read the current file, as the receiver does
	a.matched += len(blk)
	a.write(blk)
	return nil
}

type discard struct{}

func (discard) Literal([]byte) error    { return nil }
func (discard) Match(int, []byte) error { return nil }

// TestMatchBoundedBuffer checks that matching a long, mostly matching file
// keeps the read buffer at its initial size.
func TestMatchBoundedBuffer(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 5))
	basis := randBytes(r, 16<<20)
	target := append([]byte(nil), basis...)
	copy(target[8<<20:], randBytes(r, 1<<20))
	s, _ := Generate(bytes.NewReader(basis), int64(len(basis)), 8192, 16, strongSum)
	m := matcher{r: bytes.NewReader(target), s: s, strong: strongSum, e: discard{}}
	if err := m.run(); err != nil {
		t.Fatal(err)
	}
	if limit := 8192 + maxPending + 2*readSize; cap(m.buf) > limit {
		t.Errorf("buffer grew to %d bytes, want at most %d", cap(m.buf), limit)
	}
}
