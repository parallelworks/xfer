// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package filter

import "strings"

// text is the set of subject types the matcher works on. Names are matched
// as strings directly; subjects that need extra pieces (an absolute-path
// prefix, a leading or trailing slash) are assembled in a stack buffer and
// matched as []byte, so the hot path does not allocate.
type text interface{ ~string | ~[]byte }

// Results of the recursive wildcard matcher. Besides plain true/false, two
// pruning results let callers stop trying further split points when they can
// not succeed.
const (
	wmFalse         = 0
	wmTrue          = 1
	wmAbortAll      = -1 // the text ran out: no later start can match
	wmAbortToDouble = -2 // a single '*' hit a '/': only an enclosing '**' may retry
)

// WildMatch reports whether pattern matches the whole of name using rsync's
// wildcard rules: '?' matches one byte other than '/', '*' a run of non-'/'
// bytes, '**' any run of bytes, '[...]' a byte class, and '\' quotes the next
// byte.
func WildMatch(pattern, name string) bool {
	return wild(pattern, name) == wmTrue
}

func wild[T text](p string, t T) int {
	ti := 0
	for pi := 0; pi < len(p); pi++ {
		pc := p[pi]
		if pc == '*' {
			pi++
			double := false
			if pi < len(p) && p[pi] == '*' {
				double = true
				for pi < len(p) && p[pi] == '*' {
					pi++
				}
			}
			rest := p[pi:]
			if rest == "" {
				if double {
					return wmTrue
				}
				for ; ti < len(t); ti++ {
					if t[ti] == '/' {
						return wmFalse
					}
				}
				return wmTrue
			}
			for ; ti < len(t); ti++ {
				r := wild(rest, t[ti:])
				if r != wmFalse {
					if !double || r != wmAbortToDouble {
						return r
					}
				} else if !double && t[ti] == '/' {
					return wmAbortToDouble
				}
			}
			return wmAbortAll
		}
		if ti >= len(t) {
			return wmAbortAll
		}
		tc := t[ti]
		ti++
		switch pc {
		case '\\':
			pi++
			if pi >= len(p) || p[pi] != tc {
				return wmFalse
			}
		case '?':
			if tc == '/' {
				return wmFalse
			}
		case '[':
			np, ok, r := matchClass(p, pi+1, tc)
			if r != wmTrue {
				return r
			}
			if !ok {
				return wmFalse
			}
			pi = np
		default:
			if pc != tc {
				return wmFalse
			}
		}
	}
	if ti < len(t) {
		return wmFalse
	}
	return wmTrue
}

// matchClass evaluates the bracket expression whose body starts at p[i] (just
// after '[') against byte c. It returns the index of the closing ']', whether
// c is a member, and wmTrue, or a non-true result for a malformed class.
func matchClass(p string, i int, c byte) (end int, member bool, res int) {
	at := func(k int) byte {
		if k < len(p) {
			return p[k]
		}
		return 0
	}
	negate := false
	if b := at(i); b == '!' || b == '^' {
		negate = true
		i++
	}
	matched := false
	var prev byte // previous single member, 0 if none (after a range/class)
	first := true
	for {
		b := at(i)
		if b == 0 {
			return 0, false, wmAbortAll
		}
		if b == ']' && !first {
			break
		}
		first = false
		switch {
		case b == '\\':
			i++
			b = at(i)
			if b == 0 {
				return 0, false, wmAbortAll
			}
			if c == b {
				matched = true
			}
			prev = b
		case b == '-' && prev != 0 && at(i+1) != 0 && at(i+1) != ']':
			i++
			hi := at(i)
			if hi == '\\' {
				i++
				hi = at(i)
				if hi == 0 {
					return 0, false, wmAbortAll
				}
			}
			if c >= prev && c <= hi {
				matched = true
			}
			prev = 0
		case b == '[' && at(i+1) == ':':
			// Look for the ']' that ends a "[:name:]".
			s := i + 2
			k := s
			for k < len(p) && p[k] != ']' {
				k++
			}
			if k >= len(p) {
				return 0, false, wmAbortAll
			}
			if k-s-1 < 0 || p[k-1] != ':' {
				// Not a named class: '[' is an ordinary member.
				if c == '[' {
					matched = true
				}
				prev = '['
				break
			}
			ok, known := namedClass(p[s:k-1], c)
			if !known {
				return 0, false, wmAbortAll
			}
			if ok {
				matched = true
			}
			i = k
			prev = 0
		default:
			if c == b {
				matched = true
			}
			prev = b
		}
		i++
	}
	if matched == negate || c == '/' {
		return i, false, wmTrue
	}
	return i, true, wmTrue
}

// namedClass tests c against an ASCII POSIX character class.
func namedClass(name string, c byte) (ok, known bool) {
	isUpper := c >= 'A' && c <= 'Z'
	isLower := c >= 'a' && c <= 'z'
	isDigit := c >= '0' && c <= '9'
	isAlpha := isUpper || isLower
	isPrint := c >= 0x20 && c < 0x7f
	switch name {
	case "alnum":
		return isAlpha || isDigit, true
	case "alpha":
		return isAlpha, true
	case "blank":
		return c == ' ' || c == '\t', true
	case "cntrl":
		return c < 0x20 || c == 0x7f, true
	case "digit":
		return isDigit, true
	case "graph":
		return isPrint && c != ' ', true
	case "lower":
		return isLower, true
	case "print":
		return isPrint, true
	case "punct":
		return isPrint && c != ' ' && !isAlpha && !isDigit, true
	case "space":
		return c == ' ' || (c >= '\t' && c <= '\r'), true
	case "upper":
		return isUpper, true
	case "xdigit":
		return isDigit || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F'), true
	}
	return false, false
}

// wildAnywhere matches p at the start of t or right after any '/'.
func wildAnywhere[T text](p string, t T) bool {
	r := wild(p, t)
	if r == wmTrue {
		return true
	}
	if r == wmAbortAll {
		return false
	}
	for i := 0; i < len(t); i++ {
		if t[i] != '/' {
			continue
		}
		r = wild(p, t[i+1:])
		if r != wmFalse && r != wmAbortToDouble {
			return r == wmTrue
		}
	}
	return false
}

// tailStart returns the offset of the last n components of t. If t has
// exactly n-1 slashes it returns 0; with fewer, ok is false.
func tailStart[T text](t T, n int) (int, bool) {
	for i := len(t) - 1; i >= 0; i-- {
		if t[i] == '/' {
			n--
			if n == 0 {
				return i + 1, true
			}
		}
	}
	return 0, n == 1
}

func equal[T text](t T, s string) bool {
	if len(t) != len(s) {
		return false
	}
	for i := 0; i < len(s); i++ {
		if t[i] != s[i] {
			return false
		}
	}
	return true
}

// matchSubject applies the rule's pattern to a fully assembled subject.
func matchSubject[T text](r *Rule, t T) bool {
	switch r.mode {
	case modeTail:
		off, ok := tailStart(t, r.tailN)
		if !ok {
			return false
		}
		if r.kind == fastLiteral {
			return equal(t[off:], r.lit)
		}
		return wild(r.pat, t[off:]) == wmTrue
	case modeAnywhere:
		// Only wildcard patterns contain "**".
		return wildAnywhere(r.pat, t)
	default:
		if r.kind == fastLiteral {
			return equal(t, r.lit)
		}
		return wild(r.pat, t) == wmTrue
	}
}

// name is a preprocessed matcher name, computed once per Match call.
type name struct {
	full     string // without one leading '/'
	base     string // last component
	rooted   bool   // the original name had a leading '/'
	isDir    bool
	cwdRel   string // current dir relative to the module root, no leading '/'
	xattr    bool
	nonEmpty bool
}

func makeName(s string, isDir bool, cwdRel string, xattr bool) name {
	n := name{full: s, isDir: isDir, cwdRel: cwdRel, xattr: xattr}
	if strings.HasPrefix(s, "/") {
		n.full = s[1:]
		n.rooted = true
	}
	n.base = n.full
	if i := strings.LastIndexByte(n.full, '/'); i >= 0 {
		n.base = n.full[i+1:]
	}
	n.nonEmpty = n.full != ""
	return n
}

// matches reports whether rule r fires for n (taking negation into account).
func (r *Rule) matches(n *name) bool {
	hit := r.Flags&FlagNegate == 0
	if !n.nonEmpty || n.xattr != (r.Flags&FlagXattr != 0) {
		return false
	}
	if !n.isDir && r.Flags&FlagDirectory != 0 {
		return !hit
	}
	if r.mode == modeBase {
		b := n.base
		var ok bool
		switch r.kind {
		case fastLiteral:
			ok = b == r.lit
		case fastSuffix:
			ok = strings.HasSuffix(b, r.lit)
		case fastPrefix:
			ok = strings.HasPrefix(b, r.lit)
		case fastContains:
			ok = strings.Contains(b, r.lit)
		case fastAny:
			ok = true
		default:
			ok = wild(r.pat, b) == wmTrue
		}
		if ok {
			return hit
		}
		return !hit
	}
	// Assemble the subject: an optional prefix ("<cwd>/" for absolute-path
	// rules, "/" so a leading "**/" can match a top-level name), the name,
	// and a trailing "/" so "dir/***" matches the directory itself.
	var pre string
	prefixed := false
	switch {
	case r.Flags&FlagAbsPath != 0 && !n.rooted && n.cwdRel != "":
		pre, prefixed = n.cwdRel, true
	case r.Flags&flagWild2Prefix != 0 && !n.rooted:
		prefixed = true
	}
	trail := n.isDir && r.Flags&flagWild3Suffix != 0
	var ok bool
	if !prefixed && !trail {
		ok = matchSubject(r, n.full)
	} else {
		var buf [512]byte
		b := buf[:0]
		if prefixed {
			b = append(b, pre...)
			b = append(b, '/')
		}
		b = append(b, n.full...)
		if trail {
			b = append(b, '/')
		}
		ok = matchSubject(r, b)
	}
	if ok {
		return hit
	}
	return !hit
}
