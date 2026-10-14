// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package filter

import (
	"errors"
	"strings"
	"testing"
)

func TestParseRule(t *testing.T) {
	cases := []struct {
		in    string
		want  []string // Rule.String() of each result
		flags Flags    // flags that must be set on the first rule
	}{
		{"- *.o", []string{"- *.o"}, 0},
		{"+ /foo/", []string{"+ /foo/"}, FlagInclude | FlagDirectory},
		{"P core", []string{"-r core"}, FlagReceiverSide},
		{"R core", []string{"+r core"}, FlagReceiverSide | FlagInclude},
		{"H x", []string{"-s x"}, FlagSenderSide},
		{"S x", []string{"+s x"}, FlagSenderSide | FlagInclude},
		{"exclude,s foo", []string{"-s foo"}, FlagSenderSide},
		{"-,s foo", []string{"-s foo"}, FlagSenderSide},
		{"exclude foo", []string{"- foo"}, 0},
		{"include_foo", []string{"+ foo"}, FlagInclude},
		{"hide foo", []string{"-s foo"}, FlagSenderSide},
		{"show foo", []string{"+s foo"}, FlagSenderSide | FlagInclude},
		{"protect foo", []string{"-r foo"}, FlagReceiverSide},
		{"risk foo", []string{"+r foo"}, FlagReceiverSide | FlagInclude},
		{"-_foo", []string{"- foo"}, 0},
		{"-  foo", []string{"-  foo"}, 0}, // pattern " foo"
		{"- foo ", []string{"- foo "}, 0}, // trailing space kept
		{"- foo//", []string{"- foo//"}, FlagDirectory},
		{"- /", []string{"- /"}, 0},
		{": /.rsync-filter", []string{": /.rsync-filter"}, FlagPerDirMerge | FlagMerge},
		{"dir-merge,n- .gitignore", []string{":n- .gitignore"}, FlagPerDirMerge | FlagNoInherit | FlagNoPrefixes},
		{":C", []string{":C .cvsignore"}, FlagCVSIgnore | FlagWordSplit | FlagNoInherit | FlagNoPrefixes},
		{":e .rules", []string{"- .rules", ": .rules"}, 0},
		{"-C", []string{"-C"}, FlagCVSIgnore},
		{"-! */", []string{"-! */"}, FlagNegate | FlagDirectory},
		{"-/ /abs", []string{"-/ /abs"}, FlagAbsPath},
		{"-x user.*", []string{"-x user.*"}, FlagXattr},
		{"-p z", []string{"-p z"}, FlagPerishable},
		{"-sr q", []string{"-sr q"}, FlagSenderSide | FlagReceiverSide},
		{"!", []string{"!"}, FlagClear},
		{"clear", []string{"!"}, FlagClear},
	}
	for _, c := range cases {
		got, err := ParseRule(c.in)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		var s []string
		for _, r := range got {
			s = append(s, r.String())
		}
		if strings.Join(s, "|") != strings.Join(c.want, "|") {
			t.Errorf("%q: got %q want %q", c.in, s, c.want)
			continue
		}
		if got[0].Flags&c.flags != c.flags {
			t.Errorf("%q: flags %b missing %b", c.in, got[0].Flags, c.flags)
		}
	}
	if r, _ := ParseRule("-  foo"); r[0].Pattern != " foo" {
		t.Errorf("pattern %q", r[0].Pattern)
	}
	if r, _ := ParseRule("- foo//"); r[0].Pattern != "foo/" {
		t.Errorf("pattern %q", r[0].Pattern)
	}
}

func TestParseRuleErrors(t *testing.T) {
	cases := map[string]string{
		"-foo":       "invalid modifier",
		"- ":         "unexpected end",
		"-":          "unexpected end",
		"!x":         "trailing characters",
		"clear x":    "trailing characters",
		"hide,s x":   "invalid modifier",
		"Ps x":       "invalid modifier",
		" - foo":     "Unknown filter rule",
		"excludefoo": "Unknown filter rule",
		"Exclude x":  "Unknown filter rule",
		"z foo":      "Unknown filter rule",
		"-e foo":     "invalid modifier",
		"-n foo":     "invalid modifier",
		":! .x":      "invalid modifier",
		":-+ .x":     "invalid modifier",
		":-C .x":     "invalid modifier",
		"HC":         "invalid modifier",
		"-\tfoo":     "invalid modifier",
		":w,n":       "invalid modifier",
		"":           "",
	}
	for in, msg := range cases {
		r, err := ParseRule(in)
		if msg == "" {
			if err != nil || len(r) != 0 {
				t.Errorf("%q: %v %v", in, r, err)
			}
			continue
		}
		if !errors.Is(err, ErrSyntax) || !strings.Contains(err.Error(), msg) {
			t.Errorf("%q: err %v, want %q", in, err, msg)
		}
	}
}

func TestParseOld(t *testing.T) {
	var p Parser
	cases := []struct {
		in      string
		include bool
		want    string
	}{
		{"+ foo", false, "+ foo"},
		{"- foo", true, "- foo"},
		{"-foo", false, "- -foo"},
		{"foo/", true, "+ foo/"},
		{"!x", false, "- !x"},
		{"!", false, "!"},
		{"H x", false, "- H x"},
	}
	for _, c := range cases {
		r, err := p.ParseOld(c.in, c.include)
		if err != nil || len(r) != 1 || r[0].String() != c.want {
			t.Errorf("%q: %v %v, want %q", c.in, r, err, c.want)
		}
	}
	// An empty --exclude/--filter yields no rule and no error (verified
	// with GNU rsync 3.5.0, contrary to the spec's §3.2 note).
	if r, err := p.ParseOld("", false); err != nil || len(r) != 0 {
		t.Errorf("empty: %v %v", r, err)
	}
}

func TestDeleteExcludedParse(t *testing.T) {
	p := Parser{DeleteExcluded: true}
	for in, want := range map[string]string{
		"- foo":    "-s foo",
		"P foo":    "-r foo",
		"-sr foo":  "-sr foo",
		": .rules": ": .rules",
		"+ bar":    "+s bar",
	} {
		r, err := p.Parse(in)
		if err != nil || r[0].String() != want {
			t.Errorf("%q: %v %v want %q", in, r, err, want)
		}
	}
}

func TestParseFileRecords(t *testing.T) {
	var p Parser
	data := "# comment\r\n; c2\r\n\r\n- a\r+ b\r\n c\n#d\n"
	r, err := p.ParseFile([]byte(data), false)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, x := range r {
		got = append(got, x.String())
	}
	if strings.Join(got, "|") != "- a|+ b|-  c" {
		t.Errorf("got %q", got)
	}
	p.From0 = true
	r, err = p.ParseFile([]byte("a\nb\x00+ c\x00\x00#x\x00"), false)
	if err != nil || len(r) != 2 || r[0].Pattern != "a\nb" || r[1].String() != "+ c" {
		t.Errorf("from0: %v %v", r, err)
	}
	// Over-long records are discarded.
	long := strings.Repeat("x", maxRecordLen+1)
	r, err = (&Parser{}).ParseFile([]byte(long+"\nshort\n"), false)
	if err != nil || len(r) != 1 || r[0].Pattern != "short" {
		t.Errorf("long: %d rules %v", len(r), err)
	}
}

func TestMergeFile(t *testing.T) {
	files := map[string]string{
		"/m/a":     "- one\n. /m/b\n+ four\n",
		"/m/b":     "- two\n!\n- three\n",
		"/m/plus":  "x\n- y\n",
		"/m/ws":    "-_p +_q\n\n-_r",
		"/m/side":  "- s1\n",
		"/m/sided": "-r s1\n",
		"/m/loop":  ". /m/loop\n",
	}
	read := func(name string) ([]byte, error) {
		if s, ok := files[name]; ok {
			return []byte(s), nil
		}
		return nil, errors.New("no such file")
	}
	p := Parser{ReadFile: read}
	check := func(rule string, want string, wantErr error) {
		t.Helper()
		r, err := p.Parse(rule)
		if wantErr != nil {
			if !errors.Is(err, wantErr) {
				t.Errorf("%q: err %v want %v", rule, err, wantErr)
			}
			return
		}
		if err != nil {
			t.Errorf("%q: %v", rule, err)
			return
		}
		var s []string
		for _, x := range r {
			s = append(s, x.String())
		}
		if got := strings.Join(s, "|"); got != want {
			t.Errorf("%q: got %q want %q", rule, got, want)
		}
	}
	// "!" in a nested merge clears the list being built (all earlier rules).
	check(". /m/a", "!|- three|+ four", nil)
	check("merge,+ /m/plus", "+ x|+ - y", nil)
	check("merge,- /m/plus", "- x|- - y", nil)
	check("merge,w /m/ws", "- p|+ q|- r", nil)
	check("merge,s /m/side", "-s s1", nil)
	check("merge,s /m/sided", "", ErrSyntax)
	check("merge,e /m/side", "- side|- s1", nil)
	check(". /m/missing", "", ErrMergeFile)
	check(". /m/loop", "", ErrMergeFile)
	check("merge,/ /m/side", "-/ s1", nil)
	check("merge,p /m/side", "-p s1", nil)
}

// TestListClear checks that a clear rule anywhere drops the rules before it.
func TestListClear(t *testing.T) {
	var rules []Rule
	for _, s := range []string{"- a", ": .r", "!", "- b", ": .r"} {
		r, err := ParseRule(s)
		if err != nil {
			t.Fatal(err)
		}
		rules = append(rules, r...)
	}
	l := NewList(rules)
	if len(l.Rules()) != 2 || l.Excluded("a", false, SenderSide) || !l.Excluded("b", false, SenderSide) {
		t.Errorf("rules after clear: %v", l.Rules())
	}
	if len(l.merges) != 1 || !l.HasDirMerge() {
		t.Errorf("merges: %d", len(l.merges))
	}
	// Duplicate dir-merge base names are dropped.
	r1, _ := ParseRule(": .r")
	r2, _ := ParseRule(": /x/.r")
	l = NewList(append(r1, r2...))
	if len(l.Rules()) != 1 {
		t.Errorf("duplicate dir-merge kept: %v", l.Rules())
	}
}

func TestCVSRules(t *testing.T) {
	r := CVSRules()
	if len(r) != len(strings.Fields(DefaultCVSIgnore)) {
		t.Fatalf("%d rules", len(r))
	}
	if r[0].String() != "-p RCS" || r[len(r)-1].String() != "-p .bzr/" {
		t.Errorf("%v ... %v", r[0], r[len(r)-1])
	}
	r, err := BuildCVSRules(CVSConfig{Protocol: 29, Env: "a ! b c",
		HomeIgnore: func() ([]byte, error) { return []byte("h1\nh2"), nil }})
	if err != nil {
		t.Fatal(err)
	}
	var s []string
	for _, x := range r {
		s = append(s, x.String())
	}
	if strings.Join(s, " ") != "- b - c" {
		t.Errorf("got %q", s)
	}
	r, _ = BuildCVSRules(CVSConfig{Protocol: 29, HomeIgnore: func() ([]byte, error) { return []byte("h1\nh2"), nil }})
	if n := len(r); r[n-2].String() != "- h1" || r[n-1].String() != "- h2" || r[0].Flags&FlagPerishable != 0 {
		t.Errorf("home: %v", r[n-2:])
	}
}

func TestStringRoundTrip(t *testing.T) {
	for _, in := range []string{"- foo", "+ bar/", "-s x", "-r y", "-p z", ": .rules", "-! */", "-/ /abs",
		"-x user.*", ":n- .gitignore", ":C .cvsignore", ":w+ .inc", "+sr q", "-C"} {
		r, err := ParseRule(in)
		if err != nil || len(r) != 1 {
			t.Fatalf("%q: %v", in, err)
		}
		if r[0].String() != in {
			t.Errorf("%q: String() = %q", in, r[0].String())
		}
	}
}
