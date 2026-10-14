// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package flist

import (
	"fmt"
	"strings"

	"github.com/parallelworks/xfer/rsync/internal/wire"
)

// Transmitted flag bits. Several bits mean different things in different
// protocol versions or for different file types.
const (
	xmitTopDir           = 1 << 0
	xmitSameMode         = 1 << 1
	xmitExtendedFlags    = 1 << 2 // protocol 28+; same rdev before 28
	xmitSameRdevPre28    = 1 << 2
	xmitSameUID          = 1 << 3
	xmitSameGID          = 1 << 4
	xmitSameName         = 1 << 5
	xmitLongName         = 1 << 6
	xmitSameTime         = 1 << 7
	xmitSameRdevMajor    = 1 << 8 // devices
	xmitNoContentDir     = 1 << 8 // directories, protocol 30+
	xmitHlinked          = 1 << 9
	xmitSameDevPre30     = 1 << 10
	xmitUserNameFollows  = 1 << 10
	xmitRdevMinor8Pre30  = 1 << 11
	xmitGroupNameFollows = 1 << 11
	xmitHlinkFirst       = 1 << 12
	xmitIOErrorEndlist   = 1 << 12
	xmitModNsec          = 1 << 13
)

const maxPath = 1024

// Config is the part of the session state that shapes file lists. Both
// sides must agree on it.
type Config struct {
	Protocol         int
	VarintFlags      bool // CF_VARINT_FLIST_FLAGS
	SafeFlist        bool // protocol 31, or 30 with CF_SAFE_FLIST
	ID0Names         bool // CF_ID0_NAMES
	PreserveUID      bool
	PreserveGID      bool
	PreserveLinks    bool
	PreserveDevices  bool
	PreserveSpecials bool
	HardLinks        bool
	NumericIDs       bool
	Relative         bool
	AlwaysChecksum   bool
	ChecksumLen      int // length of per-file checksums with AlwaysChecksum

	// IncRecurse is set with incremental recursion: the file list arrives
	// as several lists, user and group names are sent inline the first
	// time each id appears, and no id lists follow the lists.
	IncRecurse bool
}

// state is the "same as the previous entry" cache, identical on both
// sides and never reset during a session.
type state struct {
	lastName  string
	mode      uint32
	mtime     int64
	uid, gid  uint32
	rdevMajor uint32
	rdevRaw   uint32 // protocol < 28
	hlDev     int64  // protocol < 30
}

// Encoder writes file-list entries.
type Encoder struct {
	w   *wire.Writer
	cfg Config
	st  state

	sent                  int32 // entries sent in the current list
	listStart             int32 // global index of the current list's first entry
	hlinks                map[[2]int64]int32
	users                 map[uint32]string
	groups                map[uint32]string
	userOrder, groupOrder []uint32

	// LookupUser and LookupGroup resolve ids to names for the id lists.
	// They may be nil, in which case no names are sent.
	LookupUser, LookupGroup func(id uint32) string
}

// NewEncoder returns an Encoder writing to w.
func NewEncoder(w *wire.Writer, cfg Config) *Encoder {
	return &Encoder{w: w, cfg: cfg, hlinks: map[[2]int64]int32{}, users: map[uint32]string{}, groups: map[uint32]string{}}
}

// StartList begins a new file list whose first entry has global index
// start. Hard-link references are numbered by these global indexes.
func (e *Encoder) StartList(start int32) {
	e.listStart, e.sent = start, 0
}

// inlineName returns the name to send inline for an id seen for the first
// time with incremental recursion, or "" when none is sent.
func (e *Encoder) inlineName(id uint32, m map[uint32]string, lookup func(uint32) string) string {
	if e.cfg.NumericIDs {
		return ""
	}
	if _, ok := m[id]; ok {
		return ""
	}
	name := ""
	if lookup != nil {
		name = lookup(id)
	}
	if len(name) > 255 {
		name = name[:255]
	}
	m[id] = name
	return name
}

func (e *Encoder) noteID(id uint32, m map[uint32]string, order *[]uint32, lookup func(uint32) string) {
	if id == 0 || e.cfg.NumericIDs {
		return
	}
	if _, ok := m[id]; ok {
		return
	}
	name := ""
	if lookup != nil {
		name = lookup(id)
	}
	m[id] = name
	*order = append(*order, id)
}

// Encode writes one entry. For hard-link tracking the sender sets f.Dev and
// f.Ino (with f.Flags&FlagHlinked) before calling Encode.
func (e *Encoder) Encode(f *File) error {
	cfg, st, w := &e.cfg, &e.st, e.w
	proto := cfg.Protocol
	var xf uint32

	if f.IsDir() {
		if proto >= 30 {
			switch {
			case f.Flags&FlagContentDir != 0:
				if f.Flags&FlagTopDir != 0 {
					xf = xmitTopDir
				}
			case f.Flags&FlagImpliedDir != 0:
				xf = xmitTopDir | xmitNoContentDir
			default:
				xf = xmitNoContentDir
			}
		} else if f.Flags&FlagTopDir != 0 {
			xf = xmitTopDir
		}
	}

	if f.Mode == st.mode {
		xf |= xmitSameMode
	} else {
		st.mode = f.Mode
	}

	sendRdev := false
	switch {
	case cfg.PreserveDevices && f.IsDevice():
		sendRdev = true
		if proto < 28 {
			raw := f.RdevMajor<<8 | f.RdevMinor
			if raw == st.rdevRaw {
				xf |= xmitSameRdevPre28
			} else {
				st.rdevRaw = raw
			}
		} else {
			if f.RdevMajor == st.rdevMajor {
				xf |= xmitSameRdevMajor
			} else {
				st.rdevMajor = f.RdevMajor
			}
			if proto < 30 && f.RdevMinor <= 0xff {
				xf |= xmitRdevMinor8Pre30
			}
		}
	case cfg.PreserveSpecials && f.IsSpecial() && proto < 31:
		sendRdev = true
		if proto < 28 {
			xf |= xmitSameRdevPre28
		} else {
			xf |= xmitSameRdevMajor
			if proto < 30 {
				xf |= xmitRdevMinor8Pre30
			}
		}
	case proto < 28:
		st.rdevRaw = 0
	}

	var userName, groupName string
	if !cfg.PreserveUID || (f.UID == st.uid && st.lastName != "") {
		xf |= xmitSameUID
	} else {
		st.uid = f.UID
		if cfg.IncRecurse {
			if userName = e.inlineName(f.UID, e.users, e.LookupUser); userName != "" {
				xf |= xmitUserNameFollows
			}
		} else {
			e.noteID(f.UID, e.users, &e.userOrder, e.LookupUser)
		}
	}
	if !cfg.PreserveGID || (f.GID == st.gid && st.lastName != "") {
		xf |= xmitSameGID
	} else {
		st.gid = f.GID
		if cfg.IncRecurse {
			if groupName = e.inlineName(f.GID, e.groups, e.LookupGroup); groupName != "" {
				xf |= xmitGroupNameFollows
			}
		} else {
			e.noteID(f.GID, e.groups, &e.groupOrder, e.LookupGroup)
		}
	}

	if f.ModTime == st.mtime {
		xf |= xmitSameTime
	} else {
		st.mtime = f.ModTime
	}
	if proto >= 31 && f.ModNsec > 0 && f.ModNsec <= 999_999_999 {
		xf |= xmitModNsec
	}

	// Hard links.
	abbrev, follower := false, false
	sendDevIno := false
	if cfg.HardLinks && f.Flags&FlagHlinked != 0 && !f.IsDir() {
		if proto >= 30 {
			xf |= xmitHlinked
			key := [2]int64{f.Dev, f.Ino}
			if leader, ok := e.hlinks[key]; ok {
				f.HLinkIndex = leader
				// A leader in an earlier list is referenced, but the
				// attributes are sent in full.
				abbrev = leader >= e.listStart
				follower = true
			} else {
				gnum := e.listStart + e.sent
				e.hlinks[key] = gnum
				f.HLinkIndex = gnum
				xf |= xmitHlinkFirst
			}
		} else if proto >= 28 {
			xf |= xmitHlinked
			sendDevIno = true
			if f.Dev == st.hlDev {
				xf |= xmitSameDevPre30
			} else {
				st.hlDev = f.Dev
			}
		} else if f.IsRegular() {
			sendDevIno = true
		}
	}

	name := f.Name
	l1 := 0
	for l1 < len(name) && l1 < len(st.lastName) && l1 < 255 && name[l1] == st.lastName[l1] {
		l1++
	}
	l2 := len(name) - l1
	if l1 > 0 {
		xf |= xmitSameName
	}
	if l2 > 255 {
		xf |= xmitLongName
	}

	// The flag word. A zero word would read as the end of the list.
	switch {
	case cfg.VarintFlags:
		if xf == 0 {
			xf = xmitExtendedFlags
		}
		w.Varint(int32(xf))
	case proto >= 28:
		if xf == 0 && !f.IsDir() {
			xf |= xmitTopDir
		}
		if xf&0xff00 != 0 || xf == 0 {
			xf |= xmitExtendedFlags
			w.Int16(uint16(xf))
		} else {
			w.Byte(byte(xf))
		}
	default:
		if xf&0xff == 0 {
			if f.IsDir() {
				xf |= xmitLongName
			} else {
				xf |= xmitTopDir
			}
		}
		w.Byte(byte(xf))
	}

	if xf&xmitSameName != 0 {
		w.Byte(byte(l1))
	}
	if xf&xmitLongName != 0 {
		e.varint30(int32(l2))
	} else {
		w.Byte(byte(l2))
	}
	w.Write([]byte(name[l1:]))
	st.lastName = name
	e.sent++

	if follower {
		w.Varint(f.HLinkIndex)
	}
	if abbrev {
		return w.Err()
	}

	if proto >= 30 {
		w.Varlong(f.Size, 3)
	} else {
		w.Longint(f.Size)
	}
	if xf&xmitSameTime == 0 {
		if proto >= 30 {
			w.Varlong(f.ModTime, 4)
		} else {
			w.Int32(int32(f.ModTime))
		}
	}
	if xf&xmitModNsec != 0 {
		w.Varint(f.ModNsec)
	}
	if xf&xmitSameMode == 0 {
		w.Int32(int32(f.Mode))
	}
	if cfg.PreserveUID && xf&xmitSameUID == 0 {
		e.varint30(int32(f.UID))
		if xf&xmitUserNameFollows != 0 {
			w.Byte(byte(len(userName)))
			w.Write([]byte(userName))
		}
	}
	if cfg.PreserveGID && xf&xmitSameGID == 0 {
		e.varint30(int32(f.GID))
		if xf&xmitGroupNameFollows != 0 {
			w.Byte(byte(len(groupName)))
			w.Write([]byte(groupName))
		}
	}
	if sendRdev {
		switch {
		case proto < 28:
			if xf&xmitSameRdevPre28 == 0 {
				w.Int32(int32(st.rdevRaw))
			}
		case f.IsSpecial():
			if proto < 30 {
				w.Byte(0)
			} else {
				w.Varint(0)
			}
		default:
			if xf&xmitSameRdevMajor == 0 {
				e.varint30(int32(f.RdevMajor))
			}
			switch {
			case proto >= 30:
				w.Varint(int32(f.RdevMinor))
			case xf&xmitRdevMinor8Pre30 != 0:
				w.Byte(byte(f.RdevMinor))
			default:
				w.Int32(int32(f.RdevMinor))
			}
		}
	}
	if cfg.PreserveLinks && f.IsLink() {
		e.varint30(int32(len(f.Link)))
		w.Write([]byte(f.Link))
	}
	if sendDevIno {
		if xf&xmitSameDevPre30 == 0 || proto < 28 {
			w.Longint(f.Dev + 1)
		}
		w.Longint(f.Ino)
	}
	if cfg.AlwaysChecksum && (f.IsRegular() || proto < 28) {
		sum := f.Checksum
		if len(sum) != cfg.ChecksumLen || !f.IsRegular() {
			sum = make([]byte, cfg.ChecksumLen)
			if f.IsRegular() {
				copy(sum, f.Checksum)
			}
		}
		w.Write(sum)
	}
	return w.Err()
}

func (e *Encoder) varint30(v int32) {
	if e.cfg.Protocol < 30 {
		e.w.Int32(v)
	} else {
		e.w.Varint(v)
	}
}

// End writes the end-of-list marker and, unless inc-recurse is in use, the
// trailing id lists and (before protocol 30) the I/O error code. It
// reports whether ioError still needs to be sent as an MSG_IO_ERROR
// message (protocol 30 without safe file lists).
func (e *Encoder) End(ioError int32) (sendIOErrorMsg bool) {
	cfg, w := &e.cfg, e.w
	switch {
	case cfg.VarintFlags:
		w.Varint(0)
		w.Varint(ioError)
	case ioError != 0 && cfg.SafeFlist:
		w.Int16(xmitExtendedFlags | xmitIOErrorEndlist)
		w.Varint(ioError)
	default:
		w.Byte(0)
	}
	if !cfg.NumericIDs && !cfg.IncRecurse {
		if cfg.PreserveUID {
			e.writeIDList(e.userOrder, e.users, e.LookupUser)
		}
		if cfg.PreserveGID {
			e.writeIDList(e.groupOrder, e.groups, e.LookupGroup)
		}
	}
	if cfg.Protocol < 30 {
		w.Int32(ioError)
		return false
	}
	return ioError != 0 && !cfg.SafeFlist && !cfg.VarintFlags
}

func (e *Encoder) writeIDList(order []uint32, names map[uint32]string, lookup func(uint32) string) {
	for _, id := range order {
		name := names[id]
		if name == "" {
			continue
		}
		if len(name) > 255 {
			name = name[:255]
		}
		e.varint30(int32(id))
		e.w.Byte(byte(len(name)))
		e.w.Write([]byte(name))
	}
	e.varint30(0)
	if e.cfg.ID0Names {
		name := ""
		if lookup != nil {
			name = lookup(0)
		}
		e.w.Byte(byte(len(name)))
		e.w.Write([]byte(name))
	}
}

// Decoder reads file-list entries.
type Decoder struct {
	r   *wire.Reader
	cfg Config
	st  state

	listStart int32           // global index of the current list's first entry
	count     int32           // entries decoded in the current list
	leaders   map[int32]*File // hard-link leaders by global index

	// Users and Groups map ids to names: from the id lists after the file
	// list, or inline with incremental recursion.
	Users, Groups map[uint32]string
}

// NewDecoder returns a Decoder reading from r.
func NewDecoder(r *wire.Reader, cfg Config) *Decoder {
	return &Decoder{r: r, cfg: cfg, leaders: map[int32]*File{}, Users: map[uint32]string{}, Groups: map[uint32]string{}}
}

// StartList begins a new file list whose first entry has global index
// start.
func (d *Decoder) StartList(start int32) {
	d.listStart, d.count = start, 0
}

// Leader returns the hard-link leader with global index gnum.
func (d *Decoder) Leader(gnum int32) *File { return d.leaders[gnum] }

func (d *Decoder) varint30() int32 {
	if d.cfg.Protocol < 30 {
		return d.r.Int32()
	}
	return d.r.Varint()
}

// Decode reads the next entry. At the end of the list it returns nil and
// the I/O error code carried by the end marker, if any.
func (d *Decoder) Decode() (f *File, ioError int32, err error) {
	cfg, st, r := &d.cfg, &d.st, d.r
	proto := cfg.Protocol

	var xf uint32
	switch {
	case cfg.VarintFlags:
		xf = uint32(r.Varint())
		if xf == 0 {
			ioError = r.Varint()
			return nil, ioError, r.Err()
		}
	default:
		xf = uint32(r.Byte())
		if xf == 0 {
			return nil, 0, r.Err()
		}
		if proto >= 28 && xf&xmitExtendedFlags != 0 {
			xf |= uint32(r.Byte()) << 8
			if xf == xmitExtendedFlags|xmitIOErrorEndlist {
				if !cfg.SafeFlist {
					return nil, 0, fmt.Errorf("%w: unexpected I/O error marker in file list", wire.ErrProtocol)
				}
				return nil, r.Varint(), r.Err()
			}
		}
	}

	l1 := 0
	if xf&xmitSameName != 0 {
		l1 = int(r.Byte())
	}
	var l2 int
	if xf&xmitLongName != 0 {
		l2 = int(d.varint30())
	} else {
		l2 = int(r.Byte())
	}
	if err := r.Err(); err != nil {
		return nil, 0, err
	}
	if l1 > len(st.lastName) || l2 < 0 || l1+l2 >= maxPath {
		return nil, 0, fmt.Errorf("%w: overflow in file-list name (l1=%d l2=%d)", wire.ErrProtocol, l1, l2)
	}
	nb := make([]byte, l1+l2)
	copy(nb, st.lastName[:l1])
	r.ReadFull(nb[l1:])
	name := string(nb)
	st.lastName = name
	if err := r.Err(); err != nil {
		return nil, 0, err
	}
	if err := checkName(name, cfg.Relative); err != nil {
		return nil, 0, err
	}

	f = &File{Name: name, HLinkIndex: -1}
	idx := d.listStart + d.count
	d.count++

	if proto >= 30 && xf&xmitHlinked != 0 {
		f.Flags |= FlagHlinked
		if xf&xmitHlinkFirst != 0 {
			f.Flags |= FlagHlinkFirst
			f.HLinkIndex = idx
			d.leaders[idx] = f
		} else {
			gnum := r.Varint()
			if err := r.Err(); err != nil {
				return nil, 0, err
			}
			l := d.leaders[gnum]
			if gnum < 0 || gnum >= idx || l == nil {
				return nil, 0, fmt.Errorf("%w: hard-link reference out of range: %d", wire.ErrProtocol, gnum)
			}
			f.HLinkIndex = gnum
			if gnum >= d.listStart {
				// Same list: the attributes are the leader's.
				f.Size, f.ModTime, f.ModNsec, f.Mode = l.Size, l.ModTime, l.ModNsec, l.Mode
				f.UID, f.GID, f.RdevMajor, f.RdevMinor = l.UID, l.GID, l.RdevMajor, l.RdevMinor
				f.Link, f.Checksum = l.Link, l.Checksum
				st.mtime, st.mode, st.uid, st.gid, st.rdevMajor = l.ModTime, l.Mode, l.UID, l.GID, l.RdevMajor
				d.dirFlags(f, xf)
				return f, 0, nil
			}
		}
	}

	if proto >= 30 {
		f.Size = r.Varlong(3)
	} else {
		f.Size = r.Longint()
	}
	if f.Size < 0 {
		r.SetErr(fmt.Errorf("%w: negative file size", wire.ErrProtocol))
	}
	if xf&xmitSameTime == 0 {
		if proto >= 30 {
			st.mtime = r.Varlong(4)
		} else {
			st.mtime = int64(uint32(r.Int32()))
		}
	}
	f.ModTime = st.mtime
	if xf&xmitModNsec != 0 && proto >= 31 {
		f.ModNsec = r.Varint()
		if f.ModNsec < 0 || f.ModNsec > 999_999_999 {
			r.SetErr(fmt.Errorf("%w: invalid nanoseconds", wire.ErrProtocol))
		}
	}
	if xf&xmitSameMode == 0 {
		st.mode = uint32(r.Int32())
	}
	f.Mode = st.mode
	switch f.Mode & TypeMask {
	case TypeSocket, TypeLink, TypeReg, TypeBlock, TypeDir, TypeChar, TypeFIFO:
	default:
		if r.Err() == nil {
			return nil, 0, fmt.Errorf("%w: invalid file mode %#o for %q", wire.ErrProtocol, f.Mode, name)
		}
	}
	if cfg.PreserveUID && xf&xmitSameUID == 0 {
		st.uid = uint32(d.varint30())
		if proto >= 30 && xf&xmitUserNameFollows != 0 {
			d.Users[st.uid] = d.readName() // inline with incremental recursion
		}
	}
	f.UID = st.uid
	if cfg.PreserveGID && xf&xmitSameGID == 0 {
		st.gid = uint32(d.varint30())
		if proto >= 30 && xf&xmitGroupNameFollows != 0 {
			d.Groups[st.gid] = d.readName()
		}
	}
	f.GID = st.gid

	switch {
	case cfg.PreserveDevices && f.IsDevice(), cfg.PreserveSpecials && f.IsSpecial() && proto < 31:
		switch {
		case proto < 28:
			if xf&xmitSameRdevPre28 == 0 {
				st.rdevRaw = uint32(r.Int32())
			}
			f.RdevMajor, f.RdevMinor = st.rdevRaw>>8, st.rdevRaw&0xff
		default:
			if xf&xmitSameRdevMajor == 0 {
				st.rdevMajor = uint32(d.varint30())
			}
			f.RdevMajor = st.rdevMajor
			switch {
			case proto >= 30:
				f.RdevMinor = uint32(r.Varint())
			case xf&xmitRdevMinor8Pre30 != 0:
				f.RdevMinor = uint32(r.Byte())
			default:
				f.RdevMinor = uint32(r.Int32())
			}
		}
		f.Size = 0
		if f.IsSpecial() {
			f.RdevMajor, f.RdevMinor = 0, 0
		}
	case proto < 28:
		st.rdevRaw = 0
	}

	if cfg.PreserveLinks && f.IsLink() {
		n := int(d.varint30())
		if n < 0 || n+1 > maxPath {
			return nil, 0, fmt.Errorf("%w: overflow in symlink target", wire.ErrProtocol)
		}
		b := make([]byte, n)
		r.ReadFull(b)
		f.Link = string(b)
	}

	if cfg.HardLinks && proto < 30 && (xf&xmitHlinked != 0 || (proto < 28 && f.IsRegular())) {
		f.Flags |= FlagHlinked
		if proto < 28 || xf&xmitSameDevPre30 == 0 {
			st.hlDev = r.Longint() - 1
		}
		f.Dev = st.hlDev
		f.Ino = r.Longint()
	}

	if cfg.AlwaysChecksum && (f.IsRegular() || proto < 28) {
		f.Checksum = make([]byte, cfg.ChecksumLen)
		r.ReadFull(f.Checksum)
	}

	d.dirFlags(f, xf)
	return f, 0, r.Err()
}

func (d *Decoder) dirFlags(f *File, xf uint32) {
	if !f.IsDir() {
		return
	}
	if d.cfg.Protocol >= 30 {
		switch {
		case xf&xmitNoContentDir == 0:
			f.Flags |= FlagContentDir
			if xf&xmitTopDir != 0 {
				f.Flags |= FlagTopDir
			}
		case xf&xmitTopDir != 0:
			f.Flags |= FlagImpliedDir
		}
		return
	}
	// Older protocols mark only transfer roots; everything below a root
	// is content. That is all the receiver uses the flags for.
	f.Flags |= FlagContentDir
	if xf&xmitTopDir != 0 {
		f.Flags |= FlagTopDir
	}
}

// ReadTrailer reads what follows the end marker without inc-recurse: the
// id lists and, before protocol 30, the I/O error code.
func (d *Decoder) ReadTrailer() (ioError int32, err error) {
	if !d.cfg.NumericIDs && !d.cfg.IncRecurse {
		if d.cfg.PreserveUID {
			d.Users = d.readIDList()
		}
		if d.cfg.PreserveGID {
			d.Groups = d.readIDList()
		}
	}
	if d.cfg.Protocol < 30 {
		ioError = d.r.Int32()
	}
	return ioError, d.r.Err()
}

func (d *Decoder) readIDList() map[uint32]string {
	m := map[uint32]string{}
	for d.r.Err() == nil {
		id := uint32(d.varint30())
		if id == 0 {
			if d.cfg.ID0Names {
				m[0] = d.readName()
			}
			break
		}
		m[id] = d.readName()
	}
	return m
}

func (d *Decoder) readName() string {
	n := int(d.r.Byte())
	b := make([]byte, n)
	d.r.ReadFull(b)
	return string(b)
}

func checkName(name string, relative bool) error {
	if name == "" {
		return fmt.Errorf("%w: empty file name", wire.ErrProtocol)
	}
	if strings.HasPrefix(name, "/") && !relative {
		return fmt.Errorf("%w: absolute file name %q", wire.ErrProtocol, name)
	}
	for c := range strings.SplitSeq(name, "/") {
		if c == ".." {
			return fmt.Errorf("%w: file name contains \"..\": %q", wire.ErrProtocol, name)
		}
	}
	return nil
}
