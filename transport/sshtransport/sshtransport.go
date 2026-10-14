// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

// Package sshtransport runs remote commands over golang.org/x/crypto/ssh
// connections. Programs that already hold an SSH connection, such as the pw
// CLI, use it to run transfers without spawning an ssh binary.
package sshtransport

import (
	"context"
	"io"
	"os"
	"sync"

	"golang.org/x/crypto/ssh"

	"github.com/parallelworks/xfer/transport"
)

// Dialer returns an SSH client for a host. The returned release function is
// called once the command started on that client has finished; it may close
// the client or return it to a pool.
type Dialer func(ctx context.Context, host string) (client *ssh.Client, release func(), err error)

// Transport is a transport.Transport that runs commands in SSH sessions.
type Transport struct {
	Dial Dialer

	// Stderr receives the remote command's standard error. If nil,
	// os.Stderr is used.
	Stderr io.Writer
}

var _ transport.Transport = (*Transport)(nil)

// New returns a Transport that runs every command on client, whatever host
// is asked for.
func New(client *ssh.Client) *Transport {
	return &Transport{Dial: func(context.Context, string) (*ssh.Client, func(), error) {
		return client, func() {}, nil
	}}
}

// Start implements transport.Transport.
func (t *Transport) Start(ctx context.Context, host string, argv []string) (transport.Conn, error) {
	client, release, err := t.Dial(ctx, host)
	if err != nil {
		return nil, err
	}
	sess, err := client.NewSession()
	if err != nil {
		release()
		return nil, err
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		sess.Close()
		release()
		return nil, err
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		sess.Close()
		release()
		return nil, err
	}
	sess.Stderr = t.Stderr
	if sess.Stderr == nil {
		sess.Stderr = os.Stderr
	}
	if err := sess.Start(transport.QuoteCommand(argv)); err != nil {
		sess.Close()
		release()
		return nil, err
	}
	c := &conn{sess: sess, stdin: stdin, stdout: stdout, release: release, done: make(chan struct{})}
	go func() {
		select {
		case <-ctx.Done():
			c.Close()
		case <-c.done:
		}
	}()
	return c, nil
}

type conn struct {
	sess    *ssh.Session
	stdin   io.WriteCloser
	stdout  io.Reader
	release func()

	once sync.Once
	done chan struct{}
}

func (c *conn) Read(p []byte) (int, error)  { return c.stdout.Read(p) }
func (c *conn) Write(p []byte) (int, error) { return c.stdin.Write(p) }
func (c *conn) CloseWrite() error           { return c.stdin.Close() }

func (c *conn) Wait() error {
	c.stdin.Close()
	err := c.sess.Wait()
	c.finish()
	return err
}

func (c *conn) Close() error {
	err := c.sess.Close()
	c.finish()
	return err
}

func (c *conn) finish() {
	c.once.Do(func() {
		close(c.done)
		c.release()
	})
}
