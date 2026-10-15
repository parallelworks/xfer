// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package rsync

import (
	"fmt"
	"testing"
)

func TestRsyncQuote(t *testing.T) {
	for _, c := range []struct {
		format string
		args   []any
		want   string
	}{
		{"open %q failed: %s", []any{`a "b"\c`, "ENOENT"}, `open "a "b"\c" failed: ENOENT`},
		{"x %q", []any{"new\nline\xff"}, `x "new\#012line\#377"`},
		{"ünï %q %d%%", []any{"ünïcødé", 3}, `ünï "ünïcødé" 3%`},
		{"%5d %q", []any{7, "n"}, `    7 "n"`},
		{"%q", []any{42}, "'*'"}, // not a name: Go's meaning
		{"%q %q", []any{"only one"}, `"only one" %!q(MISSING)`},
	} {
		f, a := rsyncQuote(c.format, c.args, escapeName)
		if got := fmt.Sprintf(f, a...); got != c.want {
			t.Errorf("%q %v: got %s, want %s", c.format, c.args, got, c.want)
		}
	}
}
