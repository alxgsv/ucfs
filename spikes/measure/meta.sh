#!/usr/bin/env bash
# Phase 4 measurements against a running mount.
#
#   meta.sh cost <mnt> <command...>
#       run a command and print its wall clock and what it cost the mount:
#       REST requests, CDN requests (HEADs) and bytes, info refreshes and
#       xattr calls.
#   meta.sh watch <mnt> <file> <xattr> <value> [<interval>]
#       read one attribute every interval (2 s) until it equals value, and
#       print each read with the time since the start.
#
# UCFS_BIN chooses the binary; pass --cache-dir and --project in UCFS_FLAGS
# when the mount uses them. getfattr comes from PATH.
set -euo pipefail

bin=${UCFS_BIN:-$(cd "$(dirname "$0")/../.." && pwd)/bin/ucfs}
read -r -a flags <<<"${UCFS_FLAGS:-}"

now() { python3 -c 'import time; print(f"{time.time():.3f}")'; }

counters() {
	"$bin" "${flags[@]}" status --json "$1" | python3 -c '
import json, sys
s = json.load(sys.stdin)[0]
c, m = s["cdn"], s["meta"]
print(s["api"]["rest"], c["requests"], c["heads"], c["bytes"], m["refreshes"], m["xattr_lists"], m["xattr_gets"],
      m["xattr_sets"], m["xattr_removes"], m["md5_heads"], m["group_fetches"])'
}

case $1 in
cost)
	mnt=$2
	shift 2
	read -r -a c0 < <(counters "$mnt")
	t0=$(now)
	"$@"
	t1=$(now)
	read -r -a c1 < <(counters "$mnt")
	d=()
	for i in "${!c0[@]}"; do d[i]=$((c1[i] - c0[i])); done
	printf '%.3fs  REST %d  CDN %d (%d HEAD, %d B)  refreshes %d  xattr list %d get %d set %d remove %d  md5 HEADs %d  group reads %d\n' \
		"$(python3 -c "print($t1 - $t0)")" "${d[@]}"
	;;
watch)
	mnt=$2 file=$3 name=$4 want=$5 every=${6:-2}
	t0=$(now)
	while :; do
		got=$(getfattr --only-values -n "$name" "$file" 2>/dev/null || echo '<none>')
		printf '%6.1fs  %s=%s\n' "$(python3 -c "print($(now) - $t0)")" "$name" "$got"
		[ "$got" = "$want" ] && break
		sleep "$every"
	done
	;;
*)
	echo "usage: meta.sh cost|watch ..." >&2
	exit 2
	;;
esac
