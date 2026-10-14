// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package rsync

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/parallelworks/xfer/rsync/internal/wire"
)

// TestTimeoutKeepalive checks that an idle session sends keep-alives and
// gives up with exit code 30 once its peer has been silent for the timeout.
func TestTimeoutKeepalive(t *testing.T) {
	ours, peer := net.Pipe()
	defer peer.Close()
	o := &Options{Timeout: time.Second}
	s := newSession(o, ours, true, false, newLogger(o, io.Discard, io.Discard))
	s.out.Mux.Enable()
	s.keepalive.Store(true)

	var noops atomic.Int32
	go func() {
		var hdr [4]byte
		for {
			if _, err := io.ReadFull(peer, hdr[:]); err != nil {
				return
			}
			v := binary.LittleEndian.Uint32(hdr[:])
			if int(v>>24)-7 == wire.MsgNoop && v&0xffffff == 0 {
				noops.Add(1)
			}
		}
	}()

	start := time.Now()
	err := s.runTimed(func() error {
		_, err := s.raw.Read(make([]byte, 1))
		return err
	})
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != ExitTimeout {
		t.Fatalf("got %v, want exit %d", err, ExitTimeout)
	}
	if d := time.Since(start); d < time.Second || d > 3*time.Second {
		t.Errorf("timed out after %v, want about 1s", d)
	}
	if noops.Load() == 0 {
		t.Error("no keep-alive was sent")
	}
}
