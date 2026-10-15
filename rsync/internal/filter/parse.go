// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package filter

import (
	"fmt"
	"os"
	"path"
	"strings"
)

const (
	// maxPathLen mirrors MAXPATHLEN: patterns this long or longer are
	// discarded.
	maxPathLen = 4096
	// maxRecordLen is the longest record accepted from a rule file; longer
	// records are discarded (rsync's line buffer is MAXPATHLEN+1024).
	maxRecordLen = maxPathLen + 1024 - 1
	// maxMergeDepth limits merge-file nesting.
	maxMergeDepth = 32
)

// Parser parses filter rules. The zero value is ready to use and reads merge
// files with os.ReadFile.
type Parser struct {
	// ReadFile reads a merge file named by a "." (merge) rule. Relative
	// names are passed through unchanged (names containing a slash are
	// cleaned first). Nil means os.ReadFile. Callers that want "-" to mean
	// standard input handle that here.
	ReadFile func(name string) ([]byte, error)
	// DeleteExcluded applies --delete-excluded at parse time: include and
	// exclude rules without an explicit side become sender-only.
	DeleteExcluded bool
	// From0 makes records in rule files NUL-terminated instead of
	// newline-terminated (--from0).
	From0 bool
}

// ParseRule parses one rule in --filter syntax, e.g. "- *.o", "+ /foo/",
// "P core", "exclude,s foo", ": /.rsync-filter", "dir-merge,n- .gitignore"
// or "!". A "." (merge) rule is expanded into the rules of the named file,
// read with os.ReadFile. The result may start with a rule carrying FlagClear,
// meaning every rule before it is to be discarded (NewList does that).
func ParseRule(s string) ([]Rule, error) {
	var p Parser
	return p.Parse(s)
}

// Parse parses one --filter argument (full rule syntax).
func (p *Parser) Parse(s string) ([]Rule, error) {
	c := p.ctx()
	return c.parseString(nil, s, 0, false)
}

// ParseOld parses one --include (include=true) or --exclude argument. Only
// the prefixes "+ " and "- " are recognised and "!" clears the list.
func (p *Parser) ParseOld(s string, include bool) ([]Rule, error) {
	c := p.ctx()
	var t Flags
	if include {
		t = FlagInclude
	}
	return c.parseString(nil, s, t, true)
}

// ParseFile parses the contents of an --include-from (include=true) or
// --exclude-from file: one rule per line in --include/--exclude syntax;
// empty lines and lines starting with '#' or ';' are ignored.
func (p *Parser) ParseFile(data []byte, include bool) ([]Rule, error) {
	c := p.ctx()
	var t Flags
	if include {
		t = FlagInclude
	}
	return c.parseData(nil, data, t, true)
}

// ParseMergeData parses data as the contents of a merge file named by rule
// tmpl (a "." or ":" rule as returned by Parse), honouring its modifiers
// (n, w, -, +, C, sides, ...).
func (p *Parser) ParseMergeData(data []byte, tmpl Rule) ([]Rule, error) {
	c := p.ctx()
	return c.parseData(nil, data, tmpl.Flags, false)
}

func (p *Parser) ctx() *parseCtx {
	c := &parseCtx{deleteExcluded: p.DeleteExcluded, from0: p.From0, readFile: p.ReadFile}
	if c.readFile == nil {
		c.readFile = os.ReadFile
	}
	return c
}

// parseCtx carries the state of one parse (rsync's xflags and dirbuf).
type parseCtx struct {
	readFile       func(string) ([]byte, error)
	deleteExcluded bool
	from0          bool
	depth          int

	// Per-directory merge file mode (rsync's "anchored to abs" mode):
	// rules for the other side are dropped and anchored patterns are
	// prefixed with dirPrefix.
	perDir    bool
	amSender  bool
	dirPrefix string // absolute dir of the merge file, "" for "/"
	mergeDir  string // directory relative "." names with a slash resolve against
	cwd       string // directory plain "." names resolve against
}

// parseString parses s (one rule, or several whitespace-separated tokens
// when the template has FlagWordSplit) and appends the rules to dst. A clear
// rule truncates dst and leaves a FlagClear marker at its start.
func (c *parseCtx) parseString(dst []Rule, s string, tmpl Flags, old bool) ([]Rule, error) {
	for {
		r, pat, rest, ok, err := parseTok(s, tmpl, old, c.deleteExcluded)
		if err != nil {
			return dst, err
		}
		if !ok {
			return dst, nil
		}
		s = rest
		if len(pat) >= maxPathLen {
			continue // discarded, as rsync does
		}
		if r.Flags&FlagClear != 0 {
			dst = append(dst[:0], Rule{Flags: FlagClear})
			continue
		}
		if r.Flags&FlagMerge != 0 {
			if pat == "" {
				pat = ".cvsignore"
			}
			if r.Flags&FlagExcludeSelf != 0 {
				// A name ending in '/' has no base name to exclude;
				// an empty pattern matches nothing and could not be
				// sent to the server.
				if base := pat[strings.LastIndexByte(pat, '/')+1:]; base != "" {
					dst = c.add(dst, Rule{}, base, false)
				}
				r.Flags &^= FlagExcludeSelf
			}
			if r.Flags&FlagPerDirMerge == 0 {
				dst, err = c.merge(dst, r, pat)
				if err != nil {
					return dst, err
				}
				continue
			}
		}
		dst = c.add(dst, r, pat, c.perDir)
	}
}

// merge reads a "." merge file and appends its rules.
func (c *parseCtx) merge(dst []Rule, r Rule, name string) ([]Rule, error) {
	if c.depth >= maxMergeDepth {
		return dst, fmt.Errorf("%w: merge-file include depth limit (%d) exceeded at %s", ErrMergeFile, maxMergeDepth, name)
	}
	fn := name
	if strings.Contains(name, "/") {
		if !strings.HasPrefix(name, "/") && c.mergeDir != "" {
			fn = path.Join(c.mergeDir, name)
		} else {
			fn = path.Clean(name)
		}
	} else if c.cwd != "" {
		fn = path.Join(c.cwd, name)
	}
	data, err := c.readFile(fn)
	if err != nil {
		return dst, fmt.Errorf("%w %s: %v", ErrMergeFile, fn, err)
	}
	sub := *c
	sub.depth++
	sub.perDir = false // rules of a nested "." file are not dir-anchored
	return sub.parseData(dst, data, r.Flags, false)
}

// parseData splits a rule file into records and parses each one.
func (c *parseCtx) parseData(dst []Rule, data []byte, tmpl Flags, old bool) ([]Rule, error) {
	wordSplit := tmpl&FlagWordSplit != 0
	var err error
	i := 0
	for i < len(data) {
		start := i
		end := len(data)
		next := len(data)
		for j := i; j < len(data); j++ {
			ch := data[j]
			if wordSplit && isSpace(ch) {
				end, next = j, j+1
				break
			}
			if c.from0 {
				if ch == 0 {
					end, next = j, j+1
					break
				}
			} else if ch == '\n' || ch == '\r' {
				end, next = j, j+1
				if ch == '\r' && next < len(data) && data[next] == '\n' {
					next++
				}
				break
			}
		}
		i = next
		rec := data[start:end]
		if len(rec) == 0 || len(rec) > maxRecordLen {
			continue
		}
		if !wordSplit && (rec[0] == '#' || rec[0] == ';') {
			continue
		}
		dst, err = c.parseString(dst, string(rec), tmpl, old)
		if err != nil {
			return dst, err
		}
	}
	return dst, nil
}

// add normalises a parsed rule and appends it (rsync's add_rule).
func (c *parseCtx) add(dst []Rule, r Rule, pat string, perDir bool) []Rule {
	if perDir {
		other := FlagReceiverSide
		if !c.amSender {
			other = FlagSenderSide
		}
		if r.Flags&sideFlags == other {
			return dst // applies only to the other side
		}
	}
	if len(pat) > 1 && pat[len(pat)-1] == '/' {
		pat = pat[:len(pat)-1]
		r.Flags |= FlagDirectory
	} else if pat == "" {
		// A "-C" rule has no pattern to carry the trailing slash of a
		// directory merge file name it was read from ("dir-merge foo/").
		r.Flags &^= FlagDirectory
	}
	if perDir && r.Flags&(FlagAbsPath|FlagMerge) == 0 && strings.HasPrefix(pat, "/") {
		r.Flags |= FlagAbsPath
		pat = c.dirPrefix + pat
	}
	r.Pattern = pat
	r.compile()
	return append(dst, r)
}

func isSpace(b byte) bool {
	return b == ' ' || (b >= '\t' && b <= '\r')
}

// ruleWord matches a long rule name at the start of s. It returns the index
// of the last byte consumed (the word's last byte, or a following comma).
func ruleWord(s, word string) (int, bool) {
	if !strings.HasPrefix(s, word) {
		return 0, false
	}
	n := len(word)
	if n == len(s) || isSpace(s[n]) || s[n] == '_' {
		return n - 1, true
	}
	if s[n] == ',' {
		return n, true
	}
	return 0, false
}

func syntaxErr(msg, rule string) error {
	return &SyntaxError{Msg: msg, Rule: rule}
}

// parseTok parses the next rule from s. It returns the rule (pattern not
// yet stored), its pattern text, the unconsumed remainder, and ok=false when
// s holds no further rule.
func parseTok(s string, tmpl Flags, old, deleteExcluded bool) (r Rule, pat, rest string, ok bool, err error) {
	wordSplit := tmpl&FlagWordSplit != 0
	if wordSplit {
		for len(s) > 0 && isSpace(s[0]) {
			s = s[1:]
		}
	}
	if s == "" {
		return r, "", "", false, nil
	}
	orig := s
	at := func(i int) byte {
		if i < len(s) {
			return s[i]
		}
		return 0
	}
	r.Flags = tmpl & inheritedFlags
	i := 0 // index of the pattern start (after the loop below)
	switch {
	case tmpl&FlagNoPrefixes != 0:
		if s[0] == '!' && tmpl&FlagCVSIgnore != 0 {
			r.Flags |= FlagClear
		}
	case old:
		switch {
		case strings.HasPrefix(s, "- "):
			r.Flags &^= FlagInclude
			i = 2
		case strings.HasPrefix(s, "+ "):
			r.Flags |= FlagInclude
			i = 2
		case s[0] == '!':
			r.Flags |= FlagClear
		}
	default:
		var ch byte
		var j int
		var found bool
		switch s[0] {
		case 'c':
			j, found = ruleWord(s, "clear")
			ch = '!'
		case 'd':
			j, found = ruleWord(s, "dir-merge")
			ch = ':'
		case 'e':
			j, found = ruleWord(s, "exclude")
			ch = '-'
		case 'h':
			j, found = ruleWord(s, "hide")
			ch = 'H'
		case 'i':
			j, found = ruleWord(s, "include")
			ch = '+'
		case 'm':
			j, found = ruleWord(s, "merge")
			ch = '.'
		case 'p':
			j, found = ruleWord(s, "protect")
			ch = 'P'
		case 'r':
			j, found = ruleWord(s, "risk")
			ch = 'R'
		case 's':
			j, found = ruleWord(s, "show")
			ch = 'S'
		default:
			ch, found = s[0], true
			if at(1) == ',' {
				j = 1
			}
		}
		if !found {
			return r, "", "", false, syntaxErr("Unknown filter rule", orig)
		}
		sideByPrefix := false
		switch ch {
		case ':':
			r.Flags |= FlagPerDirMerge | FlagMerge
		case '.':
			r.Flags |= FlagMerge
		case '+':
			r.Flags |= FlagInclude
		case '-':
		case 'S':
			r.Flags |= FlagInclude | FlagSenderSide
			sideByPrefix = true
		case 'H':
			r.Flags |= FlagSenderSide
			sideByPrefix = true
		case 'R':
			r.Flags |= FlagInclude | FlagReceiverSide
			sideByPrefix = true
		case 'P':
			r.Flags |= FlagReceiverSide
			sideByPrefix = true
		case '!':
			r.Flags |= FlagClear
		default:
			return r, "", "", false, syntaxErr("Unknown filter rule", orig)
		}
		if ch != '!' {
			for {
				j++
				b := at(j)
				if b == 0 || b == ' ' || b == '_' {
					break
				}
				if wordSplit && isSpace(b) {
					j--
					break
				}
				bad := false
				switch b {
				case '-', '+':
					if r.Flags&(FlagMerge|FlagNoPrefixes) != FlagMerge {
						bad = true
						break
					}
					r.Flags |= FlagNoPrefixes
					if b == '+' {
						r.Flags |= FlagInclude
					}
				case '/':
					r.Flags |= FlagAbsPath
				case '!':
					if r.Flags&FlagMerge != 0 {
						bad = true
						break
					}
					r.Flags |= FlagNegate
				case 'C':
					if r.Flags&FlagNoPrefixes != 0 || sideByPrefix {
						bad = true
						break
					}
					r.Flags |= FlagNoPrefixes | FlagWordSplit | FlagNoInherit | FlagCVSIgnore
				case 'e', 'n', 'w':
					if r.Flags&FlagMerge == 0 {
						bad = true
						break
					}
					switch b {
					case 'e':
						r.Flags |= FlagExcludeSelf
					case 'n':
						r.Flags |= FlagNoInherit
					default:
						r.Flags |= FlagWordSplit
					}
				case 'p':
					r.Flags |= FlagPerishable
				case 'r', 's':
					if sideByPrefix {
						bad = true
						break
					}
					if b == 'r' {
						r.Flags |= FlagReceiverSide
					} else {
						r.Flags |= FlagSenderSide
					}
				case 'x':
					r.Flags |= FlagXattr
				default:
					bad = true
				}
				if bad {
					return r, "", "", false, syntaxErr(fmt.Sprintf("invalid modifier '%c' at position %d", b, j), orig)
				}
			}
		}
		i = j
		if at(i) != 0 {
			i++
		}
	}
	if tmpl&sideFlags != 0 {
		if r.Flags&sideFlags != 0 {
			return r, "", "", false, syntaxErr("specified-side merge file contains specified-side filter", orig)
		}
		r.Flags |= tmpl & sideFlags
	}
	s = s[i:]
	n := len(s)
	if wordSplit {
		n = 0
		for n < len(s) && !isSpace(s[n]) {
			n++
		}
	}
	pat, rest = s[:n], s[n:]
	if r.Flags&FlagClear != 0 {
		if tmpl&FlagNoPrefixes == 0 && !old && n > 0 {
			return r, "", "", false, syntaxErr("'!' rule has trailing characters", orig)
		}
		if n > 1 {
			r.Flags &^= FlagClear
		}
	} else if n == 0 && r.Flags&FlagCVSIgnore == 0 {
		return r, "", "", false, syntaxErr("unexpected end of filter rule", orig)
	}
	if deleteExcluded && r.Flags&(sideFlags|FlagMerge|FlagPerDirMerge) == 0 {
		r.Flags |= FlagSenderSide
	}
	return r, pat, rest, true, nil
}
