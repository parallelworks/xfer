// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

package rsync

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Stats summarizes a transfer.
type Stats struct {
	NumFiles                                                                              int64 // entries in the file list
	NumRegFiles                                                                           int64
	NumDirs                                                                               int64
	NumLinks                                                                              int64
	NumDevices                                                                            int64
	NumSpecials                                                                           int64
	NumCreated                                                                            int64 // entries the transfer created, by type below
	NumCreatedReg, NumCreatedDirs, NumCreatedLinks, NumCreatedDevices, NumCreatedSpecials int64
	NumDeleted                                                                            int64
	NumTransferred                                                                        int64

	TotalSize            int64 // bytes in all files of the list
	TotalTransferredSize int64 // bytes in the files transferred
	LiteralData          int64
	MatchedData          int64
	// BlocksTouched counts the distinct 4 KiB regions of files the
	// receiver wrote. Peers report it from protocol 33 on.
	BlocksTouched  int64
	FlistSize      int64 // bytes of the file list received; 0 when sending, as rsync reports it
	FlistBuildTime time.Duration
	FlistXferTime  time.Duration

	BytesSent     int64 // on the wire
	BytesReceived int64

	Protocol int // the protocol version the transfer used
}

// countingConn counts bytes crossing a connection.
// It also records when a read or write started blocking and when the last
// write ended, for --timeout.
type countingConn struct {
	rw      io.ReadWriter
	read    atomic.Int64
	written atomic.Int64

	readSince, writeSince atomic.Int64 // UnixNano a call began, 0 when none is in progress
	lastWrite             atomic.Int64 // UnixNano the last write ended
}

func (c *countingConn) Read(p []byte) (int, error) {
	c.readSince.Store(time.Now().UnixNano())
	n, err := c.rw.Read(p)
	c.readSince.Store(0)
	c.read.Add(int64(n))
	return n, err
}

func (c *countingConn) Write(p []byte) (int, error) {
	c.writeSince.Store(time.Now().UnixNano())
	n, err := c.rw.Write(p)
	c.lastWrite.Store(time.Now().UnixNano())
	c.writeSince.Store(0)
	c.written.Add(int64(n))
	return n, err
}

// formatNum formats n the way rsync does: with thousands separators, or
// with -h as a human-readable size.
func formatNum(n int64, human int) string {
	if human > 1 {
		return humanSize(n, 1024)
	}
	if human == 1 {
		return humanSize(n, 1000)
	}
	return commaNum(n)
}

func commaNum(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	pre := len(s) % 3
	if pre == 0 {
		pre = 3
	}
	b.WriteString(s[:pre])
	for i := pre; i < len(s); i += 3 {
		b.WriteByte(',')
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

func humanSize(n int64, unit float64) string {
	f := float64(n)
	if f < unit {
		return strconv.FormatInt(n, 10)
	}
	for _, suffix := range "KMGTP" {
		f /= unit
		if f < unit || suffix == 'P' {
			return fmt.Sprintf("%.2f%c", f, suffix)
		}
	}
	return strconv.FormatInt(n, 10)
}

// printKinds ends a count's line with its breakdown by file type.
func printKinds(w io.Writer, n [5]int64) {
	var parts []string
	for i, name := range []string{"reg", "dir", "link", "dev", "special"} {
		if n[i] > 0 {
			parts = append(parts, fmt.Sprintf("%s: %s", name, commaNum(n[i])))
		}
	}
	if len(parts) > 0 {
		fmt.Fprintf(w, " (%s)", strings.Join(parts, ", "))
	}
	fmt.Fprintln(w)
}

// writeSummary prints the totals shown at the end of a verbose transfer,
// and the detailed --stats report.
func writeSummary(w io.Writer, st *Stats, o *Options, lv levels, wholeSecs int64) {
	h := o.HumanReadable
	if lv.info[infoStats] > 1 {
		fmt.Fprintf(w, "\nNumber of files: %s", commaNum(st.NumFiles))
		printKinds(w, [5]int64{st.NumRegFiles, st.NumDirs, st.NumLinks, st.NumDevices, st.NumSpecials})
		fmt.Fprintf(w, "Number of created files: %s", commaNum(st.NumCreated))
		printKinds(w, [5]int64{st.NumCreatedReg, st.NumCreatedDirs, st.NumCreatedLinks, st.NumCreatedDevices, st.NumCreatedSpecials})
		fmt.Fprintf(w, "Number of deleted files: %s\n", commaNum(st.NumDeleted))
		fmt.Fprintf(w, "Number of regular files transferred: %s\n", commaNum(st.NumTransferred))
		fmt.Fprintf(w, "Total file size: %s bytes\n", formatNum(st.TotalSize, h))
		fmt.Fprintf(w, "Total transferred file size: %s bytes\n", formatNum(st.TotalTransferredSize, h))
		fmt.Fprintf(w, "Literal data: %s bytes\n", formatNum(st.LiteralData, h))
		if st.Protocol >= 33 {
			fmt.Fprintf(w, "Number of 4 KiB logical blocks touched: %s\n", commaNum(st.BlocksTouched))
		}
		fmt.Fprintf(w, "Matched data: %s bytes\n", formatNum(st.MatchedData, h))
		fmt.Fprintf(w, "File list size: %s\n", formatNum(st.FlistSize, h))
		fmt.Fprintf(w, "File list generation time: %.3f seconds\n", st.FlistBuildTime.Seconds())
		fmt.Fprintf(w, "File list transfer time: %.3f seconds\n", st.FlistXferTime.Seconds())
		fmt.Fprintf(w, "Total bytes sent: %s\n", formatNum(st.BytesSent, h))
		fmt.Fprintf(w, "Total bytes received: %s\n", formatNum(st.BytesReceived, h))
	}
	if lv.info[infoStats] == 0 {
		return
	}
	// rsync times the run in whole seconds and adds half a second.
	rate := float64(st.BytesSent+st.BytesReceived) / (0.5 + float64(wholeSecs))
	fmt.Fprintf(w, "\nsent %s bytes  received %s bytes  %s bytes/sec\n",
		formatNum(st.BytesSent, h), formatNum(st.BytesReceived, h), formatRate(rate, h))
	speedup := 0.0
	if st.BytesSent+st.BytesReceived > 0 {
		speedup = float64(st.TotalSize) / float64(st.BytesSent+st.BytesReceived)
	}
	suffix := ""
	if o.DryRun {
		suffix = " (DRY RUN)"
	}
	fmt.Fprintf(w, "total size is %s  speedup is %.2f%s\n", formatNum(st.TotalSize, h), speedup, suffix)
}

func formatRate(r float64, human int) string {
	if human > 0 {
		return formatNum(int64(r), human)
	}
	s := strconv.FormatFloat(r, 'f', 2, 64)
	whole, frac, _ := strings.Cut(s, ".")
	n, _ := strconv.ParseInt(whole, 10, 64)
	return commaNum(n) + "." + frac
}
