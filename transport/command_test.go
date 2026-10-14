// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package transport

import (
	"os/exec"
	"slices"
	"testing"
)

func TestQuoteRoundTripsThroughShell(t *testing.T) {
	args := []string{"plain", "with space", "it's", `back\slash`, "$HOME", "a;b|c&d", "", "tab\there", "new\nline", "~user", "(x)", `"q"`}
	cmd := exec.Command("sh", "-c", `for a in "$@"; do printf '%s\0' "$a"; done`, "sh")
	line := "printf '%s\\0' " + QuoteCommand(args)
	out, err := exec.Command("sh", "-c", line).Output()
	_ = cmd
	if err != nil {
		t.Fatal(err)
	}
	got := splitNul(string(out))
	if !slices.Equal(got, args) {
		t.Fatalf("got %q\nwant %q", got, args)
	}
}

func TestQuoteKeepsWildcards(t *testing.T) {
	if got := Quote("dir/*.txt"); got != "dir/*.txt" {
		t.Errorf("Quote = %q", got)
	}
}

func TestParseShell(t *testing.T) {
	got, err := ParseShell(`ssh -p 2222 -o "ProxyCommand=pw ssh --proxy %h" 'a b'`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ssh", "-p", "2222", "-o", "ProxyCommand=pw ssh --proxy %h", "a b"}
	if !slices.Equal(got, want) {
		t.Errorf("got %q want %q", got, want)
	}
}

func splitNul(s string) []string {
	var out []string
	cur := ""
	for _, c := range s {
		if c == 0 {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(c)
	}
	return out
}
