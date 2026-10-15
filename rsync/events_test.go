// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package rsync_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/parallelworks/xfer/rsync"
	"github.com/parallelworks/xfer/transport"
)

// TestEvents checks the event stream of quiet transfers, local and over a
// remote shell in both directions.
func TestEvents(t *testing.T) {
	bin := xferBinary(t)
	for _, mode := range []string{"local", "push", "pull"} {
		t.Run(mode, func(t *testing.T) {
			base := t.TempDir()
			src, dst := filepath.Join(base, "src"), filepath.Join(base, "dst")
			os.Mkdir(src, 0o755)
			os.Mkdir(dst, 0o755)
			sourceTree(3).write(t, src)
			os.WriteFile(filepath.Join(dst, "stale"), []byte("x"), 0o644)

			o, _, err := rsync.ParseArgs([]string{"-a", "--delete"})
			if err != nil {
				t.Fatal(err)
			}
			if mode != "local" {
				o.RsyncPath = bin + " rsync"
			}
			var events []rsync.Event
			c := &rsync.Client{Stdout: io.Discard, Stderr: io.Discard, Events: func(e rsync.Event) {
				events = append(events, e)
			}}
			if mode != "local" {
				c.Transport = transport.Exec(io.Discard)
			}
			from, to := src+"/", dst+"/"
			switch mode {
			case "push":
				to = "host:" + to
			case "pull":
				from = "host:" + from
			}
			if _, err := c.Run(context.Background(), o, []string{from, to}); err != nil {
				t.Fatal(err)
			}

			items := map[string]rsync.Event{}
			progress := map[string]int64{}
			deleted := map[string]bool{}
			for _, e := range events {
				switch e.Kind {
				case rsync.EventItem:
					items[e.Name] = e
				case rsync.EventProgress:
					if e.Bytes > e.Size {
						t.Errorf("progress of %s: %d of %d bytes", e.Name, e.Bytes, e.Size)
					}
					progress[e.Name] = e.Bytes
				case rsync.EventDeleted:
					deleted[e.Name] = true
				case rsync.EventError:
					t.Errorf("error event: %s", e.Message)
				}
			}
			big, ok := items["big.bin"]
			if !ok {
				t.Fatalf("no item event for big.bin; got %v", items)
			}
			wantItem := ">f+++++++++"
			if mode == "push" {
				wantItem = "<f+++++++++"
			}
			if big.Item != wantItem || big.Size != 3<<20+12345 {
				t.Errorf("big.bin event = %+v, want item %s", big, wantItem)
			}
			if got := progress["big.bin"]; got != big.Size {
				t.Errorf("last progress of big.bin = %d, want %d", got, big.Size)
			}
			if l := items["dir/link-rel"]; l.Item[1] != 'L' || l.Link != "../a.txt" {
				t.Errorf("symlink event = %+v", l)
			}
			if _, ok := items["dir/"]; !ok {
				t.Error("no item event for dir/")
			}
			if mode != "push" && !deleted["stale"] {
				// A remote receiver reports deletions only when verbose.
				t.Errorf("no deletion event for stale; got %v", deleted)
			}
		})
	}
}
