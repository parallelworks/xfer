// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package rsync

import (
	"strings"
	"sync"
)

// EventKind says what an Event reports.
type EventKind int

const (
	// EventItem reports a file, directory or other entry that was
	// transferred, created or changed, as -i would show it. With
	// ItemizeChanges of 2 or more, unchanged entries are reported too.
	EventItem EventKind = iota + 1
	// EventProgress reports how much of a file's data has been
	// transferred. It comes when a file starts, at most four times a
	// second while it transfers, and when it ends.
	EventProgress
	// EventDeleted reports an entry deleted from the destination. A
	// remote destination's rsync reports its deletions only when Verbose
	// or ItemizeChanges is set.
	EventDeleted
	// EventMessage reports informational text, local or from the remote
	// rsync, such as "created directory dst".
	EventMessage
	// EventError reports a warning or an error, local or from the remote
	// rsync.
	EventError
)

func (k EventKind) String() string {
	switch k {
	case EventItem:
		return "item"
	case EventProgress:
		return "progress"
	case EventDeleted:
		return "deleted"
	case EventMessage:
		return "message"
	case EventError:
		return "error"
	}
	return "unknown"
}

// Event is one step of a transfer, reported to Client.Events so that an
// embedding program can show the transfer its own way.
type Event struct {
	Kind EventKind

	// Name is the entry's path in the transfer (relative to the source
	// or destination directory, as rsync's output names it), with a
	// trailing slash for a directory. It is set for EventItem,
	// EventProgress and EventDeleted.
	Name string

	// Item is the 11-character change summary that -i prints, such as
	// ">f+++++++++" for a new file sent to the receiver ("<" when this
	// side sends it to a remote receiver). It is set for EventItem.
	Item string

	// Link is a symlink's target, or for a hard link the name it links
	// to. It is set for EventItem when it applies.
	Link string

	// Size is the entry's size in bytes. It is set for EventItem and
	// EventProgress.
	Size int64

	// Bytes is how much of the file's data has been transferred so far.
	// It is set for EventProgress.
	Bytes int64

	// Message is the text of an EventMessage or EventError, without a
	// final newline.
	Message string
}

// eventSink delivers events one at a time.
type eventSink struct {
	mu sync.Mutex
	fn func(Event)
}

func (e *eventSink) emit(ev Event) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.fn(ev)
}

// message reports text as an EventMessage or EventError, a line at a time.
func (e *eventSink) message(kind EventKind, text string) {
	if e == nil {
		return
	}
	for line := range strings.SplitSeq(strings.TrimRight(text, "\n"), "\n") {
		if line != "" {
			e.emit(Event{Kind: kind, Message: line})
		}
	}
}
