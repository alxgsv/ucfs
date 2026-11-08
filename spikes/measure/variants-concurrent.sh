#!/usr/bin/env bash
# Phase 2 measurement: three variants of one file read from three shells, each
# alone and all at once, with metadata latency of another file while they
# read. Every case gets a fresh mount and an empty cache. The variants should
# already be rendered on the CDN, so only the last case, which renders a new
# one alongside, waits for image processing.
# Usage: variants-concurrent.sh <mount-dir> <work-dir> <uuid> <other-file> <rounds> <cold-ops>
set -euo pipefail

mnt=$1 work=$2 uuid=$3 other=$4 rounds=${5:-1} cold=${6:-}
bin=${UCFS_BIN:-$(cd "$(dirname "$0")/../.." && pwd)/bin/ucfs}
ops=(-/resize/3000x/-/format/png -/resize/2800x/-/format/png -/resize/2600x/-/format/png)

now() { perl -MTime::HiRes=time -e 'printf "%.3f\n", time'; }
up() {
	cache=$(mktemp -d "$work/cache-vc-XXXX")
	"$bin" --cache-dir "$cache" mount "$mnt" >"$cache/mount.log" 2>&1 &
	pid=$!
	for _ in $(seq 100); do
		"$bin" --cache-dir "$cache" status "$mnt" >/dev/null 2>&1 && break
		sleep 0.2
	done
	o=$mnt/files/$(ls "$mnt/files" | grep -m1 "_$other\$")
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
variant() { echo "$mnt/files/$uuid/$1/hero.png"; }

for _ in $(seq "$rounds"); do
	for i in 0 1 2; do
		up
		echo "solo     ${ops[$i]} $(timed cat "$(variant "${ops[$i]}")")s"
		down
	done
	up
	out=$(mktemp -d "$work/vc-XXXX")
	readers=()
	for i in 0 1 2; do
		(timed cat "$(variant "${ops[$i]}")" >"$out/$i") &
		readers+=($!)
	done
	sleep 0.3
	echo "during   stat $other $(timed stat "$o")s"
	echo "during   ls -l files/  $(timed ls -l "$mnt/files")s"
	wait "${readers[@]}"
	for i in 0 1 2; do
		echo "together ${ops[$i]} $(cat "$out/$i")s"
	done
	down
done

if [ -n "$cold" ]; then
	up
	out=$(mktemp -d "$work/vc-XXXX")
	(timed cat "$(variant "$cold")" >"$out/cold") &
	reader=$!
	sleep 0.5
	echo "while rendering $cold:"
	echo "during   ${ops[0]} $(timed cat "$(variant "${ops[0]}")")s"
	echo "during   stat $other $(timed stat "$o")s"
	wait "$reader"
	echo "cold     $cold $(cat "$out/cold")s"
	down
fi
