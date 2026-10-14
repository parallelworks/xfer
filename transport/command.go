// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Command is a Transport that runs an external remote shell program, ssh by
// default, the way rsync's -e/--rsh option does.
//
// The remote command line is passed to the remote shell as a single string
// with each argument quoted for a POSIX shell, so paths containing spaces or
// shell metacharacters arrive intact.
type Command struct {
	// Shell is the remote shell command and its arguments, for example
	// []string{"ssh", "-p", "2222"}. If empty, "ssh" is used.
	Shell []string

	// Stderr receives the remote shell's standard error. If nil, os.Stderr
	// is used.
	Stderr io.Writer

	// Env, if non-nil, is the environment of the remote shell process.
	Env []string
}

// ParseShell splits a remote shell command written the way rsync's -e option
// accepts it. Words are separated by whitespace; single and double quotes
// group words, and a backslash escapes the next character outside single
// quotes.
func ParseShell(s string) ([]string, error) {
	var (
		args    []string
		cur     strings.Builder
		inWord  bool
		quote   rune
		escaped bool
	)
	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case quote == '\'':
			if r == '\'' {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\\':
			escaped, inWord = true, true
		case quote == '"':
			if r == '"' {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, inWord = r, true
		case r == ' ' || r == '\t' || r == '\n':
			if inWord {
				args = append(args, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 || escaped {
		return nil, fmt.Errorf("unterminated quote or escape in remote shell %q", s)
	}
	if inWord {
		args = append(args, cur.String())
	}
	return args, nil
}

// Start implements Transport.
func (c *Command) Start(ctx context.Context, host string, argv []string) (Conn, error) {
	shell := c.Shell
	if len(shell) == 0 {
		shell = []string{"ssh"}
	}
	args := append([]string(nil), shell[1:]...)
	user, h, ok := strings.Cut(host, "@")
	if !ok {
		user, h = "", host
	}
	if user != "" {
		args = append(args, "-l", user)
	}
	args = append(args, h, QuoteCommand(argv))

	cmd := exec.Command(shell[0], args...)
	if c.Env != nil {
		cmd.Env = c.Env
	}
	cmd.Stderr = c.Stderr
	if cmd.Stderr == nil {
		cmd.Stderr = os.Stderr
	}
	return startCmd(ctx, cmd)
}

// Exec returns a Transport that ignores the host and runs argv locally as a
// child process. It is useful for tests and for local transfers that want a
// separate server process.
func Exec(stderr io.Writer) Transport {
	return Func(func(ctx context.Context, _ string, argv []string) (Conn, error) {
		if len(argv) == 0 {
			return nil, errors.New("transport: empty command")
		}
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Stderr = stderr
		if cmd.Stderr == nil {
			cmd.Stderr = os.Stderr
		}
		return startCmd(ctx, cmd)
	})
}

func startCmd(ctx context.Context, cmd *exec.Cmd) (Conn, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	for _, p := range []any{stdin, stdout} {
		if f, ok := p.(*os.File); ok {
			GrowPipe(f)
		}
	}
	c := &cmdConn{cmd: cmd, stdin: stdin, stdout: stdout, done: make(chan struct{})}
	go func() {
		select {
		case <-ctx.Done():
			c.Close()
		case <-c.done:
		}
	}()
	return c, nil
}

type cmdConn struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser

	closeOnce sync.Once
	done      chan struct{}
}

func (c *cmdConn) Read(p []byte) (int, error)  { return c.stdout.Read(p) }
func (c *cmdConn) Write(p []byte) (int, error) { return c.stdin.Write(p) }
func (c *cmdConn) CloseWrite() error           { return c.stdin.Close() }

func (c *cmdConn) Wait() error {
	err := c.cmd.Wait()
	c.closeOnce.Do(func() { close(c.done) })
	return err
}

// killGrace is how long Close lets the command exit on its own after its
// input ends, so a remote rsync can clean up (keeping --partial data, for
// example) before it is killed.
const killGrace = 2 * time.Second

func (c *cmdConn) Close() error {
	c.stdin.Close()
	if c.cmd.Process != nil {
		time.AfterFunc(killGrace, func() {
			select {
			case <-c.done:
			default:
				c.cmd.Process.Kill() // an error here means it already exited
			}
		})
	}
	return nil
}

// QuoteCommand joins argv into a single POSIX shell command line, quoting
// each argument only when necessary.
func QuoteCommand(argv []string) string {
	var b strings.Builder
	for i, a := range argv {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(Quote(a))
	}
	return b.String()
}

// Quote quotes s for a POSIX shell the way rsync quotes remote arguments:
// shell metacharacters are backslash-escaped, but the wildcards *, ? and [
// are left alone so the remote side can expand them. Strings containing
// line breaks are single-quoted instead.
func Quote(s string) string {
	if s == "" {
		return "''"
	}
	if strings.ContainsAny(s, "\n\r") {
		return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
	}
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(" \t!\"#$&'();<>\\`{|}~", r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
