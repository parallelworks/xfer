// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package rsync

import (
	"slices"
	"strings"
	"testing"
)

func TestReadFilesFrom(t *testing.T) {
	for _, tc := range []struct {
		in    string
		from0 bool
		want  []string
	}{
		{"a\n# comment\n; comment\n\nb c\r\nd/\n", false, []string{"a", "b c", "d/"}},
		{"a\nb", false, []string{"a", "b"}},
		{"a\x00#c\x00b\nc\x00\x00", true, []string{"a", "b\nc"}},
	} {
		got, err := readFilesFrom(strings.NewReader(tc.in), tc.from0)
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("readFilesFrom(%q, %v) = %q, %v; want %q", tc.in, tc.from0, got, err, tc.want)
		}
	}
}

func TestFilesFromRemote(t *testing.T) {
	for spec, want := range map[string]string{"host:list": "list", ":list": "list", "list": "", "-": "", "dir/a:b": ""} {
		got, ok := filesFromRemote(spec)
		if got != want || ok != (want != "") {
			t.Errorf("filesFromRemote(%q) = %q, %v", spec, got, ok)
		}
	}
}

func TestSkipCompress(t *testing.T) {
	sc := newSkipCompress(&Options{})
	for name, want := range map[string]bool{"a/b.GZ": true, "x.tar.gz": true, "notes.txt": false, "gz": false, "f.mp3": true} {
		if sc.skip(name) != want {
			t.Errorf("default list: skip(%q) = %v", name, !want)
		}
	}
	sc = newSkipCompress(&Options{SkipCompress: "txt/mp[34]"})
	for name, want := range map[string]bool{"a.txt": true, "b.mp4": true, "c.mp5": false, "d.gz": false} {
		if sc.skip(name) != want {
			t.Errorf("custom list: skip(%q) = %v", name, !want)
		}
	}
}
