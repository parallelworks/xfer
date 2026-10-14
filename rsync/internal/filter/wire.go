// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package filter

// ListExchanged reports whether the client sends (and the server reads) a
// filter list for a remote (non-local) transfer. If it returns false nothing
// at all is transmitted, not even the terminating zero length.
func ListExchanged(proto int, clientIsSender, deleteMode, deleteExcluded, pruneEmptyDirs bool) bool {
	receiverWantsList := pruneEmptyDirs || (deleteMode && (!deleteExcluded || proto >= 29))
	return !clientIsSender || receiverWantsList
}

// ParseWireRule parses one rule string received from the client (server
// side). For protocol 29 and newer the full rule syntax applies; older
// peers use the --exclude syntax (bare pattern = exclude, "+ " / "- "
// prefixes, "!" clears). The caller should feed the results, in order and
// followed by any CVS rules, to NewListWithOptions. A "." rule would be
// expanded using os.ReadFile; use Parser.ParseWire to control that and to
// apply --delete-excluded.
func ParseWireRule(s string, proto int) ([]Rule, error) {
	var p Parser
	return p.ParseWire(s, proto)
}

// ParseWire is ParseWireRule with the parser's settings.
func (p *Parser) ParseWire(s string, proto int) ([]Rule, error) {
	c := p.ctx()
	return c.parseString(nil, s, 0, proto < 29)
}

// WireRules returns the rule strings a client transmits for this list (in
// order; each is sent as an int32 length followed by the bytes, and the list
// is terminated by an int32 0). Rules meant only for the client's own side
// are omitted, "-C" is expanded when the server cannot build the CVS list
// itself, and prefixes are downgraded for old protocols. ErrTooModern is
// returned if a rule cannot be expressed in protocol proto.
//
// With --cvs-exclude the caller appends ":C" and "-C" before calling this
// (when the client is the sender; ":C" only for proto >= 29), as rsync does.
func (l *List) WireRules(proto int, clientIsSender bool, deleteExcluded bool) ([]string, error) {
	return l.wireRules(nil, l.rules, proto, clientIsSender, deleteExcluded, true)
}

const (
	elideNone = iota
	elideLocal
	elideRemote
)

func (l *List) wireRules(out []string, rules []Rule, proto int, amSender, deleteExcluded, send bool) ([]string, error) {
	for i := range rules {
		r := &rules[i]
		elide := elideNone
		if r.Flags&FlagSenderSide != 0 {
			elide = pick(amSender, elideLocal, elideRemote)
		}
		if r.Flags&FlagReceiverSide != 0 {
			if elide != elideNone {
				elide = elideNone
			} else {
				elide = pick(amSender, elideRemote, elideLocal)
			}
		} else if deleteExcluded && elide == elideNone &&
			(r.Flags&FlagPerDirMerge == 0 || r.Flags&FlagNoPrefixes != 0) {
			elide = pick(amSender, elideLocal, elideRemote)
		}
		if elide == elideLocal {
			continue
		}
		if r.isCVSPlaceholder() {
			expand := amSender || proto < 29
			var err error
			out, err = l.wireRules(out, l.cvs, proto, amSender, deleteExcluded, send && expand)
			if err != nil {
				return out, err
			}
			if expand {
				continue
			}
		}
		prefix, ok := buildPrefix(r, true, proto, amSender, deleteExcluded)
		if !ok {
			return out, ErrTooModern
		}
		if !send {
			continue
		}
		s := prefix + r.Pattern
		if r.Flags&FlagDirectory != 0 {
			s += "/"
		}
		if s != "" {
			out = append(out, s)
		}
	}
	return out, nil
}

func pick(c bool, a, b int) int {
	if c {
		return a
	}
	return b
}

// buildPrefix renders the rule prefix (including the separating space when
// one is needed). For the wire (forWire) it adapts to protocol proto and
// returns ok=false when the rule cannot be expressed.
func buildPrefix(r *Rule, forWire bool, proto int, amSender, deleteExcluded bool) (string, bool) {
	const unlimited = 16
	legal := unlimited
	if forWire && proto < 29 {
		legal = 1
	}
	var b [unlimited + 1]byte
	n := 0
	put := func(c byte) { b[n] = c; n++ }
	switch {
	case r.Flags&FlagPerDirMerge != 0:
		if legal == 1 {
			return "", false
		}
		put(':')
	case r.Flags&FlagMerge != 0:
		put('.')
	case r.Flags&FlagInclude != 0:
		put('+')
	case legal != 1 || len(r.Pattern) >= 2 && (r.Pattern[0] == '-' || r.Pattern[0] == '+') && r.Pattern[1] == ' ':
		put('-')
	default:
		legal = 0
	}
	if r.Flags&FlagAbsPath != 0 {
		put('/')
	}
	if r.Flags&FlagNegate != 0 {
		put('!')
	}
	if r.Flags&FlagCVSIgnore != 0 {
		put('C')
	} else {
		if r.Flags&FlagNoInherit != 0 {
			put('n')
		}
		if r.Flags&FlagWordSplit != 0 {
			put('w')
		}
		if r.Flags&FlagNoPrefixes != 0 {
			if r.Flags&FlagInclude != 0 {
				put('+')
			} else {
				put('-')
			}
		}
	}
	if r.Flags&FlagExcludeSelf != 0 {
		put('e')
	}
	if r.Flags&FlagXattr != 0 {
		put('x')
	}
	if r.Flags&FlagSenderSide != 0 && (!forWire || proto >= 29) {
		put('s')
	}
	if r.Flags&FlagReceiverSide != 0 && (!forWire || proto >= 29 || (deleteExcluded && amSender)) {
		put('r')
	}
	if r.Flags&FlagPerishable != 0 {
		if !forWire || proto >= 30 {
			put('p')
		} else if amSender {
			return "", false
		}
	}
	if n > legal {
		return "", false
	}
	if legal > 0 {
		put(' ')
	}
	return string(b[:n]), true
}
