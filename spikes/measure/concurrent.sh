#!/usr/bin/env bash
# Phase 1 measurement: two cold readers of different files, alone and at the
# same time, plus metadata latency while both read. Every case gets a fresh
# mount and an empty cache.
# Usage: concurrent.sh <mount-dir> <work-dir> <rounds>
set -euo pipefail

mnt=$1 work=$2 rounds=${3:-1}
bin=$(cd "$(dirname "$0")/../.." && pwd)/bin/ucfs

now() { perl -MTime::HiRes=time -e 'printf "%.3f\n", time'; }
pick() { ls "$mnt/files" | grep -m1 "_$1\$"; }

up() {
	cache=$(mktemp -d "$work/cache-cc-XXXX")
	"$bin" --cache-dir "$cache" mount "$mnt" >"$cache/mount.log" 2>&1 &
	pid=$!
	for _ in $(seq 100); do
		"$bin" --cache-dir "$cache" status >/dev/null 2>&1 && break
		sleep 0.2
	done
	a="$mnt/files/$(pick ucfs-seq16.jpg)"
	b="$mnt/files/$(pick ucfs-seq12.jpg)"
}
down() {
	"$bin" --cache-dir "$cache" umount "$mnt"
	wait "$pid"
}
timed() {
	local t0
	t0=$(now)
	"$@" >/dev/null
	echo "$(now) - $t0" | bc
}

for _ in $(seq "$rounds"); do
	up
	echo "solo     A (16 MB) $(timed cat "$a")s"
	down
	up
	echo "solo     B (12 MB) $(timed cat "$b")s"
	down
	up
	out=$(mktemp -d "$work/cc-XXXX")
	(timed cat "$a" >"$out/a") &
	ra=$!
	(timed cat "$b" >"$out/b") &
	rb=$!
	sleep 0.3
	echo "during   stat      $(timed stat "$mnt/files")s"
	echo "during   ls -l     $(timed ls -l "$mnt/files")s"
	wait "$ra" "$rb"
	echo "together A (16 MB) $(cat "$out/a")s"
	echo "together B (12 MB) $(cat "$out/b")s"
	down
done
