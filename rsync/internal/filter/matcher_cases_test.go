// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package filter

import (
	"strings"
	"testing"
)

// matcherCases are the evaluation vectors of the filter spec (§8.2), each
// verified against GNU rsync 3.5.0. Rules are "f:RULE" (--filter),
// "x:PAT" (--exclude), "i:PAT" (--include) or "C" (--cvs-exclude). The path
// is relative to the transfer root (it starts with "src/" when src is
// "src"); excluded means the path is absent from the transfer, possibly
// because an ancestor directory was excluded.
var matcherCases = []struct {
	id       int
	src      string
	rules    []string
	path     string
	isDir    bool
	excluded bool
}{
	{1, "src/", []string{`f:- foo`}, `foo`, false, true},
	{2, "src/", []string{`f:- foo`}, `bar/foo`, false, true},
	{3, "src/", []string{`f:- foo`}, `bar/deep/foo`, true, true},
	{4, "src/", []string{`f:- foo`}, `bar/deep/foo/x.txt`, false, true},
	{5, "src/", []string{`f:- foo/`}, `foo`, false, false},
	{6, "src/", []string{`f:- foo/`}, `sub/foo`, true, true},
	{7, "src/", []string{`f:- /foo`}, `foo`, false, true},
	{8, "src/", []string{`f:- /foo`}, `bar/foo`, false, false},
	{9, "src/", []string{`f:- bar/foo`}, `bar/foo`, false, true},
	{10, "src/", []string{`f:- deep/foo`}, `bar/deep/foo`, true, true},
	{11, "src/", []string{`f:- ar/foo`}, `bar/foo`, false, false},
	{12, "src/", []string{`f:- /deep/foo`}, `bar/deep/foo`, true, false},
	{13, "src/", []string{`f:- *.o`}, `sub/b.o`, false, true},
	{14, "src/", []string{`f:+ */`, `f:+ *.c`, `f:- *`}, `bar/baz.c`, false, false},
	{15, "src/", []string{`f:+ */`, `f:+ *.c`, `f:- *`}, `bar/foo`, false, true},
	{16, "src/", []string{`f:+ */`, `f:+ *.c`, `f:- *`}, `bar/deep`, true, false},
	{17, "src/", []string{`f:- b*/foo`}, `bar/foo`, false, true},
	{18, "src/", []string{`f:- b*/foo`}, `bar/deep/foo`, true, false},
	{19, "src/", []string{`f:- /b*`}, `bar`, true, true},
	{20, "src/", []string{`f:- /b*`}, `sub/b.o`, false, false},
	{21, "src/", []string{`f:- **/foo`}, `foo`, false, true},
	{22, "src/", []string{`f:- **/foo`}, `bar/deep/foo`, true, true},
	{23, "src/", []string{`f:- deep/**`}, `bar/deep`, true, false},
	{24, "src/", []string{`f:- deep/**`}, `bar/deep/bar`, false, true},
	{25, "src/", []string{`f:- /bar/**`}, `bar`, true, false},
	{26, "src/", []string{`f:- /bar/**`}, `bar/foo`, false, true},
	{27, "src/", []string{`f:- dir_name/***`}, `dir_name`, true, true},
	{28, "src/", []string{`f:+ dir_name/***`, `f:- *`}, `dir_name/sub2/f2`, false, false},
	{29, "src/", []string{`f:+ dir_name/***`, `f:- *`}, `file.txt`, false, true},
	{30, "src/", []string{`f:- fo?`}, `foo`, false, true},
	{31, "src/", []string{`f:- fo?`}, `Foo`, false, false},
	{32, "src/", []string{`f:- [Ff]oo`}, `Foo`, false, true},
	{33, "src/", []string{`f:- lit[1]`}, `lit[1]`, false, false},
	{34, "src/", []string{`f:- lit\[1]`}, `lit[1]`, false, true},
	{35, "src/", []string{`f:- back\slash`}, `back\slash`, false, true},
	{36, "src/", []string{`f:- back\slash*`}, `back\slash`, false, false},
	{37, "src/", []string{`f:- back\\slash*`}, `back\slash`, false, true},
	{38, "src/", []string{`f:-! */`}, `foo`, false, true},
	{39, "src/", []string{`f:-! */`}, `bar/deep/foo`, true, false},
	{40, "src/", []string{`f:+ foo`, `f:- foo`}, `foo`, false, false},
	{41, "src/", []string{`f:- bar/`, `f:+ bar/foo`}, `bar/foo`, false, true},
	{42, "src/", []string{`f:+ /bar/`, `f:+ /bar/foo`, `f:- *`}, `bar/foo`, false, false},
	{43, "src/", []string{`f:+ /bar/`, `f:+ /bar/foo`, `f:- *`}, `bar/baz.c`, false, true},
	{44, "src", []string{`f:- /foo`}, `src/foo`, false, false},
	{45, "src", []string{`f:- /src/foo`}, `src/foo`, false, true},
	{46, "src", []string{`f:- src`}, `src`, true, true},
	{47, "src", []string{`f:- src`}, `src/foo`, false, true},
	{48, "src/", []string{`f:- a/*/c`}, `a/b/c`, true, true},
	{49, "src/", []string{`f:- a/*`}, `a/f`, false, true},
	{50, "src/", []string{`f:- x/**/v`}, `x/y/v`, false, true},
	{51, "src/", []string{`f:- /x/**/y`}, `x/y`, true, false},
	{52, "src/", []string{`f:- */foo`}, `foo`, false, false},
	{53, "src/", []string{`f:- */foo`}, `sub/foo`, true, true},
	{54, "src/", []string{`f:exclude foo`}, `foo`, false, true},
	{55, "src/", []string{`f:-_foo`}, `foo`, false, true},
	{56, "src/", []string{`f:H foo`}, `foo`, false, true},
	{57, "src/", []string{`f:P foo`}, `foo`, false, false},
	{58, "src/", []string{`x:+ foo`, `x:foo`}, `foo`, false, false},
	{59, "src/", []string{`i:- foo`}, `foo`, false, true},
	{60, "src/", []string{`f:- foo`, `f:!`}, `foo`, false, false},
	{61, "src/", []string{`C`}, `core`, false, true},
	{62, "src/", []string{`C`}, `CVS`, true, true},
	{63, "src/", []string{`C`}, `x.bak`, false, true},
	{64, "src/", []string{`C`}, `foo.o`, false, true},
	{65, "src/", []string{`f:- foo `}, `foo`, false, false},
	{66, "src/", []string{`f:- linkdir/`}, `linkdir`, false, false},
	{67, "src/", []string{`f:-/ /t/src/foo`}, `foo`, false, true},
	{68, "src/", []string{`f:-/ /t/src/foo`}, `bar/foo`, false, false},
	{69, "src/", []string{`f:- FOO`}, `foo`, false, false},
	{70, "src/", []string{`x:foo/`}, `foo`, false, false},
	{71, "src/", []string{`f:-! foo`}, `bar`, true, true},
	{72, "src/", []string{`f:- */*/foo`}, `bar/deep/foo`, true, true},
	{73, "src/", []string{`f:- */*/foo`}, `bar/foo`, false, false},
	{74, "src/", []string{`f:- deep`}, `bar/deep`, true, true},
	{75, "src/", []string{`f:- **o`}, `bar/deep/foo`, true, true},
	{76, "src/", []string{`f:- **o`}, `bar/foo`, false, true},
	{77, "src/", []string{`f:- b**`}, `sub/b.o`, false, true},
	{78, "src/", []string{`f:- /**/foo`}, `foo`, false, false},
	{79, "src/", []string{`f:- /**/foo`}, `bar/foo`, false, true},
	{80, "src/", []string{`f:- [!a-z]oo`}, `Foo`, false, true},
	{81, "src/", []string{`f:- [!a-z]oo`}, `foo`, false, false},
	{82, "src/", []string{`f:- foo.d/`}, `foo.d`, true, true},
	{83, "src/", []string{`f:- *.d/`}, `foo.d`, true, true},
	{84, "src/", []string{`f:+,s foo`, `f:- foo`}, `foo`, false, false},
	{85, "src/", []string{`x:!`}, `foo`, false, false},
	{86, "src/", []string{`f:-/ src/foo`}, `foo`, false, true},
	{87, "src/", []string{`f:- src/foo`}, `foo`, false, false},
	{88, "src/", []string{`f:+ /a/b/c/d`, `f:- *`}, `a/b/c/d`, false, true},
	{89, "src/", []string{`f:+ /a/b/c/d`, `f:+ */`, `f:- *`}, `a/b/c/d`, false, false},
	{90, "src/", []string{`f:- [[:upper:]]*`}, `Foo`, false, true},
	{91, "src/", []string{`f:- [[:upper:]]*`}, `CVS`, true, true},
	{92, "src/", []string{`f:- q\?`}, `q?`, false, true},
	{93, "src/", []string{`f:- /*.o`}, `foo.o`, false, true},
	{94, "src/", []string{`f:- /*.o`}, `sub/b.o`, false, false},
	{95, "src/", []string{`i:*/`, `x:*`}, `bar/foo`, false, true},
	{96, "src/", []string{`i:*/`, `x:*`}, `bar/deep`, true, false},
	{97, "src/", []string{`f:- sub/**/y`}, `sub/foo/y`, false, true},
	{98, "src/", []string{`f:- foo/***`}, `foo`, false, false},
	{99, "src/", []string{`f:- foo/***`}, `sub/foo`, true, true},
	{100, "src/", []string{`f:- foo/***`}, `bar/deep/foo/x.txt`, false, true},
	{101, "src/", []string{`f:- /a/**`}, `a`, true, false},
	{102, "src/", []string{`f:- /a/**`}, `a/f`, false, true},
	{103, "src/", []string{`f:- bar/deep`}, `bar/deep`, true, true},
	{104, "src/", []string{`f:- /bar/deep/`}, `bar/deep`, true, true},
	{105, "src/", []string{`f:- b?r/d*p/`}, `bar/deep`, true, true},
	{106, "src/", []string{`f:- y`}, `x/y`, true, true},
	{107, "src/", []string{`f:- x/y/z/w`}, `x/y/z/w`, false, true},
	{108, "src/", []string{`f:- y/z/w`}, `x/y/z/w`, false, true},
	{109, "src/", []string{`f:- /y/z/w`}, `x/y/z/w`, false, false},
	{110, "src/", []string{`f:- */z/w`}, `x/y/z/w`, false, true},
	{111, "src/", []string{`f:- */w`}, `x/y/z/w`, false, true},
	{112, "src/", []string{`f:- *`}, `bar`, true, true},
}

// buildRules parses rule specs in the notation of matcherCases.
func buildRules(t testing.TB, p *Parser, specs []string) []Rule {
	t.Helper()
	var rules []Rule
	cvs := false
	for _, s := range specs {
		var r []Rule
		var err error
		switch {
		case s == "C":
			cvs = true
			continue
		case strings.HasPrefix(s, "f:"):
			r, err = p.Parse(s[2:])
		case strings.HasPrefix(s, "x:"):
			r, err = p.ParseOld(s[2:], false)
		case strings.HasPrefix(s, "i:"):
			r, err = p.ParseOld(s[2:], true)
		default:
			t.Fatalf("bad spec %q", s)
		}
		if err != nil {
			t.Fatalf("parse %q: %v", s, err)
		}
		rules = append(rules, r...)
	}
	if cvs {
		for _, s := range []string{":C", "-C"} {
			r, err := p.Parse(s)
			if err != nil {
				t.Fatal(err)
			}
			rules = append(rules, r...)
		}
	}
	return rules
}

// prunedExcluded reports whether path (or one of its ancestor directories)
// is excluded, i.e. whether the sender would leave it out of the transfer.
func prunedExcluded(l *List, p string, isDir bool) bool {
	comps := strings.Split(p, "/")
	for k := 1; k <= len(comps); k++ {
		name := strings.Join(comps[:k], "/")
		dir := k < len(comps) || isDir
		if l.Excluded(name, dir, SenderSide) {
			return true
		}
	}
	return false
}

func TestMatcherCases(t *testing.T) {
	if len(matcherCases) != 112 {
		t.Fatalf("have %d cases, want 112", len(matcherCases))
	}
	for _, c := range matcherCases {
		var p Parser
		rules := buildRules(t, &p, c.rules)
		cwd := "/t/src"
		if c.src == "src" {
			cwd = "/t"
		}
		l, err := NewListWithOptions(rules, Options{AmSender: true, Cwd: cwd})
		if err != nil {
			t.Fatalf("case %d: %v", c.id, err)
		}
		got := prunedExcluded(l, c.path, c.isDir)
		if got != c.excluded {
			t.Errorf("case %d: rules %q path %q dir=%v: excluded=%v, want %v",
				c.id, c.rules, c.path, c.isDir, got, c.excluded)
		}
	}
}
