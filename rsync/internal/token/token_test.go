// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package token

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand/v2"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/parallelworks/xfer/rsync/internal/wire"
)

var allKinds = []Kind{Simple, Zlib, Zlibx, Zstd, LZ4}

func unhex(t testing.TB, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.Join(strings.Fields(s), ""))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// op is one token: a literal (lit != nil) or a block reference.
type op struct {
	lit   []byte
	block int32
}

func encode(t testing.TB, w Writer, ops []op, blocks [][]byte) {
	t.Helper()
	for _, o := range ops {
		var err error
		if o.lit != nil {
			err = w.Literal(o.lit)
		} else {
			err = w.Match(o.block, blocks[o.block])
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := w.End(); err != nil {
		t.Fatal(err)
	}
}

func expected(ops []op, blocks [][]byte) []byte {
	var b []byte
	for _, o := range ops {
		if o.lit != nil {
			b = append(b, o.lit...)
		} else {
			b = append(b, blocks[o.block]...)
		}
	}
	return b
}

// decodeFile reads one file's tokens and reconstructs it from blocks.
func decodeFile(t testing.TB, r Reader, blocks [][]byte) ([]byte, []int32) {
	t.Helper()
	var out []byte
	var toks []int32
	for {
		lit, blk, done, err := r.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if done {
			return out, toks
		}
		if lit != nil {
			if blk != -1 {
				t.Fatalf("literal with block %d", blk)
			}
			out = append(out, lit...)
			continue
		}
		if blk < 0 || int(blk) >= len(blocks) {
			t.Fatalf("block %d out of range", blk)
		}
		toks = append(toks, blk)
		r.SeeMatch(blocks[blk])
		out = append(out, blocks[blk]...)
	}
}

func TestSpecExampleSimple(t *testing.T) {
	blocks := [][]byte{[]byte("ABCD"), []byte("EFGH"), []byte("IJ")}
	ops := []op{{block: 0}, {lit: []byte("xy")}, {block: 1}, {block: 2}}
	want := unhex(t, `ff ff ff ff  02 00 00 00 78 79  fe ff ff ff  fd ff ff ff  00 00 00 00`)
	var buf bytes.Buffer
	w := wire.NewWriter(&buf)
	encode(t, NewWriter(Simple, w, 31, 0), ops, blocks)
	w.Flush()
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("got  % x\nwant % x", buf.Bytes(), want)
	}
	r := NewReader(Simple, wire.NewReader(bytes.NewReader(want)), 31)
	got, toks := decodeFile(t, r, [][]byte{[]byte("ABCD"), []byte("EFGH"), []byte("IJ")})
	if string(got) != "ABCDxyEFGHIJ" || fmt.Sprint(toks) != "[0 1 2]" {
		t.Fatalf("decoded %q %v", got, toks)
	}
}

func TestSpecExampleZlib(t *testing.T) {
	blocks := [][]byte{[]byte("ABCD"), []byte("EFGH"), []byte("IJ")}
	ops := []op{{block: 0}, {lit: []byte("xy")}, {block: 1}, {block: 2}}
	// From the spec: TOKEN_REL 0, DEFLATED_DATA "xy", TOKENRUN_REL 1 n=1, END.
	want := unhex(t, `80  40 04 aa a8 04 00  c1 01 00  00`)

	// Level 9 happens to produce zlib's exact bytes for this input.
	var buf bytes.Buffer
	w := wire.NewWriter(&buf)
	encode(t, NewWriter(Zlib, w, 31, 9), ops, blocks)
	w.Flush()
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("got  % x\nwant % x", buf.Bytes(), want)
	}

	// The default level may choose a different deflate block type, but the
	// framing must be identical.
	buf.Reset()
	encode(t, NewWriter(Zlib, w, 31, 0), ops, blocks)
	w.Flush()
	b := buf.Bytes()
	if b[0] != 0x80 || b[1]&0xC0 != 0x40 || !bytes.HasSuffix(b, []byte{0xc1, 0x01, 0x00, 0x00}) {
		t.Fatalf("default level framing: % x", b)
	}

	for _, in := range [][]byte{want, b} {
		for _, k := range []Kind{Zlib, Zlibx} {
			r := NewReader(k, wire.NewReader(bytes.NewReader(in)), 31)
			got, toks := decodeFile(t, r, blocks)
			if string(got) != "ABCDxyEFGHIJ" || fmt.Sprint(toks) != "[0 1 2]" {
				t.Fatalf("%v decoded %q %v", k, got, toks)
			}
		}
	}
}

// TestRunEncoding checks run coalescing and REL/LONG selection.
func TestRunEncoding(t *testing.T) {
	blocks := make([][]byte, 70000)
	for i := range blocks {
		blocks[i] = []byte{byte(i)}
	}
	var ops []op
	add := func(bs ...int32) {
		for _, b := range bs {
			ops = append(ops, op{block: b})
		}
	}
	add(5)       // REL r=5
	add(6, 7, 8) // extends the run: 5..8 → c5 03 00
	add(100)     // LONG (r = 92)
	add(3)       // LONG (negative r)
	add(4)       // run 3..4 → 21 03000000 0100
	ops = append(ops, op{lit: []byte("z")})
	add(4) // REL r=0 after literal (last = 4)
	for i := int32(0); i < 65537; i++ {
		add(i) // run broken at 65536 blocks
	}
	var buf bytes.Buffer
	w := wire.NewWriter(&buf)
	encode(t, NewWriter(Zlibx, w, 31, 0), ops, blocks)
	w.Flush()
	b := buf.Bytes()
	wantPrefix := unhex(t, `c5 03 00  20 64 00 00 00  21 03 00 00 00 01 00`)
	if !bytes.HasPrefix(b, wantPrefix) {
		t.Fatalf("prefix % x", b[:20])
	}
	r := NewReader(Zlibx, wire.NewReader(bytes.NewReader(b)), 31)
	got, toks := decodeFile(t, r, blocks)
	if !bytes.Equal(got, expected(ops, blocks)) {
		t.Fatal("mismatch")
	}
	if len(toks) != len(ops)-1 {
		t.Fatalf("got %d tokens", len(toks))
	}
	// Last bytes: run 0..65535 written as LONG (r=-4) with n=65535, then
	// block 65536 as REL r=1, then END.
	wantEnd := unhex(t, `80  21 00 00 00 00 ff ff  81  00`)
	if !bytes.HasSuffix(b, wantEnd) {
		t.Fatalf("suffix % x", b[len(b)-12:])
	}
}

// genData returns n bytes mixing compressible text and random bytes.
func genData(rng *rand.Rand, n int) []byte {
	words := []string{"rsync ", "token ", "stream ", "delta ", "block ", "\n", "zlib ", "zstd "}
	b := make([]byte, 0, n)
	for len(b) < n {
		if rng.IntN(4) == 0 {
			k := min(n-len(b), 1+rng.IntN(64))
			for range k {
				b = append(b, byte(rng.Uint32()))
			}
		} else {
			b = append(b, words[rng.IntN(len(words))]...)
		}
	}
	return b[:n]
}

type fileCase struct {
	blocks [][]byte
	ops    []op
}

func genFile(rng *rand.Rand, blockLen, nblocks, nops, maxLit int) fileCase {
	var fc fileCase
	basis := genData(rng, blockLen*nblocks)
	for i := range nblocks {
		l := blockLen
		if i == nblocks-1 {
			l = 1 + rng.IntN(blockLen) // remainder block
		}
		fc.blocks = append(fc.blocks, basis[i*blockLen:i*blockLen+l])
	}
	last := int32(-1)
	for range nops {
		switch rng.IntN(5) {
		case 0, 1:
			fc.ops = append(fc.ops, op{lit: genData(rng, 1+rng.IntN(maxLit))})
		case 2:
			// consecutive run
			for range 1 + rng.IntN(10) {
				last = (last + 1) % int32(nblocks)
				fc.ops = append(fc.ops, op{block: last})
			}
		default:
			last = int32(rng.IntN(nblocks))
			fc.ops = append(fc.ops, op{block: last})
		}
	}
	return fc
}

// roundTrip encodes the files as one session, with a trailer after every
// file's stream, and decodes them again, checking that the reader leaves
// the trailer untouched.
func roundTrip(t *testing.T, k Kind, proto, level int, files []fileCase) {
	t.Helper()
	var buf bytes.Buffer
	w := wire.NewWriter(&buf)
	tw := NewWriter(k, w, proto, level)
	for i, f := range files {
		encode(t, tw, f.ops, f.blocks)
		w.Int32(int32(0x7EADBEEF - i)) // trailer: e.g. the file checksum
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	wr := wire.NewReader(bytes.NewReader(buf.Bytes()))
	tr := NewReader(k, wr, proto)
	defer tr.Close()
	for i, f := range files {
		got, _ := decodeFile(t, tr, f.blocks)
		want := expected(f.ops, f.blocks)
		if !bytes.Equal(got, want) {
			t.Fatalf("%v proto %d file %d: got %d bytes, want %d (first diff at %d)", k, proto, i, len(got), len(want), firstDiff(got, want))
		}
		if v := wr.Int32(); v != int32(0x7EADBEEF-i) || wr.Err() != nil {
			t.Fatalf("%v file %d: trailer %x err %v (reader overran its stream)", k, i, v, wr.Err())
		}
	}
	if _, err := wr.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("leftover data: %v", err)
	}
}

func firstDiff(a, b []byte) int {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}

func TestRoundTrip(t *testing.T) {
	for _, k := range allKinds {
		for _, proto := range []int{29, 30, 31} {
			t.Run(fmt.Sprintf("%v/%d", k, proto), func(t *testing.T) {
				rng := rand.New(rand.NewPCG(uint64(k), uint64(proto)))
				var files []fileCase
				for range 6 {
					files = append(files, genFile(rng, 700, 50, 60, 3000))
				}
				// An empty file and a literal-only / match-only file.
				files = append(files, fileCase{})
				files = append(files, fileCase{ops: []op{{lit: genData(rng, 100000)}}})
				f := genFile(rng, 512, 20, 0, 1)
				for i := range 20 {
					f.ops = append(f.ops, op{block: int32(i)})
				}
				files = append(files, f)
				roundTrip(t, k, proto, 0, files)
			})
		}
	}
}

func TestRoundTripLevels(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 7))
	files := []fileCase{genFile(rng, 1000, 30, 40, 5000), genFile(rng, 1000, 30, 40, 5000)}
	for _, k := range []Kind{Zlib, Zlibx, Zstd} {
		for _, level := range []int{1, 2, 5, 6, 7, 9, 19} {
			roundTrip(t, k, 31, level, files)
		}
	}
}

// TestRoundTripLarge covers literals far beyond the record sizes, many
// interleaved small segments, and blocks larger than 0xFFFF bytes (the
// proto < 31 zlib history quirk).
func TestRoundTripLarge(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	rng := rand.New(rand.NewPCG(1, 2))
	big := genFile(rng, 131072, 12, 40, 300000)
	rnd := make([]byte, 3<<20)
	for i := range rnd {
		rnd[i] = byte(rng.Uint32())
	}
	huge := fileCase{ops: []op{{lit: genData(rng, 5<<20)}, {lit: rnd}}}
	many := genFile(rng, 300, 400, 5000, 200)
	odd := genFile(rng, 65535+1000, 4, 20, 70000) // blocks just over 0xFFFF
	exact := genFile(rng, 2*0xFFFF, 3, 20, 1000)  // multiple of 0xFFFF
	for _, k := range allKinds {
		for _, proto := range []int{30, 31} {
			roundTrip(t, k, proto, 0, []fileCase{big, huge, many, odd, exact})
		}
	}
}

// TestSimpleLongRecord: receivers must accept literal records > ChunkSize.
func TestSimpleLongRecord(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789"), 10000)
	var buf bytes.Buffer
	w := wire.NewWriter(&buf)
	w.Int32(int32(len(data)))
	w.Write(data)
	w.Int32(-3)
	w.Int32(0)
	w.Flush()
	r := NewReader(Simple, wire.NewReader(&buf), 31)
	blocks := [][]byte{nil, nil, []byte("B")}
	got, toks := decodeFile(t, r, blocks)
	if !bytes.Equal(got, append(data, 'B')) || fmt.Sprint(toks) != "[2]" {
		t.Fatal("mismatch")
	}
}

func TestBadStreams(t *testing.T) {
	cases := map[string]string{
		"zero run":      "c0 00 00",
		"bad flag":      "05",
		"negative long": "20 ff ff ff ff",
		"overflow run":  "21 fe ff ff 7f 02 00",
		"truncated":     "40 05 aa",
	}
	for name, h := range cases {
		for _, k := range []Kind{Zlib, Zlibx, Zstd, LZ4} {
			r := NewReader(k, wire.NewReader(bytes.NewReader(unhex(t, h))), 31)
			var err error
			for range 10 {
				if _, _, _, err = r.Next(); err != nil {
					break
				}
			}
			if err == nil {
				t.Errorf("%s/%v: no error", name, k)
			}
			r.Close()
		}
	}
}

func TestLZ4Codec(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	var table [lz4TableEntry]int32
	for _, n := range []int{0, 1, 5, 12, 13, 20, 100, 1000, lz4ChunkMax} {
		for _, src := range [][]byte{genData(rng, n), bytes.Repeat([]byte{'a'}, n), func() []byte {
			b := make([]byte, n)
			for i := range b {
				b[i] = byte(rng.Uint32())
			}
			return b
		}()} {
			c := lz4Compress(nil, src, &table)
			if len(c) > MaxDataCount {
				t.Fatalf("n=%d: compressed %d", n, len(c))
			}
			d, err := lz4Decompress(make([]byte, 0, lz4OutMax), c)
			if err != nil || !bytes.Equal(d, src) {
				t.Fatalf("n=%d: %v", n, err)
			}
		}
	}
}

func benchLiteral(b *testing.B, k Kind, data []byte) {
	var buf bytes.Buffer
	w := wire.NewWriter(&buf)
	tw := NewWriter(k, w, 31, 0)
	b.Run("encode", func(b *testing.B) {
		b.SetBytes(int64(len(data)))
		for b.Loop() {
			buf.Reset()
			tw.Literal(data)
			tw.End()
			w.Flush()
		}
	})
	buf.Reset()
	tw = NewWriter(k, w, 31, 0) // fresh session: zstd output depends on history
	tw.Literal(data)
	tw.End()
	w.Flush()
	enc := bytes.Clone(buf.Bytes())
	b.Run("decode", func(b *testing.B) {
		b.SetBytes(int64(len(data)))
		for b.Loop() {
			// A fresh reader per iteration: a zstd stream spans the session.
			tr := NewReader(k, wire.NewReader(bytes.NewReader(enc)), 31)
			for {
				_, _, done, err := tr.Next()
				if err != nil {
					b.Fatal(err)
				}
				if done {
					break
				}
			}
			tr.Close()
		}
	})
	b.ReportMetric(float64(len(enc))/float64(len(data)), "ratio")
}

func BenchmarkLiteral(b *testing.B) {
	rng := rand.New(rand.NewPCG(1, 1))
	data := genData(rng, 8<<20)
	for _, k := range allKinds {
		b.Run(k.String(), func(b *testing.B) { benchLiteral(b, k, data) })
	}
}

// BenchmarkDelta measures a delta-heavy stream: short literals between
// 700-byte matched blocks.
func BenchmarkDelta(b *testing.B) {
	rng := rand.New(rand.NewPCG(2, 2))
	f := genFile(rng, 700, 2000, 4000, 200)
	size := len(expected(f.ops, f.blocks))
	for _, k := range allKinds {
		b.Run(k.String(), func(b *testing.B) {
			var buf bytes.Buffer
			w := wire.NewWriter(&buf)
			tw := NewWriter(k, w, 31, 0)
			b.SetBytes(int64(size))
			for b.Loop() {
				buf.Reset()
				encode(b, tw, f.ops, f.blocks)
				w.Flush()
			}
			var lits int
			for _, o := range f.ops {
				lits += len(o.lit)
			}
			b.ReportMetric(float64(buf.Len())/float64(lits), "ratio")
		})
	}
}

// TestZstdClose checks that Close stops the decoder goroutine, also while a
// decoded chunk is outstanding.
func TestZstdClose(t *testing.T) {
	var buf bytes.Buffer
	w := wire.NewWriter(&buf)
	tw := NewWriter(Zstd, w, 31, 0)
	encode(t, tw, []op{{lit: bytes.Repeat([]byte("abc"), 100000)}}, nil)
	w.Flush()
	before := runtime.NumGoroutine()
	for _, mid := range []bool{false, true} {
		r := NewReader(Zstd, wire.NewReader(bytes.NewReader(buf.Bytes())), 31)
		if mid {
			if lit, _, _, err := r.Next(); err != nil || lit == nil {
				t.Fatal(lit, err)
			}
		} else {
			decodeFile(t, r, nil)
		}
		r.Close()
		r.Close()
		if _, _, _, err := r.Next(); err == nil {
			t.Fatal("Next after Close succeeded")
		}
	}
	for i := 0; runtime.NumGoroutine() > before && i < 100; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before {
		t.Fatalf("goroutines: %d > %d", n, before)
	}
}
