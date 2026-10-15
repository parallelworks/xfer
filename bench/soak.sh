#!/bin/sh
# Copyright 2026 Parallel Works Inc.
# SPDX-License-Identifier: Apache-2.0
#
# Soak test: transfers that are interrupted and then resumed must end with
# a destination byte-identical to the source.
#
#     bench/soak.sh XFER DIR [ITERATIONS] [SIZE_GB]
#
# It builds a tree of SIZE_GB (default 5) in DIR/src: large files plus
# 20,000 small ones. Each of ITERATIONS (default 10) rounds then:
#   - changes part of the source (after the first round): rewrites a MB in
#     two large files, edits, adds and removes small files, and removes
#     two large files from the destination so they are sent again;
#   - starts xfer pushing or pulling (alternately) over a remote shell
#     with --partial --delete, limited to 100 MB/s so that it is still
#     running, and interrupts it after a random delay, with SIGINT or
#     SIGKILL (alternately);
#   - runs the same transfer again to completion;
#   - compares SHA-256 manifests of every file in the source and the
#     destination, so a stray temporary file fails the round too.
# Needs sh, python3 and coreutils-like tools; run it on a local disk.

set -eu
X=$(cd "$(dirname "$1")" && pwd)/$(basename "$1")
D=$2
N=${3:-10}
GB=${4:-5}
mkdir -p "$D"
D=$(cd "$D" && pwd)
src=$D/src dst=$D/dst

printf '#!/bin/sh\nshift\nexec sh -c "$*"\n' > "$D/fakessh"
chmod +x "$D/fakessh"

if [ ! -f "$D/.ready" ]; then
	echo "building a ${GB} GB tree in $src"
	python3 - "$src" "$GB" <<'EOF'
import os, random, sys
root, gb = sys.argv[1], int(sys.argv[2])
rng = random.Random(7)
os.makedirs(root, exist_ok=True)
small = 20000
for i in range(small):
    d = os.path.join(root, "small", "d%03d" % (i // 100))
    os.makedirs(d, exist_ok=True)
    with open(os.path.join(d, "f%05d" % i), "wb") as f:
        f.write(rng.randbytes(1000 + i % 9000))
left = (gb << 30) - small * 5500
n = max(1, left // (512 << 20))
for i in range(n):
    with open(os.path.join(root, "big%02d" % i), "wb") as f:
        for _ in range(left // n >> 20):
            f.write(rng.randbytes(1 << 20))
EOF
	touch "$D/.ready"
fi

manifest() {
	(cd "$1" && find . -type f -exec sha256sum {} + | sort -k 2)
}

change() {
	python3 - "$src" "$1" "$dst" <<'EOF'
import os, random, sys
root, seed, dst = sys.argv[1], int(sys.argv[2]), sys.argv[3]
rng = random.Random(seed)
bigs = sorted(f for f in os.listdir(root) if f.startswith("big"))
for name in rng.sample(bigs, min(2, len(bigs))):
    if os.path.exists(os.path.join(dst, name)):
        os.remove(os.path.join(dst, name))
for name in rng.sample(bigs, min(2, len(bigs))):
    p = os.path.join(root, name)
    with open(p, "r+b") as f:
        f.seek(rng.randrange(max(1, os.path.getsize(p) - (1 << 20))))
        f.write(rng.randbytes(1 << 20))
small = os.path.join(root, "small")
dirs = sorted(os.listdir(small))
for _ in range(200):
    d = os.path.join(small, rng.choice(dirs))
    files = sorted(os.listdir(d))
    op = rng.randrange(3)
    if op == 0 and files:
        with open(os.path.join(d, rng.choice(files)), "wb") as f:
            f.write(rng.randbytes(rng.randrange(100, 20000)))
    elif op == 1:
        with open(os.path.join(d, "new%06d" % rng.randrange(10**6)), "wb") as f:
            f.write(rng.randbytes(rng.randrange(100, 20000)))
    elif files:
        os.remove(os.path.join(d, rng.choice(files)))
EOF
}

# transfer runs one transfer; with EXEC=exec it replaces the (background)
# shell, so that $! is xfer itself and the interrupt reaches it.
transfer() {
	dir=$1
	shift
	if [ "$dir" = push ]; then
		${EXEC:-} "$X" rsync -a --partial --delete "$@" -e "$D/fakessh" --rsync-path="$X rsync" "$src/" "host:$dst/"
	else
		${EXEC:-} "$X" rsync -a --partial --delete "$@" -e "$D/fakessh" --rsync-path="$X rsync" "host:$src/" "$dst/"
	fi
}

rm -rf "$dst"
fail=0
i=1
while [ "$i" -le "$N" ]; do
	[ "$i" -gt 1 ] && change "$i"
	if [ $((i % 2)) -eq 1 ]; then dir=push; else dir=pull; fi
	if [ $((i % 4)) -lt 2 ]; then sig=INT; else sig=KILL; fi
	delay=$(python3 -c "import random; random.seed($i); print('%.1f' % random.uniform(0.3, 4.0))")
	EXEC=exec transfer "$dir" --bwlimit=100m >/dev/null 2>"$D/interrupted.err" </dev/null &
	pid=$!
	sleep "$delay"
	if kill -s "$sig" "$pid" 2>/dev/null; then
		state="interrupted with SIG$sig after ${delay}s"
	else
		state="NOT INTERRUPTED: finished within ${delay}s"
		fail=1
	fi
	wait "$pid" 2>/dev/null || true
	if ! transfer "$dir" >/dev/null 2>"$D/resume.err" </dev/null; then
		echo "round $i ($dir): the resumed transfer failed:"
		cat "$D/resume.err"
		fail=1
		break
	fi
	if manifest "$src" > "$D/src.sha" && manifest "$dst" > "$D/dst.sha" && cmp -s "$D/src.sha" "$D/dst.sha"; then
		echo "round $i ($dir, $state): identical, $(wc -l < "$D/dst.sha") files"
	else
		echo "round $i ($dir, $state): DIFFERENT"
		diff "$D/src.sha" "$D/dst.sha" | head -20
		fail=1
	fi
	i=$((i + 1))
done
if [ "$fail" -eq 0 ]; then
	echo "PASS: $N interrupted transfers resumed byte-identical"
else
	echo FAIL
fi
exit "$fail"
