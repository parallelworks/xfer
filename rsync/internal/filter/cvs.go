// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package filter

import (
	"os"
	"path/filepath"
)

// DefaultCVSIgnore is the built-in list of CVS-ignored patterns used by -C,
// in the order rsync documents them.
const DefaultCVSIgnore = "RCS SCCS CVS CVS.adm RCSLOG cvslog.* tags TAGS .make.state .nse_depinfo *~ " +
	"#* .#* ,* _$* *$ *.old *.bak *.BAK *.orig *.rej .del-* *.a *.olb *.o *.obj *.so *.exe " +
	"*.Z *.elc *.ln core .svn/ .git/ .hg/ .bzr/"

// cvsTemplate is the flag set of a "-C" rule.
const cvsTemplate = FlagNoPrefixes | FlagWordSplit | FlagNoInherit | FlagCVSIgnore

// CVSConfig controls how the CVS exclude list (inserted by "-C" rules and
// the --cvs-exclude option) is built.
type CVSConfig struct {
	// Protocol is the negotiated protocol version. The built-in patterns are
	// perishable when it is 30 or newer; 0 means "current" (perishable).
	Protocol int
	// HomeIgnore returns the contents of $HOME/.cvsignore (or /.cvsignore in
	// a chrooted daemon module). Nil, or any error, means none.
	HomeIgnore func() ([]byte, error)
	// Env is the value of $CVSIGNORE (whitespace-separated patterns).
	Env string
}

// DefaultCVSConfig returns a CVSConfig that reads $HOME/.cvsignore and
// $CVSIGNORE from the process environment.
func DefaultCVSConfig(proto int) CVSConfig {
	c := CVSConfig{Protocol: proto, Env: os.Getenv("CVSIGNORE")}
	if home := os.Getenv("HOME"); home != "" {
		c.HomeIgnore = func() ([]byte, error) {
			return os.ReadFile(filepath.Join(home, ".cvsignore"))
		}
	}
	return c
}

// CVSRules returns the built-in CVS exclude rules (perishable, as sent with
// protocol 30 and newer). It does not consult $HOME/.cvsignore or
// $CVSIGNORE; use BuildCVSRules for that.
func CVSRules() []Rule {
	rules, err := BuildCVSRules(CVSConfig{})
	if err != nil {
		panic(err) // the built-in list always parses
	}
	return rules
}

// BuildCVSRules builds the CVS exclude list: the built-in patterns, then
// $HOME/.cvsignore, then $CVSIGNORE, each parsed like a .cvsignore file
// (whitespace-separated bare exclude patterns; a "!" token clears the list
// built so far).
func BuildCVSRules(cfg CVSConfig) ([]Rule, error) {
	return buildCVS(cfg, cvsTemplate, false)
}

// buildCVS builds the list using the flags of the "-C" rule that triggered
// it as the template.
func buildCVS(cfg CVSConfig, tmpl Flags, deleteExcluded bool) ([]Rule, error) {
	c := &parseCtx{deleteExcluded: deleteExcluded, readFile: os.ReadFile}
	tmpl |= cvsTemplate
	def := tmpl
	if cfg.Protocol == 0 || cfg.Protocol >= 30 {
		def |= FlagPerishable
	}
	rules, err := c.parseString(nil, DefaultCVSIgnore, def, false)
	if err != nil {
		return nil, err
	}
	if cfg.HomeIgnore != nil {
		// Like rsync, an unreadable ~/.cvsignore is silently ignored.
		if data, rerr := cfg.HomeIgnore(); rerr == nil {
			if rules, err = c.parseData(rules, data, tmpl, false); err != nil {
				return nil, err
			}
		}
	}
	if rules, err = c.parseString(rules, cfg.Env, tmpl, false); err != nil {
		return nil, err
	}
	return dropClear(rules), nil
}

// dropClear removes a leading clear marker.
func dropClear(rules []Rule) []Rule {
	if len(rules) > 0 && rules[0].Flags&FlagClear != 0 {
		return rules[1:]
	}
	return rules
}
