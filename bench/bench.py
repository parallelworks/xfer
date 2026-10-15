#!/usr/bin/env python3
# Copyright 2026 Parallel Works Inc.
# SPDX-License-Identifier: Apache-2.0
"""Benchmark xfer rsync against GNU rsync.

Every row runs push and pull over a fake remote shell (the "remote" side is
started through sh on the same machine), RUNS times each, and reports the
median wall time and the peak RSS. Run it on Linux with GNU rsync installed
(on Alpine: apk add rsync python3):

    python3 bench/bench.py --xfer ./xfer --dir /mem/bench [--runs 5] [--only ROW]

The data directory should be on tmpfs so disk writeback does not swamp the
comparison. Results print as a Markdown table and are written as JSON next
to the data.

With --ssh HOST the "ssh-initial" row also runs: the initial copy of the
main tree over OpenSSH to and from HOST, which needs GNU rsync, the xfer
binary (--ssh-xfer) and room for the tree (--ssh-dir) there, and
key-based login. It measures a real network and is reported, not gated.
"""

import argparse
import json
import os
import random
import shutil
import statistics
import subprocess
import sys

# MEASURE runs a command and reports its wall time and the peak RSS (KiB)
# of the largest process in its tree; the client waits for the server, so
# both sides are covered.
MEASURE = """
import resource, subprocess, sys, time
t = time.monotonic()
rc = subprocess.call(sys.argv[1:], stdout=subprocess.DEVNULL)
el = time.monotonic() - t
print("TIME %.3f %d" % (el, resource.getrusage(resource.RUSAGE_CHILDREN).ru_maxrss), file=sys.stderr)
sys.exit(rc)
"""

ROWS = ["initial", "noop", "delta", "checksum", "compress", "delete", "many-initial", "many-noop"]


def sh(*cmd, **kw):
    subprocess.run(cmd, check=True, **kw)


def write_tree(root, dirs, per_dir, size_fn, rng):
    for d in range(dirs):
        p = os.path.join(root, "d%03d" % d)
        os.makedirs(p, exist_ok=True)
        for f in range(per_dir):
            with open(os.path.join(p, "f%03d" % f), "wb") as fh:
                fh.write(rng.randbytes(size_fn(d, f)))


def write_big(root, n, size, rng):
    os.makedirs(root, exist_ok=True)
    for i in range(n):
        with open(os.path.join(root, "big%d" % i), "wb") as fh:
            for _ in range(size >> 20):
                fh.write(rng.randbytes(1 << 20))


def text_tree(root, files, size, rng):
    words = [bytes(rng.choice(b"abcdefghijklmnopqrstuvwxyz") for _ in range(rng.randint(2, 10))) for _ in range(5000)]
    os.makedirs(root, exist_ok=True)
    for i in range(files):
        out, n = [], 0
        while n < size:
            line = b" ".join(rng.choice(words) for _ in range(12)) + b"\n"
            out.append(line)
            n += len(line)
        with open(os.path.join(root, "t%03d.txt" % i), "wb") as fh:
            fh.write(b"".join(out))


def setup(base):
    """Creates the datasets once; later runs reuse them."""
    rng = random.Random(1)
    mark = os.path.join(base, ".ready")
    if os.path.exists(mark):
        return
    shutil.rmtree(base, ignore_errors=True)
    os.makedirs(base)
    # The main tree: 20k small files and 4 x 256MB.
    old = os.path.join(base, "old")
    write_tree(old, 200, 100, lambda d, f: 2000 + (d * f) % 8000, rng)
    write_big(old, 4, 256 << 20, rng)
    sh("cp", "-a", old, os.path.join(base, "new"))
    for i in range(4):  # the delta: 1MB changed in the middle of each big file
        p = os.path.join(base, "new", "big%d" % i)
        with open(p, "r+b") as fh:
            fh.seek(100 << 20)
            fh.write(rng.randbytes(1 << 20))
        os.utime(p, (1_700_000_000, 1_700_000_000))
    # 5k extra files on the receiving side for --delete.
    write_tree(os.path.join(base, "extra"), 50, 100, lambda d, f: 100, rng)
    # Compressible text for -z.
    text_tree(os.path.join(base, "text"), 64, 4 << 20, rng)
    # 200k small files.
    write_tree(os.path.join(base, "many"), 1000, 200, lambda d, f: 64 + (d + f) % 1024, rng)
    with open(os.path.join(base, "fakessh"), "w") as fh:
        fh.write('#!/bin/sh\nshift\nexec sh -c "$*"\n')
    os.chmod(os.path.join(base, "fakessh"), 0o755)
    open(mark, "w").close()


class Impl:
    def __init__(self, name, client, server):
        self.name, self.client, self.server = name, client, server


def prepare(row, base, dst):
    """Puts dst in the state the row starts from (untimed)."""
    if row in ("initial", "compress", "many-initial"):
        shutil.rmtree(dst, ignore_errors=True)
        return
    src = {"noop": "new", "checksum": "new", "delta": "old", "delete": "new", "many-noop": "many"}[row]
    sh("rsync", "-a", "--delete", os.path.join(base, src) + "/", dst + "/")
    if row == "delete":
        sh("cp", "-a", os.path.join(base, "extra"), os.path.join(dst, "extra"))


def row_args(row, base):
    src = {"compress": "text", "many-initial": "many", "many-noop": "many"}.get(row, "new")
    opts = {"checksum": ["-ac"], "compress": ["-a", "-z", "--compress-choice=zstd"], "delete": ["-a", "--delete"]}.get(row, ["-a"])
    return opts, os.path.join(base, src) + "/"


def ssh_setup(a, base):
    """Puts the main tree on the remote host for the pull direction."""
    ssh = ["ssh", "-o", "BatchMode=yes", a.ssh]
    sh(*ssh, "mkdir", "-p", a.ssh_dir)
    sh("rsync", "-a", "--delete", "-e", "ssh -o BatchMode=yes", os.path.join(base, "new") + "/", a.ssh + ":" + a.ssh_dir + "/new/")


def run_ssh(a, impl, base, direction):
    """Runs the ssh-initial row once: the main tree over OpenSSH."""
    server = a.rsync_remote if impl.name == "gnu" else a.ssh_xfer + " rsync"
    shell = ["-e", "ssh -o BatchMode=yes", "--rsync-path=" + server]
    if direction == "push":
        dst = a.ssh_dir + "/dst-" + impl.name
        sh("ssh", "-o", "BatchMode=yes", a.ssh, "rm", "-rf", dst)
        argv = impl.client + ["-a"] + shell + [os.path.join(base, "new") + "/", a.ssh + ":" + dst + "/"]
    else:
        dst = os.path.join(base, "dst-" + impl.name)
        shutil.rmtree(dst, ignore_errors=True)
        argv = impl.client + ["-a"] + shell + [a.ssh + ":" + a.ssh_dir + "/new/", dst + "/"]
    return measure(impl, "ssh-initial", direction, argv)


def measure(impl, row, direction, argv):
    sh("sync")
    r = subprocess.run([sys.executable, "-c", MEASURE] + argv, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, text=True)
    line = [l for l in r.stderr.splitlines() if l.startswith("TIME ")]
    if r.returncode != 0 or not line:
        sys.exit("%s %s %s failed (%d):\n%s" % (impl.name, row, direction, r.returncode, r.stderr))
    _, secs, rss = line[-1].split()
    return float(secs), int(rss)


def run_once(impl, row, base, direction):
    dst = os.path.join(base, "dst-" + impl.name)
    prepare(row, base, dst)
    opts, src = row_args(row, base)
    shell = ["-e", os.path.join(base, "fakessh"), "--rsync-path=" + impl.server]
    if direction == "push":
        argv = impl.client + opts + shell + [src, "host:" + dst + "/"]
    else:
        argv = impl.client + opts + shell + ["host:" + src, dst + "/"]
    return measure(impl, row, direction, argv)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--xfer", required=True, help="path to the xfer binary")
    ap.add_argument("--rsync", default="rsync", help="path to GNU rsync")
    ap.add_argument("--dir", required=True, help="data directory (preferably tmpfs)")
    ap.add_argument("--runs", type=int, default=5)
    ap.add_argument("--only", action="append", help="run only these rows")
    ap.add_argument("--ssh", help="also run the ssh-initial row against this [user@]host over OpenSSH")
    ap.add_argument("--ssh-dir", default="/tmp/xfer-bench", help="data directory on the ssh host")
    ap.add_argument("--ssh-xfer", default="xfer", help="path to the xfer binary on the ssh host")
    ap.add_argument("--rsync-remote", default="rsync", help="path to GNU rsync on the ssh host")
    a = ap.parse_args()
    base = os.path.abspath(a.dir)
    setup(base)
    xfer = os.path.abspath(a.xfer)
    impls = [Impl("gnu", [a.rsync], a.rsync), Impl("xfer", [xfer, "rsync"], xfer + " rsync")]
    results = []
    print("| row | dir | GNU s | xfer s | xfer/GNU | GNU RSS MB | xfer RSS MB |")
    print("|---|---|---:|---:|---:|---:|---:|")
    rows = list(a.only or ROWS)
    if a.ssh and (not a.only or "ssh-initial" in a.only):
        ssh_setup(a, base)
        if "ssh-initial" not in rows:
            rows.append("ssh-initial")
    elif "ssh-initial" in rows:
        rows.remove("ssh-initial")
    for row in rows:
        for direction in ("push", "pull"):
            res = {}
            for impl in impls:
                if row == "ssh-initial":
                    runs = [run_ssh(a, impl, base, direction) for _ in range(a.runs)]
                else:
                    runs = [run_once(impl, row, base, direction) for _ in range(a.runs)]
                res[impl.name] = (statistics.median(t for t, _ in runs), max(m for _, m in runs))
            g, x = res["gnu"], res["xfer"]
            results.append({"row": row, "dir": direction, "gnu": g, "xfer": x})
            print("| %s | %s | %.2f | %.2f | %.2f | %.0f | %.0f |" % (row, direction, g[0], x[0], x[0] / max(g[0], 1e-3), g[1] / 1024, x[1] / 1024), flush=True)
    for impl in impls:
        shutil.rmtree(os.path.join(base, "dst-" + impl.name), ignore_errors=True)
    with open(os.path.join(base, "results.json"), "w") as fh:
        json.dump(results, fh, indent=1)


if __name__ == "__main__":
    main()
