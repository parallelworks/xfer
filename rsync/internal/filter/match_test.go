// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package filter

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestWildMatch(t *testing.T) {
	cases := []struct {
		pat, text string
		want      bool
	}{
		{"", "", true},
		{"abc", "abc", true},
		{"abc", "abd", false},
		{"a?c", "abc", true},
		{"a?c", "a/c", false},
		{"???", "ab", false},
		{"*", "", true},
		{"*", "abc", true},
		{"*", "a/b", false},
		{"**", "a/b", true},
		{"a*", "a/b", false},
		{"a**", "a/b", true},
		{"*c", "abc", true},
		{"*c", "ab", false},
		{"*b*d*", "abcde", true},
		{"*x*y", "axbyxy", true},
		{"a*b*c", "aXbYbZc", true},
		{"*/b", "a/b", true},
		{"*/b", "x/a/b", false},
		{"**/b", "x/a/b", true},
		{"a/**/b", "a/b", false},
		{"a/**/b", "a/x/b", true},
		{"a/**/b", "a/x/y/b", true},
		{"a/**", "a/", true},
		{"a/**", "a", false},
		{"a/***", "a/", true},
		{`\*`, "*", true},
		{`\*`, "x", false},
		{`a\`, "a", false},
		{`a\`, `a\`, false},
		{`\\`, `\`, true},
		{"[abc]", "b", true},
		{"[abc]", "d", false},
		{"[!abc]", "d", true},
		{"[^abc]", "a", false},
		{"[a-c]x", "bx", true},
		{"[a-c]", "d", false},
		{"[]]", "]", true},
		{"[]a]", "a", true},
		{"[!]]", "]", false},
		{"[!]]", "x", true},
		{"[a-]", "-", true},
		{"[-a]", "-", true},
		{"[a-c-e]", "-", true},
		{"[a-c-e]", "d", false},
		{`[\]]`, "]", true},
		{`[a\-c]`, "b", false},
		{`[a\-c]`, "-", true},
		{`[a-\z]`, "q", true},
		{"[/]", "/", false},
		{"[!a]", "/", false},
		{"[abc", "a", false},
		{"[[:upper:]]", "Q", true},
		{"[[:upper:]]", "q", false},
		{"[[:digit:][:alpha:]]", "7", true},
		{"[[:space:]]", "\t", true},
		{"[[:punct:]]", "!", true},
		{"[[:xdigit:]]", "G", false},
		{"[[:bogus:]]", "a", false},
		{"[[:alpha:]", "a", false},
		{"[[:alpha]", "a", true}, // not a named class: plain members
		{"[[:alpha]", ":", true},
		{"[[:a]x]", "[x]", true},
		{"[[:]", "[", true},
		{"[!a-z]oo", "Foo", true},
		{"lit[1]", "lit1", true},
		{"lit[1]", "lit[1]", false},
		{`lit\[1]`, "lit[1]", true},
		{"*.o", "b.o", true},
		{"-*-*-*-*-*-*-12-*-*-*-m-*-*-*", "-adobe-courier-bold-o-normal--12-120-75-75-m-70-iso8859-1", true},
		{"-*-*-*-*-*-*-12-*-*-*-m-*-*-*", "-adobe-courier-bold-o-normal--12-120-75-75-X-70-iso8859-1", false},
		{"XXX/*/*/*/*/*/*/12/*/*/*/m/*/*/*", "XXX/adobe/courier/bold/o/normal//12/120/75/75/m/70/iso8859/1", true},
		{"**/*a*b*g*n*t", "abcd/abcdefg/abcdefghijk/abcdefghijklmnop.txt", true},
		{"**/*a*b*g*n*t", "abcd/abcdefg/abcdefghijk/abcdefghijklmnop.txtz", false},
		{strings.Repeat("*a", 30) + "b", strings.Repeat("a", 60), false},
		{strings.Repeat("**a", 20) + "b", strings.Repeat("a/", 40), false},
	}
	for _, c := range cases {
		if got := WildMatch(c.pat, c.text); got != c.want {
			t.Errorf("WildMatch(%q, %q) = %v, want %v", c.pat, c.text, got, c.want)
		}
	}
}

// TestWildtestFile checks the matcher against GNU rsync's wildtest.txt when
// its path is given in $RSYNC_WILDTEST (the file is not vendored).
func TestWildtestFile(t *testing.T) {
	fn := os.Getenv("RSYNC_WILDTEST")
	if fn == "" {
		t.Skip("RSYNC_WILDTEST not set")
	}
	data, err := os.ReadFile(fn)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for i, line := range strings.Split(string(data), "\n") {
		toks := splitQuoted(line)
		if len(toks) == 0 || strings.HasPrefix(toks[0], "#") {
			continue
		}
		if len(toks) < 4 {
			t.Fatalf("line %d: %q", i+1, line)
		}
		want := toks[0] == "1"
		if got := WildMatch(toks[3], toks[2]); got != want {
			t.Errorf("line %d: WildMatch(%q, %q) = %v, want %v", i+1, toks[3], toks[2], got, want)
		}
		n++
	}
	t.Logf("%d vectors", n)
}

func splitQuoted(s string) []string {
	var out []string
	for {
		s = strings.TrimLeft(s, " \t\r")
		if s == "" {
			return out
		}
		if q := s[0]; q == '\'' || q == '"' || q == '`' {
			end := strings.IndexByte(s[1:], q)
			if end < 0 {
				return append(out, s[1:])
			}
			out = append(out, s[1:1+end])
			s = s[end+2:]
			continue
		}
		end := strings.IndexAny(s, " \t\r")
		if end < 0 {
			return append(out, s)
		}
		out = append(out, s[:end])
		s = s[end:]
	}
}

func TestMatchModes(t *testing.T) {
	cases := []struct {
		rule  string
		cwd   string
		name  string
		isDir bool
		want  bool
	}{
		{"- foo", "/", "/a/foo", false, true},           // leading '/' ignored
		{"- /foo", "/", "/foo", false, true},            // rooted name
		{"- **/foo", "/", "/foo", false, false},         // rooted name: no "/" is prepended
		{"-/ /t/src/foo", "/t/src", "foo", false, true}, // abs path
		{"-/ /t/src/foo", "/t/src", "/foo", false, false},
		{"-/ /foo", "/", "foo", false, true}, // cwd is the root
		{"- d/***", "/", "x/d", true, true},
		{"- d/***", "/", "x/d", false, false},
		{"- d/***", "/", "x/d/e/f", false, true},
		{"-x user.*", "/", "user.foo", false, false}, // xattr rule vs file name
		{"- a/b", "/", "a/b", false, true},
		{"- a/b", "/", "xa/b", false, false},
		{"- a/b", "/", "z/a/b", false, true},
		{"- a*/b", "/", "z/ax/b", false, true},
		{"- a*/b", "/", "ax/b", false, true},
		{"- a*/b", "/", "b", false, false},
		{"-! foo", "/", "foo", false, false},
		{"-! foo/", "/", "foo", false, true},
		{"- *.o", "/", "x/y.o", false, true},
		{"- core*", "/", "x/core.1", false, true},
		{"- *ore*", "/", "x/core.1", false, true},
		{"- *", "/", "x/core.1", false, true},
		{"- b\\*", "/", "b*", false, true},
		{"- b\\*", "/", "bx", false, false},
	}
	for _, c := range cases {
		r, err := ParseRule(c.rule)
		if err != nil {
			t.Fatal(err)
		}
		l, _ := NewListWithOptions(r, Options{Cwd: c.cwd})
		if got := l.Excluded(c.name, c.isDir, SenderSide); got != c.want {
			t.Errorf("%q cwd %q name %q dir=%v: %v, want %v", c.rule, c.cwd, c.name, c.isDir, got, c.want)
		}
	}
	// xattr names only see x rules.
	r, _ := ParseRule("-x user.*")
	l := NewList(r)
	if l.Match("user.foo", false, SenderSide, XattrName) != Excluded {
		t.Error("xattr rule did not match xattr name")
	}
	// Sides.
	r, _ = ParseRule("P keep")
	l = NewList(r)
	if l.Excluded("keep", false, SenderSide) || !l.Excluded("keep", false, ReceiverSide) {
		t.Error("protect sides")
	}
	l, _ = NewListWithOptions(append(mustParse(t, "- a"), mustParse(t, "-sr b")...), Options{DeleteExcluded: true})
	if !l.Excluded("a", false, SenderSide) || l.Excluded("a", false, ReceiverSide) || !l.Excluded("b", false, ReceiverSide) {
		t.Error("delete-excluded sides")
	}
}

func mustParse(t testing.TB, s string) []Rule {
	t.Helper()
	r, err := ParseRule(s)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// benchRules is a realistic mix: CVS-style suffix globs, anchored paths,
// directory rules, "**" patterns, and a per-dir merge file.
var benchRules = []string{
	"- *.o", "- *.so", "- *.a", "- *~", "- .git/", "- node_modules/", "- /build/",
	"+ /src/**/*.go", "- **/testdata/*.bin", "- vendor/*/docs", "- *.tmp", "- core",
	"P /keep/", "H .DS_Store", "- [Tt]humbs.db", "- cache*/", ": .rsync-filter",
	"+ */", "+ *.go", "+ *.c", "+ *.h", "+ Makefile", "- *",
}

var benchNames = []struct {
	name  string
	isDir bool
}{
	{"src/pkg/module/file.go", false},
	{"src/pkg/module/file_test.go", false},
	{"src/pkg/module/obj/file.o", false},
	{"src/pkg/module", true},
	{"vendor/github.com/foo/bar/README.md", false},
	{"build", true},
	{"lib/libfoo.so", false},
	{"docs/images/logo.png", false},
	{"a/b/c/d/e/f/g/h/i/j/k.txt", false},
	{"third_party/testdata/blob.bin", false},
}

func benchList(b testing.TB) *List {
	var rules []Rule
	for _, s := range benchRules {
		rules = append(rules, mustParse(b, s)...)
	}
	cvs := mustParse(b, "-C")
	rules = append(cvs, rules...)
	l, err := NewListWithOptions(rules, Options{AmSender: true, Cwd: "/home/user/project"})
	if err != nil {
		b.Fatal(err)
	}
	return l
}

func BenchmarkExcluded(b *testing.B) {
	l := benchList(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		n := benchNames[i%len(benchNames)]
		l.Excluded(n.name, n.isDir, SenderSide)
	}
}

func TestExcludedNoAlloc(t *testing.T) {
	l := benchList(t)
	// Include rules that need an assembled subject.
	l2 := NewList(append(mustParse(t, "-/ /x/y"), append(mustParse(t, "- **/z"), mustParse(t, "- d/***")...)...))
	allocs := testing.AllocsPerRun(100, func() {
		for _, n := range benchNames {
			l.Excluded(n.name, n.isDir, SenderSide)
			l2.Excluded(n.name, n.isDir, SenderSide)
		}
	})
	if allocs != 0 {
		t.Errorf("Excluded allocates %.1f times per run", allocs)
	}
}

func ExampleList_Excluded() {
	var rules []Rule
	for _, s := range []string{"+ */", "+ *.go", "- *"} {
		r, _ := ParseRule(s)
		rules = append(rules, r...)
	}
	l := NewList(rules)
	fmt.Println(l.Excluded("cmd/main.go", false, SenderSide))
	fmt.Println(l.Excluded("README.md", false, SenderSide))
	fmt.Println(l.Excluded("cmd", true, SenderSide))
	// Output:
	// false
	// true
	// false
}
