# Phase 1 acceptance: read-only mount

Measured on 2026-11-08 against `uploadcare-go-test`:

- **macOS:** Apple Silicon Mac, macOS 27.0.1, NFS front-end, nothing installed.
- **Linux:** x86_64 host, Ubuntu 26.04.1 with kernel 7.0, FUSE front-end, mounted as a normal user through `fusermount3`.

The project's 33 fixtures are tiny, so six larger files were uploaded for the read measurements: 34 MB in total, tagged
`ucfs:test=1`, and deleted after acceptance. They are `ucfs-seq16.jpg` (16.3 MB), `ucfs-seq12.jpg` (12.4 MB),
`ucfs-photo-large.jpg` (4.7 MB) and `ucfs-photo-{a,b,c}.jpg` (0.8 MB each). They are noise JPEGs, because the project
rejects other binary types at upload.

Every cold run uses a fresh mount and an empty `--cache-dir`, which means a new index and an empty block cache. The
scripts are [`spikes/measure/readahead.sh`](../spikes/measure/readahead.sh) and
[`spikes/measure/concurrent.sh`](../spikes/measure/concurrent.sh). CDN bytes come from `ucfs status`, which counts
response body bytes actually read.

## Acceptance

| Criterion | macOS, NFS | Linux, FUSE |
|---|---|---|
| Mount without installing anything | `mount_nfs` as the user: `(nfs, nodev, nosuid, read-only, nobrowse, mounted by alxgsv)` | `fuse.ucfs (ro,nosuid,nodev,noatime,user_id=1000,…,max_read=1048576)` via `fusermount3` |
| `ls`, `ls -l` | 39 entries, `<uuid>_<original_filename>` | same |
| `cat` | bytes equal to `curl` of the CDN URL | same |
| Open an image in Preview | `open -a Preview` of the 4.7 MB JPEG; `lsof` shows it open; CDN bytes 4,659,624, exactly its size | — |
| `rsync` a subset out | 15 files, 0 differ from `curl`; CDN 2,446,690 B, exactly the transferred size; a second rsync costs 0 B | 15 files, 0 differ; CDN 2,446,690 B |
| `ls -lR` costs zero CDN bytes | `ls -lR` plus `stat` of every file: 0 requests, 0 B | 0 B |
| `ucfs status` CDN bytes vs bytes read | 7,106,314 B after Preview and rsync, exactly the sum of the two | 24,463,431 B for the session, exactly the sum of what was read |
| Writes | `rdonly` mount | `touch`: Read-only file system |
| Two concurrent readers, no mutual stall | see below | see below |

Other mount lifecycle checks on macOS:

- `ucfs umount <dir>` unmounts through the control socket, and the daemon exits.
- An external `umount` is noticed within 2 s, and the daemon exits.
- SIGINT unmounts first, then exits.
- Mounting over an active mount fails with `resource busy`.
- No mount, daemon or socket was left behind (`mount | grep ucfs` was empty after every run, on both hosts).

## Read-ahead: `readahead=0` versus 2 and 4 (macOS)

These are cold reads with the daemon's concurrent prefetch. Each cell shows two runs of the final code, with the CDN
edge already warm.

| `readahead` | cold `cat` 16.3 MB | `head -c 4096` | `file(1)` |
|---|---|---|---|
| 0 | 1.53 s, 1.44 s; 16,308,541 B | 0.12 s; 1 MiB | 0.13 s; 1 MiB |
| 2 | 1.39 s, 1.54 s; 16,308,541 B | 0.12–0.22 s; 2 MiB | 0.14–0.28 s; 2–3 MiB |
| 4 (earlier build) | 1.46 s; 16,308,541 B | 0.31 s; 8 MiB | 0.24 s; 4.7 MiB |

- `cat` never fetched a byte beyond the file size. Prefetch stops at EOF.
- Sequential speed is the same at 0 and 2. The client's read-ahead buys nothing once the daemon prefetches, and costs 1–2 MiB extra per small read. So **`readahead=0` stays the default**; `--readahead` overrides it.
- The very first run against freshly uploaded files took 3.90 s for the same `cat`. A rerun minutes later took 1.78 s, so the difference was the CDN edge fetching from origin on first touch, not read-ahead.

## Two concurrent readers

Cold reads of the 16.3 MB file (A) and the 12.4 MB file (B), alone and started together. `stat` and `ls -l` of
`files/` ran while both were reading.

| Host | Solo A | Solo B | Together A | Together B | `stat` during | `ls -l` during |
|---|---|---|---|---|---|---|
| macOS, round 1 | 1.41 s | 1.54 s | 1.81 s | 1.51 s | 20 ms | 11 ms |
| macOS, round 2 | 1.38 s | 1.16 s | 2.01 s | 1.61 s | 21 ms | 11 ms |
| Linux, round 1 | 1.24 s | 1.65 s | 2.54 s | 2.68 s | 3 ms | 4 ms |
| Linux, round 2 | 1.99 s | 1.14 s | 1.17 s | 1.14 s | 3 ms | 4 ms |
| Linux, round 3 | 1.13 s | 1.01 s | 1.33 s | 1.49 s | 3 ms | 4 ms |

- Metadata never waits for reads.
- Two readers together finish in about the time of the slower solo read plus bandwidth sharing. On macOS, combined throughput rose from about 11 MB/s solo to about 14 MB/s.
- Linux round 1 is slower in both columns, and the round-to-round spread on that host is network variance; rounds 2–3 show no slowdown.

## Finder icon view traffic (macOS)

`open <mnt>/files` on a fresh mount, 45 s:

- **First visit:** 15 files thumbnailed, **each read in full**, 23,288,128 B in total. Files outside the visible window were not read.
- **Second visit**, on another mount path with an empty block cache: only the 3 files not thumbnailed before were read, each in full. Quick Look kept the other thumbnails across mounts and mount paths.

So a Finder visit costs the full size of every visible image not yet in Quick Look's cache, and nothing for the rest.

## Linux `file(1)`

`file` 5.46 reads up to 7 MiB by itself: it was served all 4,659,624 bytes of the 4.7 MB JPEG, which is why the CDN
bytes equal the file size there. `file -P bytes=65536` was served 128 KiB and fetched one 1 MiB block.

## Found while measuring

- **go-nfs handle alignment.** go-nfs reads file handles with `xdr.ReadOpaque`, which does not skip XDR padding. A handle whose length is not a multiple of 4 shifts the arguments that follow it. With 26-byte handles the ACCESS mask read as 0, and every directory answered *Permission denied*. ucfs handles are now 28 bytes, and the NFS test asserts the ACCESS result.
- **Prefetch window growth.** The Linux kernel splits reads into 128 KiB FUSE requests. Growing the window on every read call would prefetch 16 MiB for a 1 MiB reader, so the window now grows per block the reader moves into.
- **Control socket path.** A long `--cache-dir` exceeds the 104-byte `sun_path` on macOS. The socket then moves to `$TMPDIR` under a name derived from the cache dir and the mount point.

## Not covered

- **NodeID collision detection (PLAN §4.3)** is not implemented. NodeIDs are plain 64-bit FNV-1a hashes; with 1 M files the collision chance is about 3·10⁻⁸. Deferred to Phase 5.
- **The NFS front-end on Linux** builds but was not mounted, because that needs root.
- **`go test -race`** ran on macOS for `vfs`, `cache`, `frontend/nfs`, `ctl` and `index`. The FUSE package's own test ran on the Linux host without `-race`: race builds need cgo, and the host has no Go toolchain.
