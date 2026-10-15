// Copyright 2026 Parallel Works Inc.
// SPDX-License-Identifier: Apache-2.0

// Command xfer is a collection of file transfer tools.
//
//	xfer rsync [OPTION]... SRC... DEST
//
// When the binary is invoked under the name "rsync" (for example through a
// symlink), it behaves as rsync directly, so it can be used as a remote
// rsync with --rsync-path or installed in place of rsync.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/parallelworks/xfer/rsync"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	// A peer that goes away must surface as a write error we can clean up
	// after, not kill the process.
	signal.Ignore(syscall.SIGPIPE)

	name := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
	args := os.Args[1:]
	if name != "rsync" {
		if len(args) == 0 {
			usage()
			os.Exit(1)
		}
		switch args[0] {
		case "rsync":
			args = args[1:]
		case "version", "--version":
			fmt.Printf("xfer %s\n", rsync.Version)
			return
		case "help", "-h", "--help":
			usage()
			return
		default:
			fmt.Fprintf(os.Stderr, "xfer: unknown command %q\n", args[0])
			usage()
			os.Exit(1)
		}
	}
	// The live heap of a transfer is small (a few file lists and
	// buffers), so the collector's default target would leave the heap
	// mostly garbage. Collect more often unless GOGC says otherwise.
	if os.Getenv("GOGC") == "" {
		debug.SetGCPercent(25)
	}
	stopProfile := startProfile()
	code := rsync.Main(ctx, args, rsync.Stdio{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr})
	stopProfile()
	os.Exit(code)
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: xfer <command> [arguments]

commands:
  rsync     fast incremental file transfer, compatible with rsync
  version   print the version
`)
}
