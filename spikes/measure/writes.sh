#!/usr/bin/env bash
# Phase 3 measurements against a running writable mount.
#
#   writes.sh latency <mnt> <file> <tree-dir> <n>
#       cp <file> into tree/<tree-dir>/ n times under new names and print how
#       long each cp took and how long after it returned the file had a uuid.
#   writes.sh copy|finder <mnt> <src-dir>
#       cp -R <src-dir> into tree/, or have Finder duplicate it there, wait
#       until every file has a uuid, and print the wall clock and the REST,
#       Upload API and part requests, uploads and shadow writes it cost.
#
# UCFS_BIN chooses the binary; pass --cache-dir and --project in UCFS_FLAGS
# when the mount uses them.
set -euo pipefail

bin=${UCFS_BIN:-$(cd "$(dirname "$0")/../.." && pwd)/bin/ucfs}
read -r -a flags <<<"${UCFS_FLAGS:-}"

now() { perl -MTime::HiRes=time -e 'printf "%.3f\n", time'; }
since() { echo "$(now) - $1" | bc; }

# counters prints REST, Upload API and part requests, uploads, upload
# bytes, shadow creates, shadow writes and spooled files of the mount.
# An empty file stays spooled until written, so copy waits for the number
# of uploads instead of an empty spool.
counters() {
	"$bin" "${flags[@]}" status --json "$1" | python3 -c '
import json, sys
s = json.load(sys.stdin)[0]
a, w = s["api"], s["writes"]
print(a["rest"], a["upload"], a["parts"], w["uploads"], w["upload_bytes"], w["shadow_creates"], w["shadow_writes"],
      w["writing"] + w["queued"] + w["uploading"])'
}

uuid_of() { "$bin" "${flags[@]}" resolve --json --wait "${2:-120s}" "$1" | python3 -c 'import json,sys; r=json.load(sys.stdin); print(r.get("uuid") or r["state"])'; }

case $1 in
latency)
	mnt=$2 file=$3 dir=$4 n=$5
	mkdir -p "$mnt/tree/$dir"
	ext=${file##*.}
	for i in $(seq 1 "$n"); do
		dst="$mnt/tree/$dir/latency-$i.$ext"
		t0=$(now)
		cp "$file" "$dst"
		t1=$(now)
		uuid=$(uuid_of "$dst")
		printf 'cp %6.3fs  uuid after %6.3fs  %s\n' "$(echo "$t1 - $t0" | bc)" "$(since "$t1")" "$uuid"
	done
	;;
copy | finder)
	mnt=$2 src=$3
	read -r r0 u0 p0 n0 b0 c0 w0 _ < <(counters "$mnt")
	t0=$(now)
	if [ "$1" = finder ]; then
		osascript -e 'with timeout of 900 seconds' \
			-e "tell application \"Finder\" to duplicate (POSIX file \"$src\" as alias) to (POSIX file \"$mnt/tree\" as alias)" \
			-e 'end timeout' >/dev/null
	else
		cp -R "$src" "$mnt/tree/"
	fi
	tcp=$(since "$t0")
	want=$(find "$src" -type f ! -name '.*' | wc -l)
	while :; do
		read -r r1 u1 p1 n1 b1 c1 w1 _ < <(counters "$mnt")
		[ $((n1 - n0)) -ge "$want" ] && break
		sleep 0.2
	done
	printf '%s %.3fs, all uploaded after %.3fs\n' "$1" "$tcp" "$(since "$t0")"
	printf 'uploads %d (%d B), REST %d, Upload API %d, parts %d, shadow files %d, shadow writes %d\n' \
		$((n1 - n0)) $((b1 - b0)) $((r1 - r0)) $((u1 - u0)) $((p1 - p0)) $((c1 - c0)) $((w1 - w0))
	;;
*)
	echo "usage: $0 latency|copy|finder ..." >&2
	exit 2
	;;
esac
