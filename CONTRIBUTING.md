# Contributing to xfer

xfer is a clean-room, Apache-2.0 implementation of rsync, wire-compatible
with GNU rsync (protocols 27 to 33) and openrsync. Read the clean-room
policy below before writing any code.

## Building

    go build -o bin/xfer ./cmd/xfer

The binary runs rsync as `xfer rsync [OPTION]... SRC... DEST`, or directly
when invoked under the name `rsync` (for example through a symlink). Release
builds are static (`CGO_ENABLED=0`); see `.goreleaser.yaml`.

## Releasing

Releases are cut by [release-please](https://github.com/googleapis/release-please)
from the commit history. It keeps a release PR open against `canary` with
the next version and its changelog; merging that PR tags the release,
creates the GitHub release, and runs goreleaser, which attaches static
binaries for Linux and macOS on amd64 and arm64. Don't push tags by hand.

xfer is pre-1.0: `feat` commits bump the minor version and everything else
the patch version, and the API may change between minor versions until 1.0.

## Testing

    go vet ./...
    go test ./...                 # unit, local and interop tests
    go test -short ./...          # skip the interop and slower tests
    go test -race ./...           # what CI runs on Linux and macOS

Lint as CI does:

    go run honnef.co/go/tools/cmd/staticcheck@latest ./...
    go run golang.org/x/vuln/cmd/govulncheck@latest ./...
    gofmt -l .                    # must print nothing

### FIPS

    GODEBUG=fips140=only go test ./...

In this mode Go panics on MD4, MD5 and SHA-1, so the run proves that no
code path reaches them. xfer refuses them by design in FIPS mode, so
scenarios that need them (protocol < 30, or a peer without xxHash such as
GNU rsync before 3.2.0 or openrsync) skip themselves; `TestFIPS` checks the
refusal.

### Interop

`TestInterop` (in `rsync/interop_test.go`) runs every scenario between xfer
and a reference rsync in both directions, over a fake remote shell:

    XFER_INTEROP_RSYNC=/usr/bin/rsync go test -run TestInterop ./rsync

- `XFER_INTEROP_RSYNC` names the reference binary; without it the test uses
  `rsync` from `$PATH`, and skips if there is none.
- `XFER_INTEROP_XFER` names a prebuilt xfer binary (useful inside a
  container); otherwise the test builds one with the go command.
- `XFER_INTEROP_RACE=1` builds that binary with `-race`.

CI covers GNU rsync 3.1.3, 3.2.7, 3.4.4, 3.5.0 and 3.5.1 and macOS
openrsync. When you change the protocol, test against at least the oldest
and newest of these.

### Output

`TestGolden` (in `rsync/golden_test.go`) holds xfer's `-v`, `-vv`, `-i`,
`--stats`, `--progress`, `--out-format` and `--delete` output to rsync's,
recorded from GNU rsync 3.5.1 in `rsync/testdata/golden`. Timing-dependent
parts are masked, and byte counts are compared with a small tolerance,
since rsync's own counts vary with the framing of its output. To
regenerate the recordings on Linux with rsync 3.5.1:

    XFER_GOLDEN_UPDATE=1 XFER_INTEROP_RSYNC=/usr/bin/rsync go test -run '^TestGolden$' ./rsync

`TestGoldenLive` compares against a live rsync of protocol 33 in local,
push and pull transfers whenever `XFER_INTEROP_RSYNC` names one.

### Fuzzing

    grep -rn '^func Fuzz' --include='*_test.go' .
    go test -run '^$' -fuzz '^FuzzDecode$' -fuzztime 60s ./rsync/internal/flist

Every decoder that reads from the network should have a fuzz target. Add
crashers that `go test -fuzz` saves under `testdata/fuzz` to the commit that
fixes them.

## Benchmarking

`bench/bench.py` compares xfer with GNU rsync, push and pull over a fake
remote shell, and reports median wall time and peak RSS. Run it on Linux
with GNU rsync installed and the data directory on tmpfs:

    GOOS=linux CGO_ENABLED=0 go build -o xfer ./cmd/xfer
    python3 bench/bench.py --xfer ./xfer --dir /mem/bench [--runs N] [--only ROW]

`--only` may be repeated; the rows are listed at the top of the script.
`--ssh HOST` adds a row over real OpenSSH to another machine, which needs
GNU rsync and xfer there (`--ssh-xfer`) and key-based login.
Include before and after numbers in performance changes.

### Soak test

`bench/soak.sh` checks that interrupted transfers resume correctly: it
builds a large tree (5 GB by default), then over 10 rounds changes the
source, interrupts a push or pull with `--partial` at a random point
(SIGINT or SIGKILL), resumes it, and compares SHA-256 manifests of the
two trees. Run it on Linux with a local disk:

    bench/soak.sh ./xfer /var/tmp/soak [ROUNDS] [SIZE_GB]

## Commits

- One logical change per commit; tests and code together.
- Subject: a [Conventional Commit](https://www.conventionalcommits.org/),
  `type(scope): what changed`, lower case, no trailing period, about 72
  characters at most, e.g. `feat(rsync): incremental recursion`. Types are
  `feat`, `fix`, `perf`, `test`, `docs`, `ci`, `build`, `style`, `refactor`
  and `chore`; the scope is a package or directory (`rsync`, `token`,
  `bench`, ...) and may be left out. release-please builds the changelog
  from these: `feat`, `fix` and `perf` appear in it, and a `!` after the
  type or a `BREAKING CHANGE:` footer marks an incompatible change.
- Keep history linear: rebase rather than merge. PRs are squash-merged,
  so a PR's title becomes its commit and is checked the same way.
- A body, when needed, explains why, wrapped at 72 columns.
- `go vet`, `gofmt`, staticcheck and `go test ./...` must pass.
- A `Signed-off-by:` line (`git commit -s`) is welcome but not required.
- New files start with the copyright and `SPDX-License-Identifier:
  Apache-2.0` header used throughout the repository.

By contributing you agree that your contribution is licensed under the
Apache License, Version 2.0.

## Clean-room policy

xfer must remain an independent implementation that can be licensed under
Apache-2.0. GNU rsync is GPL-licensed, so:

- **Never copy, translate or paraphrase code** from GNU rsync or any other
  GPL (or otherwise incompatibly licensed) implementation, including
  porting a function from C to Go, following its structure line by line,
  or reusing its identifiers, comments or tables.
- **Do not read GPL rsync source while working on xfer.** If you have
  studied it in depth, say so in your pull request so the change can be
  reviewed with that in mind.
- **Derive behavior only from** documentation and man pages, published
  specifications, observing wire traffic between reference binaries, and
  black-box testing against reference binaries (the interop suite is the
  main tool for this).
- **Protocol notes are prose specifications**, not code: describe message
  layouts, flags and ordering in your own words; do not include source
  excerpts or pseudo-code transcribed from an implementation.
- Third-party Go dependencies must have Apache-2.0-compatible licenses.

When in doubt, ask in the pull request before writing the code.
