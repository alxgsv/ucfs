#!/usr/bin/env bash
# Spike B: does a slow reader stall a second reader on the same NFS mount?
# Usage: concurrency.sh <export-dir> <mount-dir> <concurrent-handlers>
# The export must have been populated with `nfsstatic -populate`.
set -euo pipefail

export_dir=$1 mnt=$2 ch=$3
bin=$(dirname "$0")/../../bin/nfsstatic
log=$(mktemp -t nfsstatic)

"$bin" -root "$export_dir" -concurrent "$ch" >"$log" 2>&1 &
srv=$!
cleanup() {
	umount "$mnt" 2>/dev/null || umount -f "$mnt" 2>/dev/null || true
	kill "$srv" 2>/dev/null || true
	wait "$srv" 2>/dev/null || true
}
trap cleanup EXIT

for _ in $(seq 50); do
	port=$(sed -n 's/^listening on 127.0.0.1:\([0-9]*\).*/\1/p' "$log")
	[ -n "$port" ] && break
	sleep 0.1
done
mount_nfs -o "port=$port,mountport=$port,nfsvers=3,tcp,locallocks,noresvport,rsize=1048576,wsize=1048576,nfc,soft,intr" \
	127.0.0.1:/ "$mnt"

now() { perl -MTime::HiRes=time -e 'printf "%.3f\n", time'; }

t0=$(now)
cat "$mnt/slow.bin" >/dev/null &
slow=$!
sleep 1
t1=$(now)
stat "$mnt/photos/photo-0.jpg" >/dev/null
t2=$(now)
cat "$mnt/fast.bin" >/dev/null
t3=$(now)
wait "$slow"
t4=$(now)

awk -v ch="$ch" -v a="$t0" -v b="$t1" -v c="$t2" -v d="$t3" -v e="$t4" 'BEGIN {
	printf "ConcurrentHandlers=%s  stat %.3fs  fast.bin %.2fs  slow.bin %.2fs\n", ch, c-b, d-c, e-a
}'
