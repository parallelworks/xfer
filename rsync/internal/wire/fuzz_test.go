// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"bytes"
	"testing"
)

// FuzzReader decodes arbitrary multiplexed input with every primitive.
func FuzzReader(f *testing.F) {
	f.Add([]byte{0x07, 0, 0, 0}, true)
	f.Add([]byte{5, 0, 0, 7, 'h', 'e', 'l', 'l', 'o'}, true)
	f.Add([]byte{0xfe, 0x80, 1, 2, 3, 0xff, 0x01}, false)
	f.Fuzz(func(t *testing.T, data []byte, mux bool) {
		d := NewDemux(bytes.NewReader(data))
		d.Handle = func(byte, []byte) error { return nil }
		if mux {
			d.Enable()
		}
		r := NewReader(d)
		r.Protocol = 31
		for i := 0; r.Err() == nil && i < 1000; i++ {
			switch i % 7 {
			case 0:
				r.Varint()
			case 1:
				r.Varlong(3)
			case 2:
				r.Ndx()
			case 3:
				r.Vstring(1 << 15)
			case 4:
				r.Longint()
			case 5:
				r.Int16()
			default:
				r.Byte()
			}
		}
	})
}
