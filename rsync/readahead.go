// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package rsync

import (
	"bytes"
	"io"
	"sync"
)

// readAhead drains r in a goroutine so the peer is never blocked writing
// to us. A sender needs this: it reads only between files, and a peer that
// stops reading while its own writes are stuck would otherwise deadlock
// against a large file in flight. What a sender receives (requests and
// block sums) is small next to what it sends.
type readAhead struct {
	mu     sync.Mutex
	cond   sync.Cond
	chunks [][]byte
	err    error
}

func newReadAhead(r io.Reader) *readAhead {
	ra := &readAhead{}
	ra.cond.L = &ra.mu
	go ra.fill(r)
	return ra
}

func (ra *readAhead) fill(r io.Reader) {
	buf := make([]byte, 64<<10)
	for {
		n, err := r.Read(buf)
		ra.mu.Lock()
		if n > 0 {
			ra.chunks = append(ra.chunks, bytes.Clone(buf[:n]))
		}
		if err != nil {
			ra.err = err
		}
		ra.cond.Signal()
		ra.mu.Unlock()
		if err != nil {
			return
		}
	}
}

func (ra *readAhead) Read(p []byte) (int, error) {
	ra.mu.Lock()
	defer ra.mu.Unlock()
	for len(ra.chunks) == 0 && ra.err == nil {
		ra.cond.Wait()
	}
	if len(ra.chunks) == 0 {
		return 0, ra.err
	}
	n := copy(p, ra.chunks[0])
	if ra.chunks[0] = ra.chunks[0][n:]; len(ra.chunks[0]) == 0 {
		ra.chunks[0] = nil
		ra.chunks = ra.chunks[1:]
	}
	return n, nil
}
