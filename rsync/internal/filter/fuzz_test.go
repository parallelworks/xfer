// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package filter

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
)

// fuzzReader serves data for every merge file, a bounded number of times so
// that self-including "." merges cannot expand exponentially.
func fuzzReader(data []byte, budget int) func(string) ([]byte, error) {
	return func(string) ([]byte, error) {
		if budget <= 0 {
			return nil, fs.ErrNotExist
		}
		budget--
		return data, nil
	}
}

// publicFlags are the flags a rule's text determines.
const publicFlags = FlagClear<<1 - 1

// checkRoundTrip asserts that every parsed rule re-parses from its String()
// form into the same rule.
func checkRoundTrip(t *testing.T, rules []Rule) {
	t.Helper()
	for _, r := range rules {
		if r.Flags&FlagClear != 0 {
			if r.String() != "!" {
				t.Fatalf("clear rule String() = %q", r.String())
			}
			continue
		}
		if r.Flags&FlagMerge != 0 && r.Flags&FlagPerDirMerge == 0 {
			t.Fatalf("unexpanded merge rule %q", r.String())
		}
		if r.Pattern == "" && !r.isCVSPlaceholder() {
			t.Fatalf("rule %#x has an empty pattern", r.Flags)
		}
		s := r.String()
		var p Parser
		p.ReadFile = func(string) ([]byte, error) { return nil, fs.ErrNotExist }
		got, err := p.Parse(s)
		if err != nil {
			t.Fatalf("rule %q (flags %#x pattern %q) does not re-parse: %v", s, r.Flags, r.Pattern, err)
		}
		if len(got) != 1 || got[0].Pattern != r.Pattern || got[0].Flags&publicFlags != r.Flags&publicFlags {
			t.Fatalf("rule %q (flags %#x pattern %q) re-parses as %v", s, r.Flags, r.Pattern, got)
		}
	}
}

// FuzzParse parses arbitrary rule text in every syntax the package accepts,
// then evaluates a fuzzed path against the resulting list, entering each of
// its directories so that per-directory merge files are read. Nothing may
// panic, every rule must survive a String round trip, and the rules a
// client would send must parse on the server.
func FuzzParse(f *testing.F) {
	for _, s := range []string{
		"- *.o", "+ /foo/***", ": /.rsync-filter", "dir-merge,n- .gitignore",
		"P foo", "H,p bar", "+ dir/", "-s x", "-r y", "-p z", "-! */", "-/ /abs",
		"-x user.*", ":C", "-C", "!", "clear", "exclude,s foo", "include **/b/*",
		"merge,w+ list", ". inc", ":e .per-dir", "show,x a?b", "risk [a-z]*",
		"protect /keep/**", "hide,_x", "+ - x", "- [[:alpha:]]*", "- a\\*b",
	} {
		f.Add(s, []byte("- *.tmp\n+ keep/\n: .sub\n!\n"), "a/b/c.o", uint8(0))
	}
	f.Add("- *", []byte("foo bar\n!baz"), "x/y", uint8(1))
	f.Add(": /.rsync-filter", []byte("- *.o\n: /.rsync-filter\n-/ /etc\n"), "src/x.o", uint8(2))
	f.Add("dir-merge,C .cvsignore", []byte("*.o core !\n"), "foo.o", uint8(3))
	f.Add("merge,- list", []byte("a\x00b\x00"), "a", uint8(7))
	f.Add("- foo", []byte{}, "foo", uint8(8))
	f.Add("+ bar/", []byte("- x\n+ y/\n!\n# c\n; c\r\n- z"), "bar", uint8(18))
	f.Add(":n- .gitignore", []byte("*.o\n/build/\n"), "a/build", uint8(24))
	f.Add(":C", []byte("*.o core\t!  x"), "core", uint8(26))
	f.Fuzz(func(t *testing.T, rule string, data []byte, name string, mode uint8) {
		if len(rule) > 1<<12 || len(data) > 1<<14 || len(name) > 1<<12 {
			return
		}
		p := Parser{
			ReadFile:       fuzzReader(data, 16),
			DeleteExcluded: mode&1 != 0,
			From0:          mode&4 != 0,
		}
		var rules []Rule
		switch mode >> 3 % 4 {
		case 0:
			rs, err := p.Parse(rule)
			if err == nil {
				rules = rs
			}
		case 1:
			rs, err := p.ParseOld(rule, mode&2 != 0)
			if err == nil {
				rules = rs
			}
		case 2:
			rs, err := p.ParseFile(data, mode&2 != 0)
			if err == nil {
				rules = rs
			}
		default:
			tmpl, err := p.Parse(rule)
			if err == nil && len(tmpl) == 1 && tmpl[0].Flags&FlagPerDirMerge != 0 {
				if rs, err := p.ParseMergeData(data, tmpl[0]); err == nil {
					rules = rs
				}
			}
		}
		checkRoundTrip(t, rules)

		// Wire rules are parsed by the server with the same protocol.
		for _, proto := range []int{27, 29, 31} {
			l, err := NewListWithOptions(rules, Options{
				ReadFile: func(string) ([]byte, error) { return nil, fs.ErrNotExist },
			})
			if err != nil {
				continue
			}
			ws, err := l.WireRules(proto, mode&2 != 0, mode&1 != 0)
			if err != nil {
				if !errors.Is(err, ErrTooModern) {
					t.Fatalf("WireRules(%d): %v", proto, err)
				}
				continue
			}
			for _, w := range ws {
				var sp Parser
				sp.ReadFile = func(string) ([]byte, error) { return nil, fs.ErrNotExist }
				if _, err := sp.ParseWire(w, proto); err != nil {
					t.Fatalf("wire rule %q (proto %d) does not parse: %v", w, proto, err)
				}
			}
		}

		l, err := NewListWithOptions(rules, Options{
			AmSender:       mode&2 != 0,
			DeleteExcluded: mode&1 != 0,
			Cwd:            "/src",
			ReadFile:       fuzzReader(data, 16),
			From0:          mode&4 != 0,
		})
		if err != nil {
			return
		}
		name = strings.Trim(name, "/")
		var leaves []func()
		dir := ""
		budget := 16
		for i, c := range strings.Split(name, "/") {
			if i == 32 {
				break
			}
			leave, _ := l.EnterDir(dir, func(string) ([]byte, error) {
				if budget <= 0 {
					return nil, fs.ErrNotExist
				}
				budget--
				return data, nil
			})
			leaves = append(leaves, leave)
			if dir != "" {
				dir += "/"
			}
			dir += c
			for _, side := range []Side{SenderSide, ReceiverSide} {
				for _, isDir := range []bool{false, true} {
					l.Match(dir, isDir, side, 0)
					l.Match(dir, isDir, side, IgnorePerishable)
					l.Match(c, isDir, side, XattrName)
				}
			}
		}
		for i := len(leaves) - 1; i >= 0; i-- {
			leaves[i]()
		}
		WildMatch(rule, name)
	})
}
