// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package rsync

import (
	"io"
	"time"

	"github.com/parallelworks/xfer/rsync/internal/wire"
)

// runTimed runs fn, the session's protocol, under --timeout. It returns
// the timeout error as soon as the timeout expires, even if fn is still
// blocked in a read that closing the connection cannot interrupt (a stdio
// pipe); fn then ends when the process does.
func (s *session) runTimed(fn func() error) error {
	timeout, conTimeout := s.opts.Timeout, s.opts.ConTimeout
	if s.server {
		conTimeout = 0
	}
	if timeout <= 0 && conTimeout <= 0 {
		return fn()
	}
	fired, stop := s.watchdog(timeout, conTimeout)
	defer stop()
	res := make(chan error, 1)
	go func() { res <- fn() }()
	select {
	case err := <-res:
		if err != nil && s.timedOut.Load() {
			return s.timeoutError()
		}
		return err
	case <-fired:
		return s.timeoutError()
	}
}

// watchdog enforces --timeout: it fires once a read or write has been
// blocked for the whole timeout, closing the connection, and keeps the
// connection alive by sending a no-op message whenever this side has
// written nothing for half of it, so a peer with the same timeout does not
// give up on us while we are busy.
//
// conTimeout, if set, limits how long the protocol setup may take.
func (s *session) watchdog(timeout, conTimeout time.Duration) (fired <-chan struct{}, stop func()) {
	tick := time.Second
	for _, d := range []time.Duration{timeout, conTimeout} {
		if d > 0 {
			tick = min(tick, d/4)
		}
	}
	start := time.Now()
	done, fire := make(chan struct{}), make(chan struct{})
	expire := func(conn bool) {
		if conn {
			s.conTimedOut.Store(true)
		}
		s.timedOut.Store(true)
		close(fire)
		if cl, ok := s.raw.(io.Closer); ok {
			cl.Close()
		} else if cl, ok := s.counter.rw.(io.Closer); ok {
			cl.Close()
		}
	}
	go func() {
		t := time.NewTicker(tick)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case now := <-t.C:
				c := s.counter
				if conTimeout > 0 && !s.started.Load() && now.Sub(start) >= conTimeout {
					expire(true)
					return
				}
				if timeout <= 0 {
					continue
				}
				blocked := func(since int64) bool { return since != 0 && now.Sub(time.Unix(0, since)) >= timeout }
				if blocked(c.readSince.Load()) || blocked(c.writeSince.Load()) {
					expire(false)
					return
				}
				if s.keepalive.Load() && c.writeSince.Load() == 0 && now.Sub(time.Unix(0, c.lastWrite.Load())) >= timeout/2 {
					s.out.TrySendMsg(wire.MsgNoop, nil)
				}
			}
		}
	}()
	return fire, func() { close(done) }
}

// timeoutError is the error a session that timed out ends with.
func (s *session) timeoutError() error {
	if s.conTimedOut.Load() {
		return exitErrorf(ExitConTimeout, "timeout waiting for the remote rsync to start (--contimeout=%d)", int(s.opts.ConTimeout/time.Second))
	}
	return exitErrorf(ExitTimeout, "[%s] io timeout after %d seconds -- exiting", s.roleName(), int(s.opts.Timeout/time.Second))
}

func (s *session) roleName() string {
	if s.sender {
		return "sender"
	}
	return "receiver"
}
