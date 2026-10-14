// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package rsync

import (
	"io"
	"time"
)

// rateLimiter paces reads to at most rate bytes per second (--bwlimit).
type rateLimiter struct {
	r     io.Reader
	rate  int64
	start time.Time
	n     int64
}

func newRateLimiter(r io.Reader, rate int64) *rateLimiter {
	return &rateLimiter{r: r, rate: rate, start: time.Now()}
}

func (l *rateLimiter) Read(p []byte) (int, error) {
	if max := int(l.rate / 10); max > 0 && len(p) > max {
		p = p[:max]
	}
	n, err := l.r.Read(p)
	l.n += int64(n)
	want := time.Duration(float64(l.n) / float64(l.rate) * float64(time.Second))
	if d := want - time.Since(l.start); d > 0 {
		time.Sleep(d)
	}
	return n, err
}
