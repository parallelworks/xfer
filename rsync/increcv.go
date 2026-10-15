// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package rsync

import (
	"errors"
	"io/fs"

	"github.com/parallelworks/xfer/rsync/internal/flist"
	"github.com/parallelworks/xfer/rsync/internal/wire"
)

// Incremental recursion, receiving side. The generator walks the lists in
// the order they arrive. A list's parent directory is handled when the
// list starts, under the list's gap index; directories met inside a list
// are only created. Each list ends with an NDX_DONE to the sender once all
// of its transfers are over, never while the generator is still on it.

// generateInc is the generator with incremental recursion.
func (rv *receiving) generateInc() error {
	s, o := rv.s, rv.o
	lists := s.lists
	w := s.out
	deleting := o.Delete != DeleteNone && rv.deleteMode() == DeleteDuring

	finished := 0 // lists whose NDX_DONE was sent
	// eachDone sends every NDX_DONE as soon as it is written: once the
	// lists are walked, each follows the end of a transfer.
	eachDone := false
	// finish sends NDX_DONE for the completed lists before list cur.
	finish := func(cur int) error {
		for finished < cur {
			l := lists.list(finished)
			if l == nil || l.inProgress > 0 || l.toRedo > 0 {
				return nil
			}
			w.Lock()
			w.W.Ndx(wire.NdxDone)
			if eachDone {
				w.W.Flush()
			}
			w.Unlock()
			if l.parent >= 0 {
				rv.touchUpDir(lists.dir(l.parent))
			}
			finished++
		}
		return nil
	}
	handle := func(ev recvEvent) error {
		switch {
		case ev.ack:
			return errors.New("receiver ended phase 0 early")
		case ev.list == nil:
			return nil
		case ev.done:
			ev.list.inProgress--
			return nil
		}
		// A redo: the transfer is over and must be requested again.
		ev.list.inProgress--
		f, _, err := lists.lookup(ev.redo)
		if err != nil {
			return err
		}
		requested, err := rv.genFile(ev.redo, f, true)
		if requested {
			ev.list.inProgress++
		}
		return err
	}
	// drain handles the events already waiting.
	drain := func() error {
		for {
			select {
			case ev, ok := <-rv.events:
				if !ok {
					return errReceiverStopped
				}
				if err := handle(ev); err != nil {
					return err
				}
			default:
				return nil
			}
		}
	}
	// await blocks for an event or (if lists is set) a new list.
	await := func(forList bool) error {
		var notify chan struct{}
		if forList {
			notify = lists.notify
		}
		// Output waits until we would block, as rsync's does.
		select {
		case ev, ok := <-rv.events:
			if !ok {
				return errReceiverStopped
			}
			return handle(ev)
		case <-notify:
			return nil
		default:
		}
		if err := w.Flush(); err != nil {
			return err
		}
		select {
		case ev, ok := <-rv.events:
			if !ok {
				return errReceiverStopped
			}
			return handle(ev)
		case <-notify:
			return nil
		}
	}

	for n := 0; ; n++ {
		var l *fileList
		for {
			var more bool
			l, more = lists.arrived(n)
			if l != nil || !more {
				break
			}
			if err := finish(n); err != nil {
				return err
			}
			if err := await(true); err != nil {
				return err
			}
		}
		if l == nil {
			break
		}
		if l.parent >= 0 {
			d := lists.dir(l.parent)
			if _, err := rv.genFile(l.gap(), d, false); err != nil {
				return err
			}
			if deleting && d.Flags&flist.FlagContentDir != 0 && !o.DryRun {
				names := make(map[string]bool, len(l.files))
				for _, f := range l.files {
					if f.Flags&flist.FlagCleared == 0 {
						names[f.Name] = true
					}
				}
				rv.deleteInDir(rv.localName(d), d.Name, func(n string) bool { return names[n] })
			}
		}
		for i, f := range l.files {
			if f.Flags&flist.FlagCleared != 0 {
				continue
			}
			if f.IsDir() {
				// Handled when its own list starts ("." already was).
				if f.Name != "." {
					rv.precreateDir(f)
				}
				continue
			}
			requested, err := rv.genFile(l.start+int32(i), f, false)
			if err != nil {
				return err
			}
			if requested {
				l.inProgress++
			}
			if err := drain(); err != nil {
				return err
			}
			if err := finish(n); err != nil {
				return err
			}
		}
		// Each list's requests go out together, as rsync sends them.
		if rv.wrote {
			rv.wrote = false
			if err := w.Flush(); err != nil {
				return err
			}
		}
		if err := finish(n); err != nil {
			return err
		}
	}
	// Every list is walked; finish them as their transfers end. The last
	// list's NDX_DONE also ends phase 0.
	eachDone = true
	for {
		total := lists.count()
		if err := finish(total); err != nil {
			return err
		}
		if finished == total {
			break
		}
		if err := await(false); err != nil {
			return err
		}
	}
	return rv.phases(false)
}

// precreateDir creates a directory met inside its parent's list, before its
// own list is processed, so that the parent's time can be set when the
// parent's list finishes.
func (rv *receiving) precreateDir(f *flist.File) {
	o := rv.o
	if o.DryRun || rv.root == nil || !o.Times || o.OmitDirTimes {
		return
	}
	name := rv.localName(f)
	fi, err := rv.root.Lstat(name)
	if err == nil {
		fi = rv.keepDirlink(name, fi)
	}
	if err == nil && fi.IsDir() {
		return
	}
	if err == nil {
		if err := rv.deleteForReplace(name, fi); err != nil {
			rv.s.sendError("delete of %q failed: %s", name, errnoText(err))
			return
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err := rv.root.Mkdir(name, rv.newPerm(f)|0o700); err != nil {
		return
	}
	rv.s.stats.NumCreated++
	rv.precreated[name] = true
}
