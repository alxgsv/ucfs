# What costs what

A mount spends three things: CDN traffic (bytes of file content, what Uploadcare bills as traffic), CDN requests,
and REST API requests (rate-limited). Uploads go to the Upload API. `ucfs status <dir>` counts all of them since the
mount started: `cdn.bytes`, `cdn.requests` (`heads`, `gets`), `api.rest`, `api.upload`, and the cache's `hit`
bytes, which cost nothing. Numbers below were measured in the phase reports ([1](phase1.md), [2](phase2.md),
[3](phase3.md), [4](phase4.md)) unless marked otherwise.

## Free

Everything that does not read file content is answered from the local index, `<cache dir>/index.db`:

- `ls`, `ls -l`, `ls -lR`, `stat`, `find` without `-exec`, `du`, tab completion, in every view except a variant's
  directory. `ls -lR` of the whole mount cost 0 bytes and 0 requests on macOS and Linux; so did `ls -lR by-date/`.
- Walking `files/<uuid>/` and `-/`: nothing is listed there, and an unknown operation name is refused without a request.
- `chmod`, `chown`, `touch` (accepted and ignored), `mkdir` and `rmdir` in `tree/`, and `listxattr` on Linux.
- Reading what is already in the block cache: `<cache dir>/blocks/`, up to `--cache-size` (2 GiB), least recently
  used out first. It survives remounts with the same cache dir, and originals are never fetched again: a second
  `rsync` of the same files cost 0 bytes.
- Reading back a file the mount uploaded: its bytes go into the block cache at upload.

## Reading files

Content comes from the CDN in 1 MiB blocks:

- The first read of a file fetches the block it touches: `head -c 4096` costs 1 MiB.
- A reader that keeps going sequentially gets the next blocks prefetched, 1 MiB ahead at first and doubling per block
  up to 16 MiB ahead; prefetch stops at the end of the file. Reading a whole file costs its size: `cat` of a 16.3 MB
  file, a Preview open and an `rsync` out each cost exactly the bytes read. A reader that stops early can leave up to
  16 MiB fetched and unread (from the code, not measured).
- macOS NFS read-ahead is off (`--readahead 0`): at the client's default of 16 blocks, `head -c 4096` and `file(1)`
  cost 16 MiB each. On Linux, `file` itself read up to 7 MiB of a file.
- Anything that reads content pays for it: `cp`, `rsync`, `grep -r`, checksums, editors and indexers.

## Variants

`files/<uuid>/-/<operation>/…/<name>` reads a CDN transformation, which costs its own size, usually far less than the
original: `cp` of a 1200 px WebP cost 153,608 bytes against 9,785,662 for the JPEG it came from.

- The first `stat` or `ls -l` of a variant sends one `HEAD` (no body) to learn its size; the answer is kept for
  24 hours, across remounts, then checked with another `HEAD`. A second `stat` cost nothing. Listing an operation
  directory costs the same `HEAD`.
- The CDN renders a variant on first request, which can take long: 21.3 s for a 3000 px PNG of a 24 Mpx image,
  about 1 s once rendered.
- A bad argument costs one `HEAD` and one `GET` of the CDN's error page (about 740 bytes), then nothing for an hour.
- On Linux, `user.uc.md5` costs one `HEAD` the first time it is read for a file the mount did not upload.

## Finder and Quick Look

Finder reads every image it shows a thumbnail for, in full: opening a folder of 15 images read all 15, 23,288,128
bytes. Quick Look keeps thumbnails across mounts, so a later visit read only the images not thumbnailed yet; a file
in the block cache costs nothing again. `.ql_disablethumbnails` does not stop it. Spotlight does not index the
mount. [finder.md](finder.md) has the details.

## REST API requests

A mount makes REST requests in the background:

| When | Requests |
|---|---|
| first mount of a cache dir | one per 1,000 files, to list the project |
| every later mount | one listing from where the last one stopped |
| every `--sync-interval` (30 s) | one or more: new uploads since the last listing (about 120 an hour when idle, from the code) |
| every `--sweep-interval` (10 min) | one or more: removed files, by pages of 1,000; plus one for new groups when `groups` is mounted |

and on demand:

| Operation | Requests |
|---|---|
| a CDN 404 on read | one file info request; only the REST API decides a file is gone |
| `getxattr` (Linux) or `ucfs resolve` of a file whose info is older than `--meta-ttl` (5 min) | one file info request, shared by concurrent callers; 0.21 s measured, 7–8 ms within the TTL |
| first listing of a group in `groups/` | one, then kept |
| new file written | none; one Upload API request, or for files over 10 MiB a start, one per 5 MiB part, and a complete |
| overwrite of an uploaded file | one file info request (its metadata and tags) and one delete of the old file, plus the upload |
| `mv` of an uploaded file in `tree/` | one metadata write; a directory: one per file below it |
| `rm` | one delete |
| `setfattr`, `setfattr -x` (Linux) | one metadata or tags write |

A Finder copy of 50 images into `tree/` made 50 Upload API requests and no REST request.

## Traffic budget

Measured in [phase5.md](phase5.md#traffic-budget).

`--traffic-budget 2GiB` caps the CDN bytes a mount reads per calendar day in the local time zone:

- Only range reads for the block cache count, whether a reader waited for them or they were prefetched. `HEAD`s,
  the CDN's error pages, cache hits and uploads do not.
- The day's count is kept in `index.db`, so a remount continues it, and mounts using the same cache dir share it.
- Prefetch stops while less than a tenth of the budget, at most 16 MiB, is left, which keeps the last bytes for reads
  someone waits for.
- Past the budget, a read that needs the CDN fails with `EDQUOT` until local midnight; cached content still reads.
  Over NFS the server answers `NFS3ERR_DQUOT`; what a macOS application then sees was not measured here. On Linux
  FUSE, a file opened while it is larger than what is left is read with direct I/O so the reader sees `EDQUOT`;
  other buffered reads that run out see `EIO`.
- `ucfs status` shows `budget X of Y used today, Z left; resets …`, and `--json` the `budget` object.

`--max-inflight` (default 32) bounds CDN requests in flight; reads someone waits for and `HEAD`s go before
prefetches, which hold at most three quarters of the slots, so a bulk `rsync` does not starve an interactive `ls -l`
of a variant or a `cat`.
