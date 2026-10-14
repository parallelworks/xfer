// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package flist

import (
	"bytes"
	"encoding/hex"
	"slices"
	"strings"
	"testing"

	"github.com/parallelworks/xfer/rsync/internal/wire"
)

func exampleFiles() []*File {
	return []*File{
		{Name: ".", Mode: TypeDir | 0o755, Size: 4096, ModTime: 1700000000, UID: 1000, GID: 1000, Flags: FlagTopDir | FlagContentDir},
		{Name: "hello.txt", Mode: TypeReg | 0o644, Size: 12, ModTime: 1700000100, ModNsec: 123456789, UID: 1000, GID: 1000},
		{Name: "link", Mode: TypeLink | 0o777, Size: 9, ModTime: 1700000200, UID: 1000, GID: 1000, Link: "hello.txt"},
	}
}

func exampleConfig(proto int, varint bool) Config {
	return Config{Protocol: proto, VarintFlags: varint, SafeFlist: proto >= 31,
		PreserveUID: true, PreserveGID: true, PreserveLinks: true, PreserveDevices: true, PreserveSpecials: true}
}

func unhex(s string) []byte {
	b, err := hex.DecodeString(strings.Join(strings.Fields(s), ""))
	if err != nil {
		panic(err)
	}
	return b
}

// The expected bytes are the worked examples of the file-list spec.
func TestEncodeExamples(t *testing.T) {
	names := func(id uint32) string { return map[uint32]string{1000: "alice"}[id] }
	groups := func(id uint32) string { return map[uint32]string{1000: "users"}[id] }
	for _, tc := range []struct {
		proto  int
		varint bool
		want   string
	}{
		{31, false, `01 01 2E 00 00 10 65 00 F1 53 ED 41 00 00 83 E8 83 E8
			1C 20 09 68656C6C6F2E747874 00 0C 00 65 64 F1 53 E7 15 CD 5B A4 81 00 00
			18 04 6C696E6B 00 09 00 65 C8 F1 53 FF A1 00 00 09 68656C6C6F2E747874
			00 83E8 05 616C696365 00 83E8 05 7573657273 00`},
		{31, true, `01 01 2E 00 00 10 65 00 F1 53 ED 41 00 00 83 E8 83 E8
			A0 18 09 68656C6C6F2E747874 00 0C 00 65 64 F1 53 E7 15 CD 5B A4 81 00 00
			18 04 6C696E6B 00 09 00 65 C8 F1 53 FF A1 00 00 09 68656C6C6F2E747874
			00 00 83E8 05 616C696365 00 83E8 05 7573657273 00`},
		{29, false, `01 01 2E 00 10 00 00 00 F1 53 65 ED 41 00 00 E8 03 00 00 E8 03 00 00
			18 09 68656C6C6F2E747874 0C 00 00 00 64 F1 53 65 A4 81 00 00
			18 04 6C696E6B 09 00 00 00 C8 F1 53 65 FF A1 00 00 09 00 00 00 68656C6C6F2E747874
			00 E8030000 05 616C696365 00000000 E8030000 05 7573657273 00000000 00000000`},
	} {
		var buf bytes.Buffer
		w := wire.NewWriter(&buf)
		w.Protocol = tc.proto
		enc := NewEncoder(w, exampleConfig(tc.proto, tc.varint))
		enc.LookupUser, enc.LookupGroup = names, groups
		for _, f := range exampleFiles() {
			if err := enc.Encode(f); err != nil {
				t.Fatal(err)
			}
		}
		enc.End(0)
		w.Flush()
		if want := unhex(tc.want); !bytes.Equal(buf.Bytes(), want) {
			t.Errorf("proto %d varint %v:\n got % x\nwant % x", tc.proto, tc.varint, buf.Bytes(), want)
		}

		r := wire.NewReader(bytes.NewReader(buf.Bytes()))
		r.Protocol = tc.proto
		dec := NewDecoder(r, exampleConfig(tc.proto, tc.varint))
		var got []*File
		for {
			f, _, err := dec.Decode()
			if err != nil {
				t.Fatal(err)
			}
			if f == nil {
				break
			}
			got = append(got, f)
		}
		if _, err := dec.ReadTrailer(); err != nil {
			t.Fatal(err)
		}
		for i, want := range exampleFiles() {
			g := got[i]
			if tc.proto < 31 {
				want.ModNsec = 0
			}
			if g.Name != want.Name || g.Mode != want.Mode || g.Size != want.Size || g.ModTime != want.ModTime ||
				g.ModNsec != want.ModNsec || g.UID != want.UID || g.GID != want.GID || g.Link != want.Link {
				t.Errorf("proto %d entry %d: got %+v want %+v", tc.proto, i, g, want)
			}
		}
		if dec.Users[1000] != "alice" || dec.Groups[1000] != "users" {
			t.Errorf("id lists: %v %v", dec.Users, dec.Groups)
		}
	}
}

func TestSortOrder(t *testing.T) {
	mk := func(name string, dir bool) *File {
		m := uint32(TypeReg)
		if dir {
			m = TypeDir
		}
		return &File{Name: name, Mode: m}
	}
	files := []*File{
		mk("b", true), mk("b/x", false), mk("b-c", true), mk("z", false), mk("a", true),
		mk(".", true), mk("a/f", false), mk("b/sub", true), mk("b/sub/q", false), mk("b/y", false),
	}
	Sort(files, 31)
	var got []string
	for _, f := range files {
		got = append(got, f.Name)
	}
	want := []string{".", "z", "a", "a/f", "b-c", "b", "b/x", "b/y", "b/sub", "b/sub/q"}
	if !slices.Equal(got, want) {
		t.Errorf("sorted = %q\nwant     %q", got, want)
	}
}

// TestIncRecurseNames round-trips two lists with inline user and group
// names, as incremental recursion sends them.
func TestIncRecurseNames(t *testing.T) {
	for _, varint := range []bool{false, true} {
		cfg := exampleConfig(31, varint)
		cfg.IncRecurse = true
		var buf bytes.Buffer
		w := wire.NewWriter(&buf)
		w.Protocol = 31
		enc := NewEncoder(w, cfg)
		enc.LookupUser = func(id uint32) string { return map[uint32]string{1000: "alice", 0: "root"}[id] }
		enc.LookupGroup = func(id uint32) string { return map[uint32]string{1000: "users", 20: "staff"}[id] }
		lists := [][]*File{exampleFiles(), {
			{Name: "d/x", Mode: TypeReg | 0o644, ModTime: 1, UID: 0, GID: 20},
			{Name: "d/y", Mode: TypeReg | 0o644, ModTime: 1, UID: 1000, GID: 20},
		}}
		start := int32(1)
		for _, l := range lists {
			enc.StartList(start)
			for _, f := range l {
				if err := enc.Encode(f); err != nil {
					t.Fatal(err)
				}
			}
			enc.End(0)
			start += int32(len(l)) + 1
		}
		w.Flush()

		r := wire.NewReader(bytes.NewReader(buf.Bytes()))
		r.Protocol = 31
		dec := NewDecoder(r, cfg)
		start = 1
		for li, l := range lists {
			dec.StartList(start)
			for i, want := range l {
				g, _, err := dec.Decode()
				if err != nil || g == nil {
					t.Fatalf("varint %v list %d entry %d: %v %v", varint, li, i, g, err)
				}
				if g.Name != want.Name || g.UID != want.UID || g.GID != want.GID {
					t.Errorf("varint %v list %d: got %+v want %+v", varint, li, g, want)
				}
			}
			if f, _, err := dec.Decode(); f != nil || err != nil {
				t.Fatalf("varint %v list %d: no end marker: %v %v", varint, li, f, err)
			}
			start += int32(len(l)) + 1
		}
		if dec.Users[1000] != "alice" || dec.Users[0] != "root" || dec.Groups[20] != "staff" || dec.Groups[1000] != "users" {
			t.Errorf("varint %v: names %v %v", varint, dec.Users, dec.Groups)
		}
	}
}
