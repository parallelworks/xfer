# xfer

xfer is a clean-room Go implementation of rsync. It speaks the rsync wire
protocol (versions 27 to 33), so it works with GNU rsync and openrsync on
the other end, and it is built to be embedded: the whole command is a Go
function, the transport is pluggable, and the server half can run inside
any program that accepts SSH sessions.

- Wire-compatible with GNU rsync 3.1.3 and later, and with openrsync.
- Faster than GNU rsync 3.5.1 on every row of the benchmark below.
- One static binary, no cgo, FIPS 140-3 mode supported.
- Apache-2.0. No GNU code was used; see [CONTRIBUTING.md](CONTRIBUTING.md).

## Install

Download a release archive (static binaries for Linux and macOS on amd64
and arm64), or build from source with Go 1.26 or later:

    go install github.com/parallelworks/xfer/cmd/xfer@latest

## Command-line usage

xfer takes rsync's command line after the `rsync` subcommand:

    xfer rsync -av --delete src/ user@host:/backup/src/
    xfer rsync -az --progress host:/data/ ./data/
    xfer rsync -ai --dry-run src/ dst/

Under the name `rsync`, through a symlink or a copy, it behaves as rsync
directly. That's how a remote side runs it:

    ln -s "$(command -v xfer)" ~/bin/rsync
    rsync -a src/ host:dst/                     # xfer on both ends
    xfer rsync -a --rsync-path='xfer rsync' src/ host:dst/

Output follows GNU rsync's byte for byte for `-v`, `-vv`, `-i`, `--stats`,
`--progress`, `--out-format` and `--delete`. Golden tests check this
against rsync 3.5.1's recorded output. Exit codes are rsync's. An option
xfer does not implement fails with a clear error rather than being
ignored (see the compatibility matrix below).

## Embedding

The `rsync` package is the whole tool as a library. Its stable surface:

| Identifier | Purpose |
|---|---|
| `rsync.Main(ctx, args, stdio)` | the complete command; returns the exit code |
| `rsync.Client`, `rsync.Options`, `rsync.ParseArgs` | run transfers from code |
| `rsync.Serve(ctx, args, conn, stderr)` | the server half (`rsync --server …`) |
| `rsync.Stats`, `rsync.Event` | results and the structured event stream |
| `transport.Transport` | how the remote side is started |
| `sshtransport.New(client)` | a Transport over an existing `x/crypto/ssh` client |

Everything takes a `context.Context`, and canceling it stops the transfer
and its remote side. The package keeps no global state, so transfers can
run concurrently in one process.

### As a cobra subcommand

Let rsync parse its own flags:

```go
cmd := &cobra.Command{
	Use:                "rsync [OPTION]... SRC... DEST",
	Short:              "Fast incremental file transfer",
	DisableFlagParsing: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		code := rsync.Main(cmd.Context(), args, rsync.Stdio{
			Stdin: cmd.InOrStdin(), Stdout: cmd.OutOrStdout(), Stderr: cmd.ErrOrStderr(),
		})
		if code != 0 {
			os.Exit(code)
		}
		return nil
	},
}
```

### Over an SSH connection you already hold

```go
o, operands, err := rsync.ParseArgs([]string{"-a", "--delete", "src/", "host:/data/"})
if err != nil {
	return err
}
c := &rsync.Client{
	Transport: sshtransport.New(sshClient), // runs "rsync --server …" in a session
	Stdout:    io.Discard,
	Events: func(e rsync.Event) {
		switch e.Kind {
		case rsync.EventItem:
			ui.File(e.Name, e.Item, e.Size) // e.Item is -i's ">f+++++++++"
		case rsync.EventProgress:
			ui.Progress(e.Name, e.Bytes, e.Size)
		case rsync.EventDeleted:
			ui.Deleted(e.Name)
		case rsync.EventError:
			ui.Warn(e.Message)
		}
	},
}
stats, err := c.Run(ctx, o, operands)
```

`Events` gets items, per-file progress (at most four updates a second),
deletions, messages and errors, whatever the output options are, so a
program can draw its own UI and discard the text output. Events are
delivered one at a time from the transfer's goroutines.

`sshtransport.Transport` also takes a `Dial` function, so a pool of
connections can serve many hosts.

### The server side, inside an agent

A program that accepts SSH sessions can answer rsync clients without
installing rsync: when a session's command starts with `rsync --server`,
hand the session to `Serve`.

```go
func handleExec(ctx context.Context, sess ssh.Channel, argv []string, stderr io.Writer) int {
	if len(argv) > 1 && argv[0] == "rsync" && argv[1] == "--server" {
		err := rsync.Serve(ctx, argv[1:], sess, stderr)
		return rsync.ExitCode(err)
	}
	// ...
}
```

The `sshtransport` tests run this exact path. An in-process
`x/crypto/ssh` server hands `rsync --server` sessions to `Serve`, and
xfer's client pushes and pulls through `sshtransport` against it.

## FIPS posture

xfer builds and runs under Go's FIPS 140-3 mode (`GODEBUG=fips140=on`, or
`=only`, which CI tests). It uses checksums to find changed data, not to
protect it: confidentiality and integrity in transit come from the
transport (SSH). In FIPS mode xfer:

- negotiates only xxHash checksums (xxh128, xxh3, xxh64), which are not
  cryptographic and are not in the FIPS boundary;
- refuses MD4 and MD5, so peers that offer nothing else (GNU rsync before
  3.2.0, openrsync, or protocol 29 and older) fail with a clear error
  instead of silently falling back.

The SSH layer has to be FIPS-compatible too. In FIPS 140-only mode Go
refuses `x/crypto/ssh`'s default X25519 key exchange and its GCM ciphers,
so an embedder using `sshtransport` there should set the client's
`ssh.Config` to approved algorithms, for example key exchange
`ecdh-sha2-nistp256`, cipher `aes128-ctr` and MAC
`hmac-sha2-256-etm@openssh.com`, as the `sshtransport` tests do.

`TestFIPS` checks the refusal, and the full suite runs with
`GODEBUG=fips140=only`, where Go panics on any use of a non-approved
algorithm.

## Compatibility

Interoperability is tested in both directions (xfer as client and as
server, pushing and pulling) on every scenario in
`rsync/interop_test.go`, in CI and on Linux test hosts.

| Peer | Protocol | Status |
|---|---|---|
| GNU rsync 3.5.0, 3.5.1 | 33 | full suite passes |
| GNU rsync 3.4.4 | 32 | full suite passes |
| GNU rsync 3.2.7 | 32 | passes; `--link-dest`/`--copy-dest`/`--compare-dest` skipped with 3.2.7 receiving, since it fails those against itself |
| GNU rsync 3.1.3 | 31 | full suite passes (MD5 checksums; not in FIPS mode) |
| openrsync (macOS) | 29 | passes for the options openrsync implements |

Supported: archive mode and its parts (`-rlptgoD`), `-H`, `-x`, `-R`,
`--delete` in all its timings with `--max-delete`, `--delete-excluded` and
`--force`, filters (`--filter`, `--exclude`, `--include`, `-F`, `-C`,
per-directory merge files), `--files-from` (local and remote, `-0`),
checksums (`-c`, `--checksum-choice`), compression (`-z` with zstd, lz4,
zlibx, zlib, and `--skip-compress`), `--inplace`, `--partial`,
`--partial-dir`, `--delay-updates`, `-S`, `--preallocate`, `--backup`,
`--backup-dir`, `--suffix`, `--link-dest`, `--copy-dest`,
`--compare-dest`, `--remove-source-files`, `--existing`,
`--ignore-existing`, `-u`, `--size-only`, `-I`, `--modify-window`, `-m`,
`-k`, `-K`, `-L`, `--copy-unsafe-links`, `--safe-links`, `--chmod`,
`--numeric-ids`, `--super`, `--timeout`, `--contimeout`, `--bwlimit`,
`-s`, `--mkpath`, `-n`, `--list-only`, `--info`, `--debug`, `-v`, `-q`,
`-i`, `--out-format`, `--stats`, `-h`, `--progress`, `-8`, and
incremental recursion.

Not implemented, and refused with an error: `-A`/`--acls`,
`-X`/`--xattrs`, `-U`/`--atimes`, `-N`/`--crtimes`, `--fake-super`,
`--copy-devices`, `--write-devices`, `--munge-links`, `--append`,
`--append-verify`, `--delete-missing-args`, `--ignore-missing-args`,
`-y`/`--fuzzy`, `--log-file`, `--log-file-format`, batch mode
(`--read-batch`, `--write-batch`, `--only-write-batch`), `--old-args`,
`--usermap`, `--groupmap`, `--chown`, `--stop-after`, `--stop-at`,
`--iconv`, `--copy-as`, and daemon mode (`--daemon`, `rsync://`, `--port`,
`--address`, `--password-file`, `--config`, `--sockopts`).

## Performance

`bench/bench.py` compares xfer with GNU rsync 3.5.1. Every row runs push
and pull over a remote shell, with `sync` before each run, and reports the
median of 5 runs. The data lives on tmpfs, on an 8-CPU Linux pod:

    python3 bench/bench.py --xfer ./xfer --dir /mem/bench --runs 5

| row | dir | GNU s | xfer s | xfer/GNU | GNU RSS MB | xfer RSS MB |
|---|---|---:|---:|---:|---:|---:|
| initial: 20k files + 4×256 MB | push | 0.83 | 0.74 | 0.89 | 8 | 8 |
| | pull | 0.80 | 0.74 | 0.93 | 8 | 9 |
| no-op resync | push | 0.11 | 0.05 | 0.49 | 8 | 8 |
| | pull | 0.09 | 0.05 | 0.60 | 8 | 8 |
| delta: 1 MB changed per big file | push | 0.73 | 0.46 | 0.64 | 8 | 9 |
| | pull | 0.71 | 0.46 | 0.65 | 8 | 9 |
| `-c` | push | 0.50 | 0.33 | 0.64 | 8 | 12 |
| | pull | 0.49 | 0.32 | 0.66 | 8 | 11 |
| `-z` (zstd), compressible text | push | 1.26 | 1.06 | 0.85 | 8 | 24 |
| | pull | 1.23 | 1.07 | 0.87 | 8 | 24 |
| `--delete`, 5k removed | push | 0.15 | 0.10 | 0.68 | 8 | 8 |
| | pull | 0.13 | 0.10 | 0.77 | 8 | 8 |
| 200k files, initial copy | push | 4.29 | 3.74 | 0.87 | 8 | 12 |
| | pull | 4.29 | 3.75 | 0.87 | 8 | 12 |
| 200k files, no-op | push | 0.69 | 0.43 | 0.62 | 8 | 9 |
| | pull | 0.68 | 0.44 | 0.65 | 8 | 9 |

Peak memory on the 200k-file tree stays within 1.5× of GNU rsync's,
thanks to incremental recursion. With `--ssh HOST` the harness adds a row
over real OpenSSH between two machines. That row measures the network
and is reported, not gated.

## Development

See [CONTRIBUTING.md](CONTRIBUTING.md) for building, the test suites
(unit, interop, golden output, FIPS, fuzzing), the release process and
the clean-room policy.
