// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

// Package transport starts the remote half of a file transfer.
//
// A transfer tool such as rsync runs a peer process on the remote host and
// talks to it over that process's standard input and output. Transport is the
// seam that hides how the peer is reached: by running an external ssh binary
// ([Command]), over an existing golang.org/x/crypto/ssh connection
// ([sshtransport]), or by any other means an embedding program provides.
package transport

import (
	"context"
	"io"
)

// A Transport starts argv on a remote host and returns a connection to its
// standard input and output.
//
// host is the host part of a remote path as the user wrote it, for example
// "cluster", "alice@cluster" or "cluster.example.com". Transports interpret
// it however makes sense for them.
type Transport interface {
	Start(ctx context.Context, host string, argv []string) (Conn, error)
}

// Conn is a running remote command.
//
// Reads return the command's standard output and writes go to its standard
// input. Standard error is the Transport's concern; implementations usually
// copy it to a configured writer.
type Conn interface {
	io.ReadWriter

	// CloseWrite closes the command's standard input.
	CloseWrite() error

	// Wait waits for the command to exit and reports how it exited.
	// It must be called exactly once, after the caller is done reading.
	Wait() error

	// Close aborts the command and releases its resources. It is safe to
	// call Close after Wait.
	Close() error
}

// Func adapts a function to the Transport interface.
type Func func(ctx context.Context, host string, argv []string) (Conn, error)

// Start calls f.
func (f Func) Start(ctx context.Context, host string, argv []string) (Conn, error) {
	return f(ctx, host, argv)
}
