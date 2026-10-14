// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package flist

import (
	"bytes"
	"testing"

	"github.com/parallelworks/xfer/rsync/internal/wire"
)

// FuzzDecode feeds arbitrary bytes to the file-list decoder, which reads
// data from an untrusted peer. It must fail cleanly, never panic or
// allocate without bound.
func FuzzDecode(f *testing.F) {
	for _, tc := range []struct {
		proto  int
		varint bool
	}{{31, false}, {31, true}, {29, false}} {
		var buf bytes.Buffer
		w := wire.NewWriter(&buf)
		w.Protocol = tc.proto
		enc := NewEncoder(w, exampleConfig(tc.proto, tc.varint))
		for _, file := range exampleFiles() {
			enc.Encode(file)
		}
		enc.End(0)
		w.Flush()
		f.Add(buf.Bytes(), tc.proto, tc.varint)
	}
	f.Fuzz(func(t *testing.T, data []byte, proto int, varint bool) {
		proto = 27 + (proto%5+5)%5
		r := wire.NewReader(bytes.NewReader(data))
		r.Protocol = proto
		cfg := exampleConfig(proto, varint && proto >= 30)
		cfg.HardLinks, cfg.AlwaysChecksum, cfg.ChecksumLen = true, true, 16
		dec := NewDecoder(r, cfg)
		var files []*File
		for range 10000 {
			file, _, err := dec.Decode()
			if err != nil || file == nil {
				break
			}
			files = append(files, file)
		}
		dec.ReadTrailer()
		Sort(files, proto)
		Clean(files)
	})
}
