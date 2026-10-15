// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package rsync

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/parallelworks/xfer/rsync/internal/flist"
)

// Item flags describe what happened to a file. They travel on the wire
// as 16 bits; the higher bits are local.
const (
	itemReportAtime      = 1 << 0
	itemReportChange     = 1 << 1
	itemReportSize       = 1 << 2
	itemReportTimefail   = 1 << 2 // symlinks only
	itemReportTime       = 1 << 3
	itemReportPerms      = 1 << 4
	itemReportOwner      = 1 << 5
	itemReportGroup      = 1 << 6
	itemReportACL        = 1 << 7
	itemReportXattr      = 1 << 8
	itemReportCrtime     = 1 << 10
	itemBasisTypeFollows = 1 << 11
	itemXnameFollows     = 1 << 12
	itemIsNew            = 1 << 13
	itemLocalChange      = 1 << 14
	itemTransfer         = 1 << 15
	itemMissingData      = 1 << 16
	itemDeleted          = 1 << 17
	itemMatched          = 1 << 18

	significantItemFlags = ^uint32(itemBasisTypeFollows | itemXnameFollows | itemLocalChange)
)

// logger writes the client's user-facing output.
type logger struct {
	mu     sync.Mutex
	stdout io.Writer
	stderr io.Writer
	opts   *Options
	lv     levels // --info and --debug
	format string // per-file output format; "" prints nothing
	hasI   int    // the format contains %i (2 with -ii: show unchanged too)

	// Set when we are the sending client so '<' marks outgoing files.
	sending bool
	// localSender is set for the sending client of a local transfer, which
	// itemizes as the receiver but names its operation "send".
	localSender bool
	// symlinkTimes reports whether the receiver can set symlink times.
	symlinkTimes bool

	progress *progress
}

func newLogger(o *Options, stdout, stderr io.Writer) *logger {
	l := &logger{stdout: stdout, stderr: stderr, opts: o, lv: outputLevels(o)}
	switch {
	case o.OutFormat != "":
		l.format = o.OutFormat
	case o.ItemizeChanges > 0:
		l.format = "%i %n%L"
	case l.lv.info[infoName] > 0:
		l.format = "%n%L"
	}
	if strings.Contains(l.format, "%i") {
		l.hasI = 1
		if o.ItemizeChanges > 1 || strings.Contains(l.format, "%I") {
			l.hasI = 2
		}
	}
	if o.Quiet {
		l.format = ""
	}
	if p := l.lv.info[infoProgress]; p > 0 {
		l.progress = &progress{l: l, total: p > 1}
	}
	return l
}

func (l *logger) print(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.progress.clearLine()
	io.WriteString(l.stdout, s)
}

func (l *logger) errorf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if !strings.HasPrefix(msg, "rsync") {
		msg = "rsync: " + msg
	}
	if !strings.HasSuffix(msg, "\n") {
		msg += "\n"
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.progress.clearLine()
	io.WriteString(l.stderr, msg)
}

// remote prints text received from the peer.
func (l *logger) remote(isErr bool, text []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.progress.clearLine()
	if isErr {
		l.stderr.Write(text)
	} else if !l.opts.Quiet {
		l.stdout.Write(text)
	}
}

func (l *logger) deleted(name string, isDir bool) {
	if l.format == "" && l.lv.info[infoDel] == 0 {
		return
	}
	if isDir && !strings.HasSuffix(name, "/") {
		name += "/"
	}
	if l.hasI > 0 {
		l.print(l.expand(&flist.File{Name: name}, itemDeleted, "del.", "") + "\n")
	} else {
		l.print("deleting " + l.safe(name) + "\n")
	}
}

// shouldLog decides whether a record that transferred nothing is shown
// (rsync's maybe_log_item).
func (l *logger) shouldLog(f *flist.File, iflags uint32, xname string) bool {
	if l.format == "" {
		return false
	}
	sig := iflags&significantItemFlags != 0
	switch {
	case l.hasI > 0 && (sig || xname != "" || l.hasI > 1 || l.lv.info[infoName] > 1):
		return true
	case iflags&itemLocalChange != 0 && sig:
		return true
	case xname != "":
		return true
	case f.IsDir() && sig:
		return true
	}
	return false
}

// item logs a file-list entry with its item flags.
func (l *logger) item(f *flist.File, iflags uint32, xname string) {
	if l.format == "" {
		return
	}
	op := "recv"
	if l.sending || l.localSender {
		op = "send"
	}
	l.print(l.expand(f, iflags, op, xname) + "\n")
}

// itemString renders the 11-character %i field.
func (l *logger) itemString(f *flist.File, iflags uint32) string {
	if iflags&itemDeleted != 0 {
		return "*deleting  "
	}
	var c [11]byte
	switch {
	case iflags&itemLocalChange != 0:
		if iflags&itemXnameFollows != 0 {
			c[0] = 'h'
		} else {
			c[0] = 'c'
		}
	case iflags&itemTransfer == 0:
		c[0] = '.'
	case l.sending:
		c[0] = '<'
	default:
		c[0] = '>'
	}
	switch {
	case f.IsLink():
		c[1] = 'L'
	case f.IsDir():
		c[1] = 'd'
	case f.IsSpecial():
		c[1] = 'S'
	case f.IsDevice():
		c[1] = 'D'
	default:
		c[1] = 'f'
	}
	flag := func(bit uint32, ch byte) byte {
		if iflags&bit != 0 {
			return ch
		}
		return '.'
	}
	c[2] = flag(itemReportChange, 'c')
	c[3] = flag(itemReportSize, 's')
	if f.IsLink() {
		c[3] = '.'
	}
	c[4] = '.'
	if iflags&itemReportTime != 0 {
		c[4] = 't'
		if !l.opts.Times || (f.IsLink() && (!l.symlinkTimes || iflags&itemReportTimefail != 0)) {
			c[4] = 'T'
		}
	}
	c[5] = flag(itemReportPerms, 'p')
	c[6] = flag(itemReportOwner, 'o')
	c[7] = flag(itemReportGroup, 'g')
	c[8] = '.'
	switch {
	case iflags&itemReportAtime != 0 && iflags&itemReportCrtime != 0:
		c[8] = 'b'
	case iflags&itemReportAtime != 0:
		c[8] = 'u'
	case iflags&itemReportCrtime != 0:
		c[8] = 'n'
	}
	c[9] = flag(itemReportACL, 'a')
	c[10] = flag(itemReportXattr, 'x')
	fill := func(ch byte) {
		for i := 2; i < 11; i++ {
			c[i] = ch
		}
	}
	switch {
	case iflags&itemIsNew != 0:
		fill('+')
	case iflags&itemMissingData != 0:
		fill('?')
	case c[0] == '.' || c[0] == 'h' || c[0] == 'c':
		if string(c[2:]) == "........." {
			fill(' ')
		}
	}
	return string(c[:])
}

// expand renders the output format for one entry.
func (l *logger) expand(f *flist.File, iflags uint32, op, xname string) string {
	var b strings.Builder
	format := l.format
	if format == "" {
		format = "%n%L"
	}
	for i := 0; i < len(format); i++ {
		ch := format[i]
		if ch != '%' || i+1 >= len(format) {
			b.WriteByte(ch)
			continue
		}
		// Optional width, as in "%-10n" or "%'l".
		j := i + 1
		for j < len(format) && strings.IndexByte("-'0123456789", format[j]) >= 0 {
			j++
		}
		if j >= len(format) {
			b.WriteString(format[i:])
			break
		}
		spec := format[i+1 : j]
		verb := format[j]
		i = j
		var v string
		switch verb {
		case '%':
			v = "%"
		case 'n':
			v = l.safe(f.Name)
			if f.IsDir() && !strings.HasSuffix(v, "/") {
				v += "/"
			}
		case 'f':
			v = l.safe(f.Name)
		case 'L':
			switch {
			case xname != "" && iflags&itemXnameFollows != 0:
				v = " => " + l.safe(xname)
			case f.IsLink() && f.Link != "":
				v = " -> " + l.safe(f.Link)
			}
		case 'i':
			v = l.itemString(f, iflags)
		case 'I':
		case 'o':
			v = op
		case 'l':
			v = strconv.FormatInt(f.Size, 10)
		case 'b', 'c':
			v = "0"
		case 'M':
			v = time.Unix(f.ModTime, 0).Format("2006/01/02-15:04:05")
		case 't':
			v = time.Now().Format("2006/01/02 15:04:05")
		case 'B':
			v = permString(f.Mode)
		case 'U':
			v = strconv.FormatUint(uint64(f.UID), 10)
		case 'G':
			v = strconv.FormatUint(uint64(f.GID), 10)
		case 'p':
			v = strconv.Itoa(pid())
		default:
			v = "%" + spec + string(verb)
			spec = ""
		}
		b.WriteString(pad(v, strings.Trim(spec, "'")))
	}
	return b.String()
}

// safe escapes a name for output the way rsync does: control characters
// other than tab, and bytes that are not valid UTF-8 unless -8 is given,
// become \#ooo (octal).
func (l *logger) safe(name string) string {
	clean := true
	for i := 0; i < len(name); i++ {
		if c := name[i]; (c < 0x20 && c != '\t') || c >= 0x80 {
			clean = false
			break
		}
	}
	if clean {
		return name
	}
	var b strings.Builder
	for i := 0; i < len(name); {
		c := name[i]
		switch {
		case c < 0x20 && c != '\t':
			fmt.Fprintf(&b, "\\#%03o", c)
			i++
		case c >= 0x80 && !l.opts.EightBitOutput:
			r, n := utf8.DecodeRuneInString(name[i:])
			if r == utf8.RuneError && n <= 1 {
				fmt.Fprintf(&b, "\\#%03o", c)
				i++
			} else {
				b.WriteString(name[i : i+n])
				i += n
			}
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

func pad(s, spec string) string {
	if spec == "" {
		return s
	}
	left := strings.HasPrefix(spec, "-")
	n, err := strconv.Atoi(strings.TrimPrefix(spec, "-"))
	if err != nil || len(s) >= n {
		return s
	}
	if left {
		return s + strings.Repeat(" ", n-len(s))
	}
	return strings.Repeat(" ", n-len(s)) + s
}

func permString(mode uint32) string {
	const rwx = "rwxrwxrwx"
	b := []byte("---------")
	for i := range 9 {
		if mode&(1<<(8-i)) != 0 {
			b[i] = rwx[i]
		}
	}
	if mode&0o4000 != 0 {
		b[2] = map[bool]byte{true: 's', false: 'S'}[b[2] == 'x']
	}
	if mode&0o2000 != 0 {
		b[5] = map[bool]byte{true: 's', false: 'S'}[b[5] == 'x']
	}
	if mode&0o1000 != 0 {
		b[8] = map[bool]byte{true: 't', false: 'T'}[b[8] == 'x']
	}
	return string(b)
}

// listLine renders a --list-only line.
func listLine(f *flist.File, human int) string {
	var t byte = '-'
	switch {
	case f.IsDir():
		t = 'd'
	case f.IsLink():
		t = 'l'
	case f.IsDevice():
		t = map[bool]byte{true: 'c', false: 'b'}[f.Mode&flist.TypeMask == flist.TypeChar]
	case f.IsSpecial():
		t = map[bool]byte{true: 's', false: 'p'}[f.Mode&flist.TypeMask == flist.TypeSocket]
	}
	name := f.Name
	if f.IsLink() {
		name += " -> " + f.Link
	}
	size := formatNum(f.Size, human)
	return fmt.Sprintf("%c%s %14s %s %s\n", t, permString(f.Mode), size,
		time.Unix(f.ModTime, 0).Format("2006/01/02 15:04:05"), name)
}

// progress renders --progress and --info=progress2 output.
type progress struct {
	l     *logger
	total bool

	// whole-transfer state for progress2
	xferred   int64
	totalSize int64
	start     time.Time

	// current file
	name      string
	size      int64
	done      int64
	fileStart time.Time
	lastPrint time.Time
	xfrNum    int
	toCheck   int
	numFiles  int
	more      bool // more file lists are coming (incremental recursion)
	updated   bool // the current file's progress was shown
	onLine    bool
}

func (p *progress) clearLine() {
	if p != nil && p.onLine {
		io.WriteString(p.l.stdout, "\n")
		p.onLine = false
	}
}

// begin starts tracking a file transfer. remaining counts the entries
// after this one in the file lists so far, numFiles all of them, and more
// is set while further lists may come.
func (p *progress) begin(name string, size int64, remaining, numFiles int, more bool) {
	if p == nil {
		return
	}
	now := time.Now()
	if p.start.IsZero() {
		p.start = now
	}
	p.name, p.size, p.done, p.fileStart = name, size, 0, now
	p.toCheck, p.numFiles, p.more = remaining, numFiles, more
	p.updated = false
	p.xfrNum++
}

// shown shows the current file at 0%, as a receiver does before its data
// arrives.
func (p *progress) shown() {
	if p == nil {
		return
	}
	p.updated = true
	p.render(false)
}

// update records n more bytes of the current file.
func (p *progress) update(n int64) {
	if p == nil {
		return
	}
	p.done += n
	p.xferred += n
	if !p.updated || time.Since(p.lastPrint) >= 250*time.Millisecond {
		p.updated = true
		p.render(false)
	}
}

// end finishes the current file.
func (p *progress) end() {
	if p == nil {
		return
	}
	p.render(true)
}

func (p *progress) render(final bool) {
	p.l.mu.Lock()
	defer p.l.mu.Unlock()
	now := time.Now()
	p.lastPrint = now
	done, size, since := p.done, p.size, p.fileStart
	if p.total {
		done, size, since = p.xferred, p.totalSize, p.start
	}
	pct := 100
	if size > 0 {
		pct = int(done * 100 / size)
	}
	elapsed := now.Sub(since).Seconds()
	rate := 0.0
	if elapsed > 0 {
		rate = float64(done) / elapsed
	}
	var eta time.Duration
	if final {
		eta = now.Sub(since)
	} else if rate > 0 && size > done {
		eta = time.Duration(float64(size-done) / rate * float64(time.Second))
	}
	line := fmt.Sprintf("\r%15s %3d%% %s %s", formatNum(done, p.l.opts.HumanReadable), pct, rateString(rate), clock(eta))
	if final || p.total {
		chk := "to-chk"
		if p.more {
			chk = "ir-chk"
		}
		line += fmt.Sprintf(" (xfr#%d, %s=%d/%d)", p.xfrNum, chk, p.toCheck, p.numFiles)
	}
	io.WriteString(p.l.stdout, line)
	p.onLine = true
	if final && !p.total {
		io.WriteString(p.l.stdout, "\n")
		p.onLine = false
	}
}

// rateString renders a rate as rsync's progress does: in kB/s, MB/s or
// GB/s, right-aligned.
func rateString(r float64) string {
	units := []string{"kB/s", "MB/s", "GB/s"}
	r /= 1024
	i := 0
	for r > 1024 && i < len(units)-1 {
		r /= 1024
		i++
	}
	return fmt.Sprintf("%7.2f%s", r, units[i])
}

func clock(d time.Duration) string {
	s := int64(d.Seconds())
	return fmt.Sprintf("%4d:%02d:%02d", s/3600, s/60%60, s%60)
}
