# Phase 0 spikes

Results of the de-risking spikes from [PLAN.md](../PLAN.md) Phase 0, run on 2026-11-08 on an Apple Silicon Mac with
macOS 27.0.1 (26A434), no FUSE installed, Go 1.26.1. Spikes A–C use the static export rig in
[`spikes/nfsstatic`](../spikes/nfsstatic), which serves a local directory over loopback NFSv3 with go-nfs and logs
every filesystem call. Spike D is [`spikes/uploadnames`](../spikes/uploadnames). Spike E is `ucfs index sync`.

go-nfs is pinned to master commit `c092ae79c64e` (`v0.0.5-0.20260920152406-c092ae79c64e`, 2026-09-20), the merge that
added `Server.ConcurrentHandlers`. The last tag, v0.0.4, does not have it.

## A. Mounting as a normal user

**Question.** Can `mount_nfs` mount a localhost go-nfs export without root, and which option string works?

**Method.** Export on `127.0.0.1:<random port>`, mount point owned by the user, `mount_nfs` run as the user with no
`sudo`. Effective options read back with `nfsstat -m`. READ sizes counted in the server log.

**Result.** Works without root. `mount` reports `(nfs, nodev, nosuid, mounted by alxgsv)`: the kernel forces `nodev`
and `nosuid` on user mounts. `umount` as the user works too. The option string that works:

```
mount_nfs -o port=P,mountport=P,vers=3,tcp,locallocks,noresvport,rsize=1048576,wsize=1048576,readahead=0,nfc,soft,intr,acregmax=3600,acdirmax=5[,rdonly][,nobrowse] 127.0.0.1:/ <dir>
```

- `port` and `mountport` must both be given; go-nfs runs no portmapper and serves MOUNT and NFS on one port.
- Use `127.0.0.1`, not `localhost`, which may resolve to `::1`. Listen on `127.0.0.1` only.
- `noresvport`: a mount without it also succeeded as a normal user, but the man page says `resvport` needs root, so it
  stays explicit.
- Without `rsize`/`wsize` the client negotiates only 32 KiB even though go-nfs advertises 1 GiB in FSINFO. With them,
  `nfsstat -m` shows 1 MiB, and `cat` of a 16 MiB file is exactly 16 READs of 1 MiB.
- `nfc`, `acregmax=3600`, `acdirmax=5`, `rdonly` and `nobrowse` are all accepted and show up in `nfsstat -m`.
- Read-ahead matters for traffic. At `rsize=1048576` the default read-ahead of 16 blocks turns `head -c 4096` into
  16 MiB of READs, and `file(1)` into 16 MiB as well. With `readahead=0`, `head -c 4096` is one 1 MiB READ and `file(1)`
  two. A 4 KiB `dd` at a 4 MB offset was one READ either way.
- Read-ahead also carries throughput. With `readahead=0` the client issues one READ at a time per reader:
  `cat slow.bin` (16 READs of 400 ms each) took 6.49 s, against 1.08 s with the default read-ahead (B).

**Consequence.** No `sudo` setup step in V1. PLAN §4.6 now has this option string. `readahead=0` is a traffic choice:
cold sequential speed then depends entirely on the daemon's own prefetch, which must fetch blocks ahead concurrently.
Phase 1 re-measures it against a small read-ahead (2–4). Read-only mounts add `rdonly` (see C). The macOS NFS
front-end can be mounted in CI on a macOS runner as the user.

## B. Concurrency: `ConcurrentHandlers` 1 versus 128

**Question.** Does one slow request stall every other process on the mount, and does `ConcurrentHandlers` fix it?

**Method.** [`concurrency.sh`](../spikes/nfsstatic/concurrency.sh): `slow.bin` (16 MiB, every `ReadAt` sleeps
400 ms) and `fast.bin` (16 MiB, no delay). Start `cat slow.bin`, after 1 s run `stat photos/photo-0.jpg` and then
`cat fast.bin` in a second process. Fresh server and mount for every run, so no client cache carries over.

**Result.** Two runs at each setting, within 5 ms of each other:

| ConcurrentHandlers | `stat` of an unrelated file | `cat fast.bin` (16 MiB) | `cat slow.bin` |
|---|---|---|---|
| 1 | 5.46 s | 0.03 s (after the stall) | 6.52 s |
| 128 | 0.024 s | 0.02 s | 1.08 s |

With 1 handler, the `stat` waits until all queued 400 ms reads of `slow.bin` have drained. With 128, it is not
affected. The slow reader itself also speeds up 6×, because the client's read-ahead READs run in parallel.

**Consequence.** `ConcurrentHandlers: 128` from the first mount, as planned. The VFS behind the NFS adapter must be
safe for concurrent use. Run `go test -race` on it ad hoc, since `make test` is `CGO_ENABLED=0`.

## C. Finder and Quick Look

**Question.** What does Finder read and write when it opens the mount?

**Method.** Writable export with go-nfs trace logging (every RPC) and the billy request log, mounted without
`nobrowse`. `open <mnt>` and `open <mnt>/photos`, 15–25 s each, with JPEG and PNG fixtures. Then `cp` of a file into
the mount, a check of `.ql_disablethumbnails`, and a read-only mount with a 16 MB JPEG.

**Result.**

- Spotlight: `mdutil -s` reports *Indexing disabled* on the NFS mount by default.
- Opening the root: Finder looks up `.metadata_never_index`, `.metadata_never_index_unless_rootfs`,
  `.metadata_direct_scope_only`, `.ql_disablethumbnails`, `.ql_disablecache`, `.Spotlight-V100`, `.hidden`, `DCIM` and
  `._.`. It creates and writes `.DS_Store` (6 KiB) and `._.DS_Store` (4 KiB AppleDouble) at the root. That is 15 LOOKUP,
  15 GETATTR, 10 FSSTAT, 10 ACCESS, 3 CREATE, 3 WRITE and 1 READDIRPLUS.
- Opening `photos/` in icon view: **every image is read in full** for thumbnails, 7 of 7. On a writable mount each is
  then followed by a SETATTR of its times.
- The same holds for large images, independent of client read-ahead. A fresh 16 MB JPEG on a `rdonly,readahead=0`
  mount was read to the last byte (16,310,483 bytes in sequential 256 KiB and 1 MiB READs). The fixtures are
  Go-encoded JPEGs with no embedded EXIF thumbnail, so this is the worst case. Camera JPEGs with an embedded thumbnail
  were not tested.
- A `.ql_disablethumbnails` file at the root does not stop it. Six fresh images were still read in full.
- `rdonly` stops all writes: no `.DS_Store`, no `._.DS_Store`, no SETATTR. Reads are unchanged.
- `cp` of any file creates an AppleDouble sidecar `._<name>`, even when the source has no user xattrs, because macOS 27
  attaches `com.apple.provenance` to files. The sequence for one `cp`:
  1. create the data file, then create `._<name>` and write 4 KiB to it
  2. chmod both to 0600
  3. write the data, then truncate to the length just written
  4. chmod both back
  5. remove `._<name>`, recreate it and rewrite it
  6. chtimes on the data file
  The data file is written exactly once.
- go-nfs calls `Lstat` on almost every RPC (679 calls in this session) and re-opens the file for every READ and WRITE.

**Consequence.**

- Mount with `rdonly` while `--read-only` is in effect (Phase 1).
- Use `nobrowse` by default so the mount does not appear in the Finder sidebar unasked.
- Finder icon view of a directory costs up to the full size of every image in it. No marker file and no mount option
  prevents that, so the traffic counter (Phase 1) and the traffic budget (Phase 5) are the controls. Document it in
  the cost guidance.
- Phase 3 must absorb `._*` and `.DS_Store` locally, as planned. It must also accept a truncate to the current length
  as a no-op, because `cp` issues one after every write.
- `Lstat`/`Open` in the NFS adapter must be index-only and cheap.

## D. Upload semantics against `uploadcare-go-test`

**Question.** Does direct upload preserve `original_filename` byte for byte? What happens with an empty file? Does
`UPLOADCARE_STORE=1` behave when `autostore_enabled` is true?

**Method.** Base upload (`POST /base/`) through uploadcare-go, `ToStore` `1` unless noted, metadata `ucfs:test=1`.
Each file was polled through `GET /files/{uuid}/` until `is_ready`, and `original_filename` compared as bytes. Three
cases went through multipart upload (11 MiB each). Every uuid was written to a ledger as it was created, then deleted and
verified as removed. Before and after, the project lists the same 33 files and none carries `ucfs:test`.

**Result.** The project has `autostore_enabled=true`.

| Name sent | `original_filename` kept |
|---|---|
| `plain.txt`, `with  two spaces .txt` | yes |
| `café.txt` NFC (`c3 a9`) and NFD (`65 cc 81`) | yes, both forms kept distinct |
| `привет мир.txt`, `日本語.txt`, `emoji 🙂.txt` | yes |
| `say "hi".txt`, `a;b=c.txt`, `.hidden`, 230-byte name | yes |
| `back\slash.txt` (base) | **no**: `slash.txt` |
| `dir/sub.txt` (base) | **no**: `sub.txt` |
| `multi café 🙂 "q".bin` NFD (multipart) | yes |
| `multi back\slash.bin` (multipart) | yes |
| `multi dir/sub.bin` (multipart) | yes, `/` included |

- Base upload keeps only what follows the last `\` or `/`. This is IE-style path stripping of the `Content-Disposition`
  filename on the server. Multipart sends the name as a plain form field and keeps it, `/` included.
- The Upload API's own `filename` field is a sanitised form: `[A-Za-z0-9._]` only, and `noroot.txt` for an all
  non-ASCII name. Never use it.
- Empty file: accepted, `is_ready`, stored, size 0.
- Store: `1` gives stored; `0` gives **not stored** even with autostore on; `auto` gives stored, following the project.
  `datetime_stored` was set by the time `is_ready` was true.
- Metadata sent with the upload was present on every file.

**Consequence.**

- The `original_filename` risk is retired for everything except `\`. Phase 3 always sends `UPLOADCARE_STORE=1`, as
  planned.
- A written name containing `\` must not go through base upload as-is. Phase 3 rejects such names with `EINVAL` until
  it implements the `ucfs:name` fallback from the risk table.
- Names are stored in whatever normalisation the client sent. Measured on the rig with an NFD-named file
  (`cafe` + U+0301):
  - With `nfc`, `ls` shows the NFD bytes, but opening that very name sends NFC (`c3 a9`) in LOOKUP. Opening by the
    NFC name also sends NFC.
  - Without `nfc`, the client sends the bytes it was given.
  - The open succeeded in all four cases only because the export sits on APFS, which matches either form.

  ucfs keeps `nfc`, so new uploads from macOS get NFC names, and Phase 1 lookups must match names under NFC.
  Otherwise NFD names uploaded by other clients would be listed but fail to open.
- Phase 1 must map `/` in server names, because multipart stores it, and NUL defensively.
- Empty files are uploadable, so the zero-length limitation in Phase 3 is an NFS-semantics choice, not an API limit.

## E. Scan cost

**Question.** How long does a full scan take at 1000 files per page, and do we hit 429s?

**Method.** `ucfs index sync --project uploadcare-go-test`, fresh index, then three incremental and two `--full`
runs. Request count, 429s and latency come from the counting transport around uploadcare-go, which sees the
retries the library hides. Only `uploadcare-go-test` was used. No larger project was looked up, and scans were not
looped to provoke throttling.

**Result.**

- The project holds 33 files (2.8 MiB). Every sync was one request: 0.58–0.96 s wall time, 0 throttled.
- `from` is inclusive at second precision. Each incremental run re-listed the 2 files that share the newest second.
  uploadcare-go also truncates `datetime_*` to seconds, which is why the index dedupes by uuid.
- A page costs about 490 bytes of JSON per file in the CLI's compact form, so a full 1000-file page is roughly
  0.5–1.5 MB.

**Consequence.**

- Not representative: rate limits for list-heavy scans remain unmeasured.
- Extrapolating from the fixed ~0.6–0.9 s per request plus about 1 MB of transfer, 1 M files is about 1000 pages and
  roughly 15–30 min sequentially. That fits "index once, poll rarely".
- The implementation already resumes from the committed cursor after an interruption (tested against the fake), and
  retries 429 with `Retry-After` (also tested).
- The risk row stays open until a scan of a large internal project, with the platform team's agreement, or published
  limits.
