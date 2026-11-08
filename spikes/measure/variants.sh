#!/usr/bin/env bash
# Phase 2 measurement: the variants view on a fresh mount and an empty cache.
# Prints the CDN requests (HEAD, GET) and bytes each step cost, compares a
# copied variant with curl of the same CDN URL, and checks the errors of an
# unknown operation and a bad argument.
# Usage: variants.sh <mount-dir> <work-dir> <uuid> <name> <cdn-base>
set -euo pipefail

mnt=$1 work=$2 uuid=$3 name=$4 cdn=$5
bin=${UCFS_BIN:-$(cd "$(dirname "$0")/../.." && pwd)/bin/ucfs}
stem=${name%.*}

now() { perl -MTime::HiRes=time -e 'printf "%.3f\n", time'; }
counters() {
	"$bin" --cache-dir "$cache" status --json | python3 -c '
import json, sys
s = json.load(sys.stdin)[0]["cdn"]
print(s["heads"], s["gets"], s["bytes"])'
}
md5of() { if command -v md5sum >/dev/null; then md5sum "$1" | cut -d" " -f1; else md5 -q "$1"; fi; }

# step runs a command and prints what it cost; the command's own output and
# errors are shown indented.
step() {
	local label=$1
	shift
	read -r h0 g0 b0 < <(counters)
	local t0 t1 rc=0
	t0=$(now)
	"$@" >"$work/out" 2>&1 || rc=$?
	t1=$(now)
	sleep 1 # let prefetch settle before reading the counters
	read -r h1 g1 b1 < <(counters)
	printf '%-34s %6.3fs  exit %d  %d HEAD %d GET %9d B\n' "$label" "$(echo "$t1 - $t0" | bc)" "$rc" \
		$((h1 - h0)) $((g1 - g0)) $((b1 - b0))
	sed 's/^/    /' "$work/out" | head -5
}

cache=$(mktemp -d "$work/cache-var-XXXX")
"$bin" --cache-dir "$cache" mount "$mnt" >"$cache/mount.log" 2>&1 &
pid=$!
for _ in $(seq 100); do
	"$bin" --cache-dir "$cache" status >/dev/null 2>&1 && break
	sleep 0.2
done

f=$mnt/files/$uuid
webp=-/resize/1200x/-/format/webp
step "ls files/<uuid>/" ls "$f"
step "ls files/<uuid>/-/" ls "$f/-"
step "stat never-seen variant" stat "$f/$webp/$stem.webp"
step "stat it again" stat "$f/$webp/$stem.webp"
step "ls its directory" ls -l "$f/$webp"
step "ls never-seen -/resize/800x/" ls -l "$f/-/resize/800x"
step "cp the 1200 px WebP" cp "$f/$webp/$stem.webp" "$work/$stem.webp"
step "cp it again" cp "$f/$webp/$stem.webp" "$work/$stem-again.webp"
curl -s -o "$work/$stem-curl.webp" "$cdn/$uuid/$webp/"
echo "md5 cp   $(md5of "$work/$stem.webp")  $(wc -c <"$work/$stem.webp") B"
echo "md5 curl $(md5of "$work/$stem-curl.webp")  $(wc -c <"$work/$stem-curl.webp") B"
echo "original $(stat -c %s "$f/$name" 2>/dev/null || stat -f %z "$f/$name") B"
step "stat unknown operation" stat "$f/-/nosuchop/$name"
step "stat bad argument" stat "$f/-/resize/abc/$name"
step "cat bad argument" cat "$f/-/resize/abc/$name"
step "ls bad argument directory" ls "$f/-/resize/abc"
step "stat bad argument again" stat "$f/-/resize/abc/$name"
grep -i "rejected" "$cache/mount.log" | head -2 | sed 's/^/log: /'

"$bin" --cache-dir "$cache" umount "$mnt"
wait "$pid"
