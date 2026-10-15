// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

// Package token encodes and decodes the rsync sender→receiver token stream:
// the per-file sequence of literal data and references to basis-file blocks.
//
// Five encodings exist. Simple is the uncompressed int32 framing. The
// compressed kinds share a flag-byte framing (END_FLAG, TOKEN_REL,
// TOKENRUN_REL, TOKEN_LONG, TOKENRUN_LONG, DEFLATED_DATA records) and differ
// in how literal bytes are compressed:
//
//   - Zlib (plain -z): raw deflate, reset per file, whose history includes
//     the bytes of matched blocks as well as literals.
//   - Zlibx (--new-compress / zlibx): like Zlib but the history contains
//     only literals.
//   - Zstd: one zstd frame for the whole session that is never ended.
//   - LZ4: every DEFLATED_DATA record is an independent LZ4 block.
//
// A Writer or Reader is created once per session and used for every file;
// End (writer) and a done token (reader) delimit files.
package token

import (
	"errors"
	"fmt"

	"github.com/parallelworks/xfer/rsync/internal/wire"
)

// Kind is a token-stream encoding.
type Kind int

const (
	Simple Kind = iota
	Zlib
	Zlibx
	Zstd
	LZ4
)

func (k Kind) String() string {
	switch k {
	case Simple:
		return "none"
	case Zlib:
		return "zlib"
	case Zlibx:
		return "zlibx"
	case Zstd:
		return "zstd"
	case LZ4:
		return "lz4"
	}
	return fmt.Sprintf("Kind(%d)", int(k))
}

// Protocol constants.
const (
	// ChunkSize is the largest literal run a Simple sender puts in one record.
	ChunkSize = 32 << 10
	// MaxDataCount is the largest DEFLATED_DATA payload.
	MaxDataCount = 16383
	// maxBlockIndex is the largest acceptable block index.
	maxBlockIndex = 0x7FFFFFFE
)

// Flag bytes of the compressed framing.
const (
	flagEnd          = 0x00
	flagTokenLong    = 0x20
	flagTokenRunLong = 0x21
	flagDeflated     = 0x40
	flagTokenRel     = 0x80
	flagTokenRunRel  = 0xC0
)

// Writer encodes one session's token streams. A single Writer is used for
// every file of a session.
type Writer interface {
	// Literal appends literal data, of any length, to the current file.
	Literal(p []byte) error
	// Match appends a reference to basis block `block`; data is the
	// block's bytes (the compressing kinds may add them to their history).
	Match(block int32, data []byte) error
	// End terminates the current file's token stream.
	End() error
}

// Reader decodes one session's token streams.
type Reader interface {
	// Next returns the next token: literal data (lit != nil, valid until
	// the next call), or a matched block index (block >= 0, lit == nil),
	// or the end of the file (done). After done the next call starts the
	// next file's stream.
	Next() (lit []byte, block int32, done bool, err error)
	// SeeMatch must be called with the basis block's bytes after Next
	// returned a block, so compressing kinds can add it to their history.
	SeeMatch(data []byte)
	// Close releases resources (the zstd decoder goroutine). It does not
	// touch the wire. Safe to call more than once.
	Close() error
}

// ErrBadToken reports a malformed token stream.
var ErrBadToken = fmt.Errorf("%w: bad token stream", wire.ErrProtocol)

func badf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrBadToken, fmt.Sprintf(format, args...))
}

var errBadBlock = errors.New("token: negative block index")

// DefaultLevel returns the compression level a level of 0 selects for k.
func DefaultLevel(k Kind) int {
	switch k {
	case Zlib, Zlibx:
		return 6
	case Zstd:
		return 3
	}
	return 0
}

// FileCompressor is implemented by Writers that can leave a file's data
// uncompressed (--skip-compress). The zlib kinds can; zstd and LZ4 compress
// every file. CompressFile is called before a file's first token.
type FileCompressor interface {
	CompressFile(on bool)
}

// NewWriter returns a Writer encoding kind k to w for protocol proto.
// level 0 selects the kind's default level.
func NewWriter(k Kind, w *wire.Writer, proto int, level int) Writer {
	if level == 0 {
		level = DefaultLevel(k)
	}
	switch k {
	case Simple:
		return &simpleWriter{w: w}
	case Zlib, Zlibx:
		return newCompWriter(w, newZlibEnc(w, k == Zlib, proto < 31, level))
	case Zstd:
		return newCompWriter(w, newZstdEnc(w, level))
	case LZ4:
		return newCompWriter(w, newLZ4Enc(w))
	}
	panic(fmt.Sprintf("token: unknown kind %d", k))
}

// NewReader returns a Reader decoding kind k from r for protocol proto.
func NewReader(k Kind, r *wire.Reader, proto int) Reader {
	switch k {
	case Simple:
		return &simpleReader{r: r}
	case Zlib, Zlibx:
		return newCompReader(r, newZlibDec(r, k == Zlib, proto < 31))
	case Zstd:
		return newCompReader(r, newZstdDec(r))
	case LZ4:
		return newCompReader(r, newLZ4Dec(r))
	}
	panic(fmt.Sprintf("token: unknown kind %d", k))
}
