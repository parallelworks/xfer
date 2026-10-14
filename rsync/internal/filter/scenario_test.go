// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package filter

import (
	"errors"
	"io/fs"
	"path"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// memFS is an in-memory tree: absolute path -> file content. A key ending
// in "/" declares a directory; parent directories are implied.
type memFS map[string]string

func (m memFS) read(p string) ([]byte, error) {
	if s, ok := m[p]; ok {
		return []byte(s), nil
	}
	return nil, fs.ErrNotExist
}

// children lists the entries of directory dir (absolute).
func (m memFS) children(dir string) (names []string, isDir map[string]bool) {
	isDir = map[string]bool{}
	prefix := strings.TrimSuffix(dir, "/") + "/"
	for k := range m {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		rest := k[len(prefix):]
		if rest == "" {
			continue
		}
		name, sub, deeper := strings.Cut(rest, "/")
		if _, seen := isDir[name]; !seen {
			names = append(names, name)
		}
		isDir[name] = isDir[name] || deeper || sub != ""
	}
	sort.Strings(names)
	return names, isDir
}

// sendWalk simulates the sender's recursive scan below root (absolute,
// also the List's Cwd) and returns the transferred non-directories.
func sendWalk(t *testing.T, l *List, m memFS, root string) ([]string, error) {
	var out []string
	var walk func(rel string) error
	walk = func(rel string) error {
		abs := path.Join(root, rel)
		leave, err := l.EnterDir(rel, func(base string) ([]byte, error) {
			return m.read(path.Join(abs, base))
		})
		defer leave()
		if err != nil {
			return err
		}
		names, isDir := m.children(abs)
		for _, n := range names {
			name := path.Join(rel, n)
			if l.Excluded(name, isDir[n], SenderSide) {
				continue
			}
			if isDir[n] {
				if err := walk(name); err != nil {
					return err
				}
			} else {
				out = append(out, name)
			}
		}
		return nil
	}
	err := walk("")
	return out, err
}

func allFiles(m memFS, root string) []string {
	var out []string
	for k := range m {
		if strings.HasPrefix(k, root+"/") && !strings.HasSuffix(k, "/") {
			out = append(out, strings.TrimPrefix(k, root+"/"))
		}
	}
	sort.Strings(out)
	return out
}

func absent(all, present []string) []string {
	p := map[string]bool{}
	for _, s := range present {
		p[s] = true
	}
	out := []string{}
	for _, s := range all {
		if !p[s] {
			out = append(out, s)
		}
	}
	return out
}

func pdTree() memFS {
	m := memFS{
		"/t/pd/.rules":           "- /top.txt\n- *.log\n",
		"/t/pd/sub/.rules":       "+ x.log\n",
		"/t/pd/sub2/.rules":      "!\n",
		"/t/pd/.ex":              "- top.txt\n",
		"/t/pd/cv/.cvsignore":    "a.tmp\n  b.tmp\n",
		"/t/pd/other/.w":         "- foo + x.log\n",
		"/t/.rsync-filter":       "- x.log\n",
		"/tmp/m1":                "# c\n; c\n\n- top.txt\n+ sub/x.log\n- x.log\n",
		"/tmp/m2":                "top.txt\n+ sub/x.log\n!\nx.log\n",
		"/t/pd/sub/deeper/":      "",
		"/t/pd/cv/in/":           "",
		"/t/pd/top.txt":          "",
		"/t/pd/x.log":            "",
		"/t/pd/sub/top.txt":      "",
		"/t/pd/sub/x.log":        "",
		"/t/pd/sub/deeper/x.log": "",
		"/t/pd/other/x.log":      "",
		"/t/pd/sub2/x.log":       "",
		"/t/pd/cv/a.tmp":         "",
		"/t/pd/cv/b.tmp":         "",
		"/t/pd/cv/in/a.tmp":      "",
	}
	return m
}

// TestPerDirScenarios covers the merge / per-directory table (§8.3),
// transferring pd/ (Cwd /t/pd).
func TestPerDirScenarios(t *testing.T) {
	allLogs := []string{"other/x.log", "sub/deeper/x.log", "sub/x.log", "sub2/x.log", "x.log"}
	cases := []struct {
		name   string
		specs  []string // as in matcherCases; "X:FILE"/"I:FILE" = --exclude-from/--include-from
		absent []string
	}{
		{"dir-merge", []string{"f:: .rules"}, []string{"other/x.log", "top.txt", "x.log"}},
		{"dir-merge e", []string{"f::e .rules"}, []string{".rules", "other/x.log", "sub/.rules", "sub2/.rules", "top.txt", "x.log"}},
		{"dir-merge n", []string{"f::n .rules"}, []string{"top.txt", "x.log"}},
		// The spec table abbreviates this row; GNU rsync also omits top.txt
		// (re-verified with rsync 3.5.0).
		{"global first", []string{"f:- x.log", "f:: .rules"}, []string{"other/x.log", "sub/deeper/x.log", "sub/x.log", "sub2/x.log", "top.txt", "x.log"}},
		{"global after", []string{"f:: .rules", "f:+ x.log"}, []string{"other/x.log", "top.txt", "x.log"}},
		{"no-prefix", []string{"f::- .ex"}, []string{}},
		{"prefixed", []string{"f:: .ex"}, []string{"sub/top.txt", "top.txt"}},
		{"cvs dir-merge", []string{"f::C"}, []string{"cv/a.tmp", "cv/b.tmp"}},
		{"cvs option", []string{"C"}, []string{"cv/a.tmp", "cv/b.tmp"}},
		{"parent scan", []string{"f:: /.rsync-filter"}, allLogs},
		{"-F", []string{"f:: /.rsync-filter"}, allLogs},
		{"-FF", []string{"f:: /.rsync-filter", "f:- .rsync-filter"}, allLogs},
		{"no parent scan", []string{"f:: .rsync-filter"}, []string{}},
		{"merge", []string{"f:. /tmp/m1"}, []string{"other/x.log", "sub/deeper/x.log", "sub/top.txt", "sub2/x.log", "top.txt", "x.log"}},
		{"exclude-from", []string{"X:/tmp/m1"}, []string{"other/x.log", "sub/deeper/x.log", "sub/top.txt", "sub2/x.log", "top.txt", "x.log"}},
		{"exclude-from clear", []string{"X:/tmp/m2"}, allLogs},
		{"include-from", []string{"I:/tmp/m2", "x:*.log"}, []string{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := pdTree()
			p := Parser{ReadFile: m.read}
			var specs []string
			var rules []Rule
			for _, s := range c.specs {
				if strings.HasPrefix(s, "X:") || strings.HasPrefix(s, "I:") {
					data, _ := m.read(s[2:])
					r, err := p.ParseFile(data, s[0] == 'I')
					if err != nil {
						t.Fatal(err)
					}
					rules = append(rules, buildRules(t, &p, specs)...)
					specs = nil
					rules = append(rules, r...)
					continue
				}
				specs = append(specs, s)
			}
			rules = append(rules, buildRules(t, &p, specs)...)
			l, err := NewListWithOptions(rules, Options{AmSender: true, Cwd: "/t/pd", ReadFile: m.read})
			if err != nil {
				t.Fatal(err)
			}
			got, err := sendWalk(t, l, m, "/t/pd")
			if err != nil {
				t.Fatal(err)
			}
			if a := absent(allFiles(m, "/t/pd"), got); !reflect.DeepEqual(a, c.absent) {
				t.Errorf("absent = %q, want %q", a, c.absent)
			}
		})
	}
}

func TestPerDirWordSplit(t *testing.T) {
	tree := func(w string) memFS {
		return memFS{"/t/w/.w": w, "/t/w/foo": "", "/t/w/bar": "", "/t/w/baz": "", "/t/w/qux": "",
			"/t/w/in/foo": "", "/t/w/in/bar": ""}
	}
	run := func(rule, w string, opts Options) ([]string, error) {
		m := tree(w)
		r, err := ParseRule(rule)
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(rule, "-C") {
			r2, _ := ParseRule(":C")
			r = append(r2, r...)
		}
		opts.AmSender, opts.Cwd = true, "/t/w"
		l, err := NewListWithOptions(r, opts)
		if err != nil {
			t.Fatal(err)
		}
		got, err := sendWalk(t, l, m, "/t/w")
		return absent(allFiles(m, "/t/w"), got), err
	}
	if a, err := run(":w .w", "-_foo +_bar\n-_bar", Options{}); err != nil || !reflect.DeepEqual(a, []string{"foo", "in/foo"}) {
		t.Errorf(":w: absent %q err %v", a, err)
	}
	if _, err := run(":w .w", "- foo + x.log", Options{}); !errors.Is(err, ErrSyntax) || !strings.Contains(err.Error(), "unexpected end of filter rule") {
		t.Errorf(":w with '- foo': err %v", err)
	}
	if a, err := run(":C .w", "foo bar ! baz", Options{}); err != nil || !reflect.DeepEqual(a, []string{"baz"}) {
		t.Errorf(":C: absent %q err %v", a, err)
	}
	if a, err := run("-C", "", Options{CVS: &CVSConfig{Env: "baz qux"}}); err != nil || !reflect.DeepEqual(a, []string{"baz", "qux"}) {
		t.Errorf("-C CVSIGNORE: absent %q err %v", a, err)
	}
}

// TestPerDirAnchored checks that "/x" in a per-dir file is anchored to the
// file's directory, also when transferring "pd" (no trailing slash).
func TestPerDirAnchored(t *testing.T) {
	m := memFS{"/t/pd/sub/.rules": "- /top.txt\n", "/t/pd/sub/top.txt": "", "/t/pd/sub/deeper/top.txt": "", "/t/pd/top.txt": ""}
	for _, cwd := range []string{"/t", "/"} {
		r, _ := ParseRule(": .rules")
		l, err := NewListWithOptions(r, Options{AmSender: true, Cwd: cwd})
		if err != nil {
			t.Fatal(err)
		}
		got, err := sendWalk(t, l, m, cwd)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"pd/sub/.rules", "pd/sub/deeper/top.txt", "pd/top.txt"}
		if cwd == "/" {
			want = []string{"t/pd/sub/.rules", "t/pd/sub/deeper/top.txt", "t/pd/top.txt"}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("cwd %s: got %q want %q", cwd, got, want)
		}
	}
}

// TestNestedDirMerge checks a dir-merge rule defined inside a per-dir file
// and its scope.
func TestNestedDirMerge(t *testing.T) {
	m := memFS{
		"/r/.rules":   ": .more\n",
		"/r/.more":    "- a\n",
		"/r/d/.more":  "- b\n",
		"/r/a":        "",
		"/r/b":        "",
		"/r/d/a":      "",
		"/r/d/b":      "",
		"/r/d/c":      "",
		"/r/e/b":      "",
		"/r/e/.rules": ": .rules\n", // duplicate base name: ignored
	}
	r, _ := ParseRule(": .rules")
	l, err := NewListWithOptions(r, Options{AmSender: true, Cwd: "/r"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := sendWalk(t, l, m, "/r")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{".more", ".rules", "b", "d/.more", "d/c", "e/.rules", "e/b"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q want %q", got, want)
	}
	if len(l.merges) != 1 {
		t.Errorf("merge lists not restored: %d", len(l.merges))
	}
}

// TestDeleteScenarios simulates the receiver's --delete scan (§8.4).
func TestDeleteScenarios(t *testing.T) {
	cases := []struct {
		name           string
		deleteExcluded bool
		specs          []string
		want           string
	}{
		{"plain", false, nil, "keep sub"},
		{"exclude", false, []string{"f:- prot"}, "keep prot sub sub/prot"},
		{"anchored", false, []string{"f:- /prot"}, "keep prot sub"},
		{"delete-excluded", true, []string{"f:- prot"}, "keep sub"},
		{"hide", false, []string{"f:H prot"}, "keep sub"},
		{"protect", false, []string{"f:P prot"}, "keep prot sub sub/prot"},
		{"protect delete-excluded", true, []string{"f:P prot", "f:- prot"}, "keep prot sub sub/prot"},
		{"sr delete-excluded", true, []string{"f:-sr prot"}, "keep prot sub sub/prot"},
		{"non-perishable", false, []string{"f:- *.o"}, "gone gone/y.o keep sub x.o"},
		{"perishable", false, []string{"f:-p *.o"}, "keep sub x.o"},
		{"dir-merge e", false, []string{"f::e .rules"}, ".rules keep sub zz"},
		{"dir-merge r", false, []string{"f::r .rules"}, "keep sub zz"},
		{"exclude dir", false, []string{"x:gone/"}, "gone gone/y.o keep sub"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dest := memFS{"/t/d/keep": "", "/t/d/prot": "", "/t/d/x.o": "", "/t/d/zz": "",
				"/t/d/.rules": "- zz\n", "/t/d/gone/y.o": "", "/t/d/sub/prot": ""}
			source := map[string]bool{"keep": true, "sub": true}
			p := Parser{DeleteExcluded: c.deleteExcluded}
			rules := buildRules(t, &p, c.specs)
			l, err := NewListWithOptions(rules, Options{AmSender: false, DeleteExcluded: c.deleteExcluded, Cwd: "/t/d"})
			if err != nil {
				t.Fatal(err)
			}
			deleteScan(t, l, dest, "/t/d", source)
			var got []string
			for k := range dest {
				got = append(got, strings.TrimPrefix(k, "/t/d/"))
			}
			// Include surviving directories.
			dirs := map[string]bool{}
			for _, k := range got {
				if i := strings.LastIndexByte(k, '/'); i >= 0 {
					dirs[k[:i]] = true
				}
			}
			if source["sub"] {
				dirs["sub"] = true
			}
			for d := range dirs {
				got = append(got, d)
			}
			sort.Strings(got)
			if s := strings.Join(got, " "); s != c.want {
				t.Errorf("dest = %q, want %q", s, c.want)
			}
		})
	}
}

// deleteScan deletes from dest (below root) everything that is not in
// source and not protected, mimicking rsync's generator.
func deleteScan(t *testing.T, l *List, dest memFS, root string, source map[string]bool) {
	var scan func(rel string)
	scan = func(rel string) {
		abs := path.Join(root, rel)
		leave, err := l.EnterDir(rel, func(base string) ([]byte, error) { return dest.read(path.Join(abs, base)) })
		if err != nil {
			t.Fatal(err)
		}
		defer leave()
		names, isDir := dest.children(abs)
		for _, n := range names {
			name := path.Join(rel, n)
			if source[name] {
				if isDir[n] {
					scan(name)
				}
				continue
			}
			if l.Match(name, isDir[n], ReceiverSide, 0) == Excluded {
				continue // protected
			}
			deleteTree(t, l, dest, root, name, isDir[n])
		}
	}
	scan("")
}

// deleteTree removes name; for a directory its contents are checked with
// perishable rules ignored, and protected entries keep it alive.
func deleteTree(t *testing.T, l *List, dest memFS, root, name string, isDir bool) bool {
	abs := path.Join(root, name)
	if !isDir {
		delete(dest, abs)
		return true
	}
	leave, err := l.EnterDir(name, func(base string) ([]byte, error) { return dest.read(path.Join(abs, base)) })
	if err != nil {
		t.Fatal(err)
	}
	defer leave()
	empty := true
	names, sub := dest.children(abs)
	for _, n := range names {
		child := path.Join(name, n)
		if l.Match(child, sub[n], ReceiverSide, IgnorePerishable) == Excluded {
			empty = false
			continue
		}
		if !deleteTree(t, l, dest, root, child, sub[n]) {
			empty = false
		}
	}
	if empty {
		delete(dest, abs+"/")
	}
	return empty
}

// TestWireRules covers the client->server list captures (§7.3).
func TestWireRules(t *testing.T) {
	devnull := func(string) ([]byte, error) { return nil, nil }
	cases := []struct {
		name           string
		proto          int
		clientIsSender bool
		deleteExcluded bool
		specs          []string
		want           []string
		err            error
	}{
		{"pull 32", 32, false, false,
			[]string{"f:- foo", "f:+ bar/", "f:H x", "f:P y", "f:-p z", "f:: .rules", "f:-! */", "f:merge,e /dev/null"},
			[]string{"- foo", "+ bar/", "-s x", "-p z", ": .rules", "-! */", "- null"}, nil},
		{"pull delete-excluded", 32, false, true,
			[]string{"f:- foo", "f:P y", "f:R w", "f:-sr q"},
			[]string{"-s foo", "-sr q"}, nil},
		{"pull -C", 32, false, false, nil, nil, nil},
		{"pull 28", 28, false, false,
			[]string{"f:- foo", "f:+ bar", "f:- - dash", "f:H x"},
			[]string{"foo", "+ bar", "- - dash", "x"}, nil},
		{"pull 28 dir-merge", 28, false, false, []string{"f:: .rules"}, nil, ErrTooModern},
		{"pull 29 perishable", 29, false, false, []string{"f:-p z"}, []string{"- z"}, nil},
		{"pull abs xattr", 32, false, false, []string{"f:-/ /abs", "f:-x user.*"}, []string{"-/ /abs", "-x user.*"}, nil},
		{"push delete", 32, true, false,
			[]string{"f:- foo", "f:H x", "f:P y", "f:S s", "f:R r", "f::s .a", "f::r .b"},
			[]string{"- foo", "-r y", "+r r", ":r .b"}, nil},
		{"push delete-excluded", 32, true, true, []string{"f:- foo", "f:P y"}, []string{"-r y"}, nil},
		{"push 28 protect", 28, true, false, []string{"f:P y"}, []string{"y"}, nil},
		{"push 29 perishable", 29, true, false, []string{"f:-p y"}, nil, ErrTooModern},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := Parser{ReadFile: devnull, DeleteExcluded: c.deleteExcluded}
			l, err := NewListWithOptions(buildRules(t, &p, c.specs), Options{AmSender: c.clientIsSender, DeleteExcluded: c.deleteExcluded})
			if err != nil {
				t.Fatal(err)
			}
			got, err := l.WireRules(c.proto, c.clientIsSender, c.deleteExcluded)
			if !errors.Is(err, c.err) {
				t.Fatalf("err = %v, want %v", err, c.err)
			}
			if c.err == nil && !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}

	// --delete -C as sender: ":C" and the expanded perishable CVS list.
	r1, _ := ParseRule(":C")
	r2, _ := ParseRule("-C")
	l := NewList(append(r1, r2...))
	got, err := l.WireRules(32, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1+len(strings.Fields(DefaultCVSIgnore)) || got[0] != ":C .cvsignore" ||
		got[1] != "-p RCS" || got[2] != "-p SCCS" || got[3] != "-p CVS" || got[len(got)-1] != "-p .bzr/" {
		t.Errorf("-C push: %q", got)
	}
	// As receiver with P >= 29 the placeholder itself is sent.
	got, err = l.WireRules(32, false, false)
	if err != nil || !reflect.DeepEqual(got, []string{":C .cvsignore", "-C "}) {
		t.Errorf("-C pull: %q %v", got, err)
	}
}

func TestListExchanged(t *testing.T) {
	cases := []struct {
		proto                                     int
		sender, deleteMode, deleteExcluded, prune bool
		want                                      bool
	}{
		{32, true, false, false, false, false},
		{32, true, true, false, false, true},
		{28, true, true, true, false, false},
		{29, true, true, true, false, true},
		{32, true, false, false, true, true},
		{32, false, false, false, false, true},
		{28, false, true, true, false, true},
	}
	for _, c := range cases {
		if got := ListExchanged(c.proto, c.sender, c.deleteMode, c.deleteExcluded, c.prune); got != c.want {
			t.Errorf("%+v: got %v", c, got)
		}
	}
}

// TestWireRoundTrip parses what a client would send and checks the server
// interprets it the same way.
func TestWireRoundTrip(t *testing.T) {
	got, err := ParseWireRule("foo", 28)
	if err != nil || len(got) != 1 || got[0].Pattern != "foo" || got[0].IsInclude() {
		t.Errorf("proto 28 foo: %v %v", got, err)
	}
	if _, err := ParseWireRule("foo", 32); !errors.Is(err, ErrSyntax) {
		t.Errorf("proto 32 foo: err %v", err)
	}
	got, err = ParseWireRule("+ bar/", 28)
	if err != nil || len(got) != 1 || !got[0].IsInclude() || got[0].Flags&FlagDirectory == 0 {
		t.Errorf("+ bar/: %v %v", got, err)
	}
	got, err = ParseWireRule("-sr q", 32)
	if err != nil || len(got) != 1 || got[0].Flags&sideFlags != sideFlags {
		t.Errorf("-sr q: %v %v", got, err)
	}
}
