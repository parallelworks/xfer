// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package rsync

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Exit codes, as documented by rsync.
const (
	ExitOK            = 0
	ExitSyntax        = 1  // syntax or usage error
	ExitProtocol      = 2  // protocol incompatibility
	ExitFileSelect    = 3  // errors selecting input/output files, dirs
	ExitUnsupported   = 4  // requested action not supported
	ExitStartClient   = 5  // error starting client-server protocol
	ExitSocketIO      = 10 // error in socket I/O
	ExitFileIO        = 11 // error in file I/O
	ExitStreamIO      = 12 // error in rsync protocol data stream
	ExitMessageIO     = 13 // errors with program diagnostics
	ExitIPC           = 14 // error in IPC code
	ExitSignal        = 20 // received SIGUSR1 or SIGINT
	ExitPartial       = 23 // partial transfer due to error
	ExitVanished      = 24 // partial transfer due to vanished source files
	ExitDeleteLimit   = 25 // the --max-delete limit stopped deletions
	ExitTimeout       = 30 // timeout in data send/receive
	ExitConTimeout    = 35 // timeout waiting for the connection
	ExitRemoteCommand = 255
)

// I/O error bits carried by the protocol.
const (
	ioErrGeneral   = 1 << 0
	ioErrVanished  = 1 << 1
	ioErrDelLimit  = 1 << 2
	ioErrValidMask = 7
)

// ExitError is an error with an rsync exit code.
type ExitError struct {
	Code int   // rsync exit code, such as ExitStreamIO
	Err  error // underlying error; may be nil
}

// Error returns the underlying error's message, or a generic message
// naming the exit code when there is none.
func (e *ExitError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("rsync error: exit code %d", e.Code)
	}
	return e.Err.Error()
}

// Unwrap returns the underlying error.
func (e *ExitError) Unwrap() error { return e.Err }

// ExitCode returns the rsync exit code for err: 0 for nil, the code of an
// *ExitError, and ExitStreamIO for anything else.
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	var ee *ExitError
	if errors.As(err, &ee) {
		return ee.Code
	}
	var ue *UsageError
	if errors.As(err, &ue) {
		return ExitSyntax
	}
	return ExitStreamIO
}

func exitErrorf(code int, format string, args ...any) error {
	format, args = rsyncQuote(format, args, escapeName)
	return &ExitError{Code: code, Err: fmt.Errorf(format, args...)}
}

// rsyncQuote rewrites the %q verbs of format, which quote names, to quote
// them as rsync does: in plain double quotes, with the characters that
// safe escapes written as \#ooo. Go's %q would escape backslashes, quotes
// and non-ASCII characters instead.
func rsyncQuote(format string, args []any, safe func(string) string) (string, []any) {
	if !strings.Contains(format, "%q") {
		return format, args
	}
	var b strings.Builder
	out := slices.Clone(args)
	n := 0 // the argument the next verb takes
	for i := 0; i < len(format); i++ {
		c := format[i]
		b.WriteByte(c)
		if c != '%' || i+1 == len(format) {
			continue
		}
		i++
		if format[i] == '%' {
			b.WriteByte('%')
			continue
		}
		for i+1 < len(format) && strings.IndexByte("+-# 0123456789.", format[i]) >= 0 {
			b.WriteByte(format[i]) // flags, width and precision
			i++
		}
		var name string
		ok := false
		if n < len(out) {
			name, ok = out[n].(string)
		}
		if format[i] == 'q' && ok {
			out[n] = `"` + safe(name) + `"`
			b.WriteByte('s')
		} else {
			b.WriteByte(format[i])
		}
		n++
	}
	return b.String(), out
}

// escapeName escapes a name for messages where no output options apply.
func escapeName(name string) string {
	return (&logger{opts: &Options{}}).safe(name)
}

// codeForIOError maps accumulated I/O error bits to an exit code.
func codeForIOError(bits int32) int {
	switch {
	case bits&ioErrGeneral != 0:
		return ExitPartial
	case bits&ioErrVanished != 0:
		return ExitVanished
	case bits&ioErrDelLimit != 0:
		return ExitDeleteLimit
	}
	return ExitOK
}

func randomSeed() int32 {
	var b [4]byte
	rand.Read(b[:])
	return int32(binary.LittleEndian.Uint32(b[:]) & 0x7fffffff)
}
