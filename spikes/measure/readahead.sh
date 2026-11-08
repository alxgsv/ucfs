#!/usr/bin/env bash
# Phase 1 measurement: cold reads through a fresh mount and an empty cache per
# macOS NFS read-ahead setting. The project must hold the fixtures named
# below (see docs/phase1.md).
# Usage: readahead.sh <mount-dir> <work-dir> <readahead>...
set -euo pipefail

mnt=$1 work=$2
shift 2
bin=$(cd "$(dirname "$0")/../.." && pwd)/bin/ucfs

now() { perl -MTime::HiRes=time -e 'printf "%.3f\n", time'; }
counters() {
	"$bin" --cache-dir "$cache" status --json "$mnt" | python3 -c '
import json, sys
s = json.load(sys.stdin)[0]
print(s["cdn"]["bytes"], s["cache"]["fetched_demand"], s["cache"]["fetched_prefetch"])'
}
pick() { ls "$mnt/files" | grep -m1 "_$1\$"; }

measure() {
	local label=$1
	shift
	read -r b0 d0 p0 < <(counters)
	local t0 t1
	t0=$(now)
	"$@" >/dev/null
	t1=$(now)
	sleep 2 # let prefetch settle before reading the counters
	read -r b1 d1 p1 < <(counters)
	awk -v ra="$ra" -v l="$label" -v t="$(echo "$t1 - $t0" | bc)" -v b=$((b1 - b0)) -v d=$((d1 - d0)) -v p=$((p1 - p0)) \
		'BEGIN { printf "readahead=%-2s %-18s %6.2fs  cdn %9d B  demand %9d  prefetch %9d\n", ra, l, t, b, d, p }'
}

for ra in "$@"; do
	cache=$(mktemp -d "$work/cache-ra$ra-XXXX")
	"$bin" --cache-dir "$cache" mount --readahead "$ra" "$mnt" >"$cache/mount.log" 2>&1 &
	pid=$!
	for _ in $(seq 100); do
		"$bin" --cache-dir "$cache" status "$mnt" >/dev/null 2>&1 && break
		sleep 0.2
	done
	measure "cat 16 MB" cat "$mnt/files/$(pick ucfs-seq16.jpg)"
	measure "head -c 4096" head -c 4096 "$mnt/files/$(pick ucfs-seq12.jpg)"
	measure "file(1)" file "$mnt/files/$(pick ucfs-photo-large.jpg)"
	"$bin" --cache-dir "$cache" umount "$mnt"
	wait "$pid"
done
