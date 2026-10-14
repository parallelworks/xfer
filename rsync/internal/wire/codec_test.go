// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"bytes"
	"math/rand/v2"
	"testing"
)

func TestVarintVectors(t *testing.T) {
	for _, tc := range []struct {
		v    int32
		want []byte
	}{
		{0, []byte{0}},
		{0x7f, []byte{0x7f}},
		{0x80, []byte{0x80, 0x80}},
		{0x3fff, []byte{0xbf, 0xff}},
		{0x4000, []byte{0xc0, 0x00, 0x40}},
		{-1, []byte{0xf0, 0xff, 0xff, 0xff, 0xff}},
	} {
		got := AppendVarlong(nil, uint64(uint32(tc.v)), 4, 1)
		if !bytes.Equal(got, tc.want) {
			t.Errorf("varint(%#x) = % x, want % x", tc.v, got, tc.want)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 1))
	var buf bytes.Buffer
	for _, proto := range []int{29, 30, 31} {
		buf.Reset()
		w := NewWriter(&buf)
		w.Protocol = proto
		var ints []int32
		var longs []int64
		var ndxs []int32
		for i := range 2000 {
			v := int32(r.Uint32() >> r.UintN(32))
			if i%3 == 0 {
				v = -v
			}
			ints = append(ints, v)
			l := int64(r.Uint64() >> r.UintN(64))
			if i%5 == 0 {
				l = -l
			}
			longs = append(longs, l)
			n := int32(r.UintN(100000))
			switch i % 7 {
			case 0:
				n = NdxDone
			case 1:
				n = NdxFlistEOF
			case 2:
				n = NdxFlistOffset - int32(r.UintN(1000))
			}
			ndxs = append(ndxs, n)
		}
		for i := range ints {
			w.Varint(ints[i])
			w.Varlong(longs[i], 3)
			w.Varlong(longs[i]&(1<<48-1), 1+i%4)
			w.Longint(longs[i])
			w.Ndx(ndxs[i])
			w.Vstring(string(make([]byte, i%300)))
		}
		if err := w.Flush(); err != nil {
			t.Fatal(err)
		}
		rd := NewReader(&buf)
		rd.Protocol = proto
		for i := range ints {
			if got := rd.Varint(); got != ints[i] {
				t.Fatalf("varint %d: got %d want %d", i, got, ints[i])
			}
			if got := rd.Varlong(3); got != longs[i] {
				t.Fatalf("varlong %d: got %d want %d", i, got, longs[i])
			}
			if got := rd.Varlong(1 + i%4); got != longs[i]&(1<<48-1) {
				t.Fatalf("varlong(min %d) %d: got %d want %d", 1+i%4, i, got, longs[i]&(1<<48-1))
			}
			if got := rd.Longint(); got != longs[i] {
				t.Fatalf("longint %d: got %d want %d", i, got, longs[i])
			}
			if got := rd.Ndx(); got != ndxs[i] {
				t.Fatalf("proto %d ndx %d: got %d want %d", proto, i, got, ndxs[i])
			}
			if got := rd.Vstring(1 << 15); len(got) != i%300 {
				t.Fatalf("vstring %d: len %d", i, len(got))
			}
		}
		if rd.Err() != nil {
			t.Fatal(rd.Err())
		}
	}
}

func TestMux(t *testing.T) {
	var conn bytes.Buffer
	out := NewOut(&conn)
	out.W.Int32(42) // before multiplexing: raw
	out.W.Flush()
	out.Mux.Enable()
	out.W.Bytes([]byte("hello"))
	out.SendMsg(MsgInfo, []byte("note\n"))
	out.W.Bytes([]byte(" world"))
	out.W.Flush()

	d := NewDemux(&conn)
	var msgs []string
	d.Handle = func(tag byte, p []byte) error {
		msgs = append(msgs, string(p))
		return nil
	}
	r := NewReader(d)
	if v := r.Int32(); v != 42 {
		t.Fatalf("raw int = %d", v)
	}
	d.Enable()
	p := make([]byte, 11)
	r.ReadFull(p)
	if string(p) != "hello world" || r.Err() != nil {
		t.Fatalf("data = %q, err %v", p, r.Err())
	}
	if len(msgs) != 1 || msgs[0] != "note\n" {
		t.Fatalf("msgs = %q", msgs)
	}
}
