# ucfs — Implementation plan (V1)

Companion to [RESEARCH.md](RESEARCH.md), which holds the feasibility analysis, verified API/CDN facts and the
namespace design. This document says what gets built, in which order, and how each phase is accepted.
Effort figures are estimates for one engineer.

## 1. Scope and decisions

**Repository:** `github.com/alxgsv/ucfs`, private while experimental, checked out at `~/workspace/ucfs`. Each phase is a
branch with a pull request to `main`; the phase's acceptance criteria are the PR's checklist.

**V1 delivers** a daemon plus CLI, `ucfs`, that mounts one Uploadcare project as a read-mostly, write-once filesystem on
macOS and Linux, with these views: `files/`, `files/<uuid>/-/…` variants, `tree/` (writable, metadata-backed), `by-date/`,
`tags/`, `groups/`. Metadata is exposed as xattrs where the front-end supports them (Linux FUSE in V1).

**Out of scope for V1:** Windows (WinFsp; nothing in the core should preclude it, but no code or CI for it),
FUSE-T/cgofuse front-end on macOS (V1.1: brings xattrs and close semantics to macOS), FSKit and File Provider, webhook
receiver, rclone backend, signed/authenticated CDN URLs, custom storages, conversion views, group creation,
`local_copy` on `cp` of a variant, `.removed/` view.

Decisions fixed up front:

| Decision | Choice | Why |
|---|---|---|
| Language, toolchain | Go 1.26, `CGO_ENABLED=0` | Matches `uploadcare-go` and `uploadcare-cli`; static binaries; cross-compile for darwin/linux from one machine |
| Repository and module path | `github.com/alxgsv/ucfs`, private for now, checkout `~/workspace/ucfs` | Personal repo while experimental; the module path equals the repo path, so going public needs no rename. A later move to the `uploadcare` org is a module-path change in `go.mod` and imports, nothing structural |
| macOS front-end | Loopback NFSv3 server (`github.com/willscott/go-nfs` at master `c092ae79c64e` for `Server.ConcurrentHandlers`, which v0.0.4 lacks; carried in `third_party/go-nfs` since Phase 2 with a patch that passes errnos through as NFS statuses, since Phase 3 one that passes COMMIT to the filesystem, and since Phase 5 one that lets the handler narrow ACCESS by the caller's uid) mounted with the built-in `mount_nfs` as the user, no `sudo` (Spike A) | Zero install, no kext, works on managed Macs. Proven by rclone `nfsmount` |
| Linux front-end | FUSE via `github.com/hanwen/go-fuse/v2` (pure Go) | No libfuse dependency, full semantics (close, xattrs, invalidation). NFS front-end also builds on Linux as a fallback |
| Index | SQLite via `modernc.org/sqlite` | Pure Go; WAL gives concurrent readers; survives restarts |
| API client | `github.com/uploadcare/uploadcare-go/v2` (`file`, `upload`, `metadata`, `tag`, `group`, `project`) with `RetryConfig` enabled | Already covers list, search, info, store/delete, local copy, base and multipart upload, metadata, tags, groups |
| CLI framework | cobra, same conventions as `uploadcare-cli` | Familiar flags, `--json`, config file sharing |
| Config | `~/.uploadcare/config.yaml` (CLI format), env `UPLOADCARE_PUBLIC_KEY`/`UPLOADCARE_SECRET_KEY`, flags | One credential store for CLI and mount |
| Logging | `log/slog` | Standard library |

## 2. Repository layout

```
cmd/ucfs/                      main; cobra commands: mount, umount, status, resolve, index, version
internal/config/               credentials, project selection, cache and spool directories
internal/api/                  thin layer over uploadcare-go: paged listing, info, search, metadata, tags, upload (base/multipart), delete, local copy; error → errno mapping
internal/cdn/                  per-project CDN base, URL building, HEAD, ranged GET, variant operation grammar
internal/index/                SQLite schema and migrations, full scan, incremental sync, removed sweep, group sync, queries
internal/cache/                on-disk block cache, LRU eviction, singleflight, sequential detector, adaptive prefetch, traffic accounting
internal/spool/                write spool, commit triggers, upload, overwrite policy, aliases, upload journal, crash recovery
internal/vfs/                  platform-neutral filesystem model: node ids, attrs, lookup/readdir/open/read/write/flush, xattrs, local-only shadow files
internal/vfs/views/            files, variants, tree, bydate, tags, groups (planned as one file each; built in internal/vfs as vfs.go, variants.go, tree.go and views.go for by-date, tags and groups, with writes in write.go, shadow files in shadow.go and xattrs in xattr.go)
internal/frontend/nfs/         go-nfs Handler (stable file handles), billy.Filesystem adapter, mount_nfs/umount driver (darwin, linux)
internal/frontend/fuse/        go-fuse adapter (linux build tag)
internal/ctl/                  unix-socket control API used by `ucfs status`
internal/fake/                 httptest doubles for REST, Upload API and CDN used by tests
third_party/go-nfs/            go-nfs at the pinned commit with the errno, COMMIT and exclusive-create patches (README.ucfs.md), wired in by a go.mod replace
testdata/                      golden files, fixtures
docs/                          mount options, Finder notes, costs, semantics, troubleshooting, release, phase reports
spikes/                        measurement tools: photogen, rest, keygrep (keys in logs), benchtable (benchmark tables), measure/*.sh
scripts/install.sh             installer for release archives (Phase 6)
contrib/                       launchd and systemd examples (Phase 6)
Makefile  .github/workflows/ci.yml (Phase 5)  .github/workflows/release.yml  .goreleaser.yaml  CLAUDE.md  AGENTS.md (Phase 6)
```

## 3. Phases

### Phase 0: Skeleton and de-risking spikes (≈ 2 days)

Get a binary, a config, an index, and answers to the open questions that could change the design.

1. `go.mod`, `cmd/ucfs/main.go`, root cobra command with global flags, `ucfs version`, `Makefile` (`build`, `test`, `lint`, `run`).
2. `internal/config/`: read the CLI config file and env; pick project; derive cache dir (`~/Library/Caches/ucfs/<pubkey>` on macOS, `$XDG_CACHE_HOME/ucfs/<pubkey>` on Linux).
3. `internal/api/` list wrapper and `internal/index/` schema; `ucfs index sync` performs a full scan of the configured project into SQLite and prints counts.
4. Spike A, macOS mount: a static go-nfs export mounted with `mount_nfs` as a normal user on macOS 27. Record the exact option string that works (`port`, `mountport`, `nfsvers=3`, `tcp`, `locallocks`, `rsize`/`wsize` 1 MiB, `nfc`, `soft`/`hard`, `noresvport`) and whether `sudo` is needed. If non-root mounting is impossible, V1 ships a one-time `sudo` setup step; the design does not change.
5. Spike B, concurrency: two readers on different files against the static export with `ConcurrentHandlers` 1 and 128; confirm the stall disappears.
6. Spike C, Finder: open the static export in Finder, record what it reads and writes (`.DS_Store`, `._*`, thumbnails) with a request log.
7. Spike D, upload semantics against `uploadcare-go-test`: does direct upload preserve `original_filename` byte-for-byte (spaces, Unicode NFC/NFD, non-ASCII)? What happens with an empty file? Does `UPLOADCARE_STORE=1` behave as expected when `autostore_enabled` is true?
8. Spike E, scan cost: full scan of the largest internal test project available at 1000/page; record duration and any 429s.

Result: `ucfs version`, `ucfs index sync`, and a short `docs/spikes.md` with the answers. Every later phase assumes them.
The answers are in [docs/spikes.md](docs/spikes.md); the items below already reflect them.

### Phase 1: Read-only mount on macOS and Linux (≈ 4 days)

One view end to end through both front-ends.

9. `internal/vfs/`: node model (`NodeID` = 64-bit hash of view + uuid + ops/path), attrs from the index, `Lookup`, `ReadDir` (a full listing from the index; the NFS adapter pages it with go-nfs's verifier cache, so no cursor is needed), `Open`/`ReadAt`/`Release`, per-node attribute TTL hints (long for immutable entries, short for directories). `Lookup` matches names under Unicode NFC: the server keeps NFC and NFD names distinct, and with the `nfc` mount option macOS sends NFC in LOOKUP even for a name it listed as NFD (Spike D).
10. `views/files`: `files/<uuid>_<original_filename>`; `files/<uuid>/` resolvable but not listed (it holds the Phase 2 variants view). Server names can contain `/` (multipart keeps it, Spike D), so the view maps `/`, and NUL defensively, to safe characters.
11. `internal/cdn/`: CDN base from public key (same derivation as the CLI), `HEAD`, ranged `GET`, ETag capture.
12. `internal/cache/`: 1 MiB blocks on disk, size-bounded LRU, singleflight per (uuid, ops, block), sequential detector with prefetch window 1 → 16 MiB, per-mount byte counters. NFS has no open handle (go-nfs re-opens the file for every READ), so the detector is keyed per file and tracks several streams, so two readers of one file do not reset each other's window. Prefetch fetches the blocks ahead concurrently: with `readahead=0` the client sends one READ at a time per reader (Spike A). The window grows per block a reader moves into, not per read call, because Linux splits reads into 128 KiB FUSE requests (docs/phase1.md).
13. `internal/frontend/nfs/`: `nfs.Handler` with stable, self-describing file handles (version, kind, uuid, ops/path hash) instead of the path-LRU helper; billy adapter over `vfs`; `ConcurrentHandlers` 128; mount/unmount driver using the Spike A option string (§4.6). go-nfs calls `Lstat` on almost every RPC and `Open`+`Stat` on every READ, so both must be index-only. `HandleLimit` must be large because READDIR caps a reply at `HandleLimit()/2` entries, and the handler implements go-nfs's verifier cache so a large directory is not re-listed for every READDIR page.
14. `internal/frontend/fuse/` (linux): go-fuse node adapter; `MaxReadAhead` and `MaxWrite` 1 MiB; entry/attr timeouts from vfs hints. Mounts as the user through `fusermount3`; `DirectMount` uses mount(2) where allowed, such as root in a container.
15. `ucfs mount <dir> [--read-only]`, `ucfs umount <dir>`, `ucfs status` (counters over the control socket). Mounts
    were read-only by default through Phase 2; since Phase 3 they are writable, and `--read-only` opts out. On macOS
    it adds `rdonly`, which stops Finder's `.DS_Store` writes and SETATTRs (Spike C).
16. Index sync loop: incremental listing every 30 s from the stored cursor; `removed=true` sweep every 10 min; a CDN 404 refreshes the file's info over REST, and only REST decides that it is removed.

Acceptance: on this Mac without installing anything, `ls`, `cat`, `open` an image in Preview and `rsync` a subset out all
work; two shells reading different files concurrently show no mutual stall; `ucfs status` shows CDN bytes close to the
sum of file sizes read, and a `ls -lR` costs zero CDN bytes. The same on a Linux VM through FUSE.
Measured in [docs/phase1.md](docs/phase1.md): CDN bytes equal the bytes read exactly, and `ls -lR` costs 0 B on both
macOS and Linux.

### Phase 2: Variants as paths (≈ 2 days)

17. `internal/cdn/ops.go`: static table of the 40 URL API operation names, the union of the operation tables in the
    URL API spec (plus `json`, a path there) and the CLI's `url-api` reference, with the two commands that regenerate
    it. An unknown name after `-/` is `ENOENT` without a CDN request; arguments are not checked, the CDN judges them.
    `OutputExt` gives the output's extension: the last `format/<x>` (`jpeg` → `jpg`, `auto` and `preserve` keep the
    original's), `json` and `main_colors` → `json`.
18. Variants view (`internal/vfs/variants.go`): `files/<uuid>/-/<op>/<args>/…/<name>`. `files/<uuid>/` stays out of
    `files/` and lists the original under its own name plus `-`; `-/`, and every `…/-/`, lists nothing and resolves
    only names from the table, so walking a file's directory costs no request. Operation and argument components are
    directories. In one, `-` starts the next operation, the default entry is the variant, a name starting with `.` is
    `ENOENT` (so `.DS_Store` and `._*` probes do not become directories), and any other name is one more argument.
    The default entry is the original's name, mapped like `files/` (`""`, `.`, `..` and `-` become `unnamed`), with
    the extension from `OutputExt`. Its size comes from one `HEAD`, shared by concurrent lookups and kept in the
    `variants` table; `readdir` of an operation or argument directory reports the real size (macOS caches
    READDIRPLUS attributes), so a listing costs the same one `HEAD` as a `stat`.
19. Reads go through the block cache keyed by (uuid, ops), with the variant's ETag in the block object name. A HEAD
    answer is trusted for 24 h; then the next access sends a new `HEAD`, keeps the blocks when size and ETag are
    unchanged, and otherwise drops them and moves the variant's mtime, so NFS clients drop cached pages. FUSE keeps the
    page cache across opens for originals only.
20. A CDN 400 is `EINVAL` on lookup, `stat`, `readdir` and read; the reason is the `<p>` text of the CDN's HTML page,
    fetched with one `GET` (a HEAD answer has no body) and logged at info, and the 400 is remembered for an hour,
    the CDN's own `max-age` for it. A 404 goes to the Phase 1 REST refresh and is remembered for 10 s. go-nfs answered
    every failed LOOKUP with `NOENT` and every failed READ with `IO` or `ACCES`, so `third_party/go-nfs` maps the errno
    inside an error to the NFS status, and macOS reports "Invalid argument". `ucfs status` splits CDN requests into
    HEADs and GETs.

Acceptance: `cp files/<uuid>/-/resize/1200x/-/format/webp/hero.webp .` produces the same bytes as `curl` of the URL;
`stat` on a never-seen variant costs exactly one `HEAD`; a second `stat` costs nothing.
Measured in [docs/phase2.md](docs/phase2.md) on macOS and Linux: same MD5 as `curl`; one `HEAD` and no `GET` for the
first `stat`, nothing for the second; an unknown operation is `ENOENT` and a bad argument `EINVAL`; three variants of one
file read at once share the bandwidth without stalling, with `stat` of another file at 4–22 ms; `cp` of the 1200 px WebP
cost 153,608 B against 9,785,662 B for the original.

Also built in Phase 2: a registry of running mounts in `<user cache dir>/ucfs/mounts/` (canonical mount dir → control
socket, pruned when the socket refuses connections), so `ucfs status` without arguments and `ucfs umount <dir>` find a
mount whatever `--cache-dir` it was started with.

### Phase 3: Writes (≈ 5 days)

21. `internal/spool/`: a written file is a spool file under `<cache>/spool/` plus a row in the index `spools` table.
    `WriteAt` and `Truncate` act on the local file, and a truncate to the current length changes nothing (macOS `cp`
    issues one after every write, Spike C). Commit is an upload with `UPLOADCARE_STORE=1`, the written name as the
    filename (`original_filename`), the `--upload-metadata key=value` pairs, and the view's own metadata
    (`ucfs:path`). Files up to 10 MiB go as one base upload, larger ones in 5 MiB parts, 4 at a time
    (`--multipart-threshold`; 10 MiB is the Upload API's multipart minimum and what uploadcare-go and the CLI use).
    `internal/api` calls `/base/` and `/multipart/start|complete/` itself, so a multipart upload's uuid is known at
    its start. Up to 8 uploads run at once (`--upload-concurrency`). A base upload keeps only what follows the last `\` of a
    name (Spike D), so since Phase 5 a name containing `\` goes up with underscores and whole in metadata `ucfs:name`,
    which the index shows as the file's name in every view (it was `EINVAL` before). Errors:
    "Uploading of these file types is not allowed" (`uploadcare-go-test` rejects a random binary) is `EPERM`, a too
    large file `EFBIG`, a 429 after the Retry-After retries `EAGAIN` (NFS3ERR_JUKEBOX over NFS), network errors and
    5xx `EIO` after three tries. A finished upload is recorded in the index without moving the sync cursor (its time
    comes from the local clock), and its bytes seed the block cache, so reading it back costs no CDN traffic.
22. Commit triggers: FUSE `Flush` and `Fsync` block until the upload ends and return its errno to the writer. On NFS
    the upload starts 300 ms after a COMMIT or a stable (`FILE_SYNC`/`DATA_SYNC`) WRITE, or 3 s after the last WRITE
    (`--commit-idle`); a later write cancels an upload in flight, and an upload of content that changed meanwhile is
    soft-deleted. `third_party/go-nfs` now answers WRITE with `UNSTABLE` when asked and passes COMMIT and stable
    WRITEs to the filesystem: stock go-nfs answered `FILE_SYNC`, so clients never sent COMMIT, and macOS sends a file
    that fits in one 1 MiB WRITE as a single `FILE_SYNC` WRITE. A failed NFS upload is logged and kept as `failed`
    for `ucfs spool retry`. Zero-length files stay local until written or removed (documented limitation; the API
    itself accepts empty files, Spike D). A write to a file this mount uploaded less than a minute ago continues it
    as an overwrite, for writers that pause longer than the idle timer.
23. `tree/` (`internal/vfs/tree.go`): `tree/<path>/<name>` from metadata key `ucfs:path`, whose value is the
    absolute path under `tree/`, name included, as written (NFC from macOS), at most 512 characters
    (`ENAMETOOLONG` beyond). The name comes from the path, not `original_filename`, so `rename` of an uploaded file is
    one metadata `PUT`; a directory rename is one `PUT` per file below it, with a warning above 1000. It is not
    atomic: if a `PUT` fails, the files moved so far are under the new path and the rest under the old, and `mv`
    gets the error; `mv old/* new/` completes it (journaling it was not worth it at one request per file). Files that share
    a path are told apart by a uuid suffix (`photo~1a2b3c4d.jpg`), the newest keeping the plain name. Files without a
    valid `ucfs:path` appear under `tree/_inbox/` by original name, with the same suffix rule. A directory exists
    while a file below it has a tree path; `mkdir`, and `rm` or `mv` of a directory's last file, keep it in
    `tree_dirs`, which is local, until `rmdir`. Writes into `files/` create an alias (`aliases` table, `--alias-ttl`,
    1 h), so `cp x files/photo.jpg && cat files/photo.jpg` works while `ls` shows `<uuid>_photo.jpg`; writes into
    `files/<uuid>_<name>` and `files/<uuid>/…` are `EROFS`. A written file keeps one inode number and NFS handle
    through its upload, overwrites and renames (`objects` table), and so does a renamed directory, so macOS, which
    keeps using the handle of what it renamed, sees no stale handle.
24. Overwrite policy `--on-overwrite=replace|keep`, default `replace`: upload the new file with the replaced file's
    current metadata (read over REST, one request: listings do not show metadata other clients changed later) and
    tags, soft-delete the old one, and point its aliases at the new one; `keep` leaves the old file under a suffixed
    name. Rename over an uploaded file follows the policy without copying metadata. An NSDocument save (TextEdit)
    writes into a scratch directory `(A Document Being Saved By <app>)` beside the file and renames over it, which
    lands as one upload under the final name when the rename comes within the 300 ms settle.
25. `unlink` soft-deletes through `DELETE /files/{uuid}/storage/`, in `tree/` and for aliases in `files/`. Files with
    no tree path that the mount did not write (`files/<uuid>_<name>`, `tree/_inbox/`) cannot be removed, overwritten
    or moved without `--allow-foreign`, so a stray `rm -r` cannot empty a project. `.ucfs/uploads.log` at the mount
    root shows the journal `<cache>/uploads.log`: one tab-separated line per completed upload with time, path, uuid,
    size and MIME type. `ucfs resolve <path> [--json] [--wait <d>]` asks the daemon what a path stands for (uuid and
    CDN URL from the index, aliases and spools, the journal for a path that no longer resolves), since NFSv3 on macOS
    has no xattrs.
26. Local-only shadow files: `.DS_Store`, `._*`, `.Spotlight-V100`, `.fseventsd`, `.Trashes`, `.TemporaryItems`, and
    vim swap files (`.name.swp`, `.swx`, …), which vim rewrites while editing, are stored in `<cache>/shadow/` with
    a `shadow` row and never uploaded; renamed to an ordinary name in `tree/` or `files/`, a shadow file becomes a
    spool and uploads. `.metadata_never_index` always exists at the root. A Finder copy of 50 images wrote 51 shadow
    files with 62 writes.
27. Crash safety: on the next `ucfs mount`, spools that were queued or uploading are uploaded again. A multipart upload
    is checked by the uuid recorded at its start: the API lists an abandoned one as a file that is not ready, and it
    is soft-deleted first; a finished one is only recorded. A base upload that completed before the crash is found in
    the synced index by name, size, tree path and time. Spools cut off while written stay for `ucfs spool
    list|retry|drop`, which talk to the running daemon or act on the cache dir when none runs. A writable mount
    locks its cache dir; unmount uploads what is still being written and waits up to 2 minutes.

Acceptance: `cp photo.jpg tree/photos/` leaves `tree/photos/photo.jpg` with `original_filename` `photo.jpg` and a
journal line; saving from TextEdit and from `vim` (temp file + rename) works in `tree/`; `rm` soft-deletes; Finder
copy of a folder with 50 images uploads 50 files and zero sidecars; a killed daemon mid-upload resumes on restart.
Measured in [docs/phase3.md](docs/phase3.md) on macOS and Linux: the file is readable with its name, tree path,
journal line and a uuid from `ucfs resolve`, 1.8–4.3 s after `cp` returns on macOS (0.33–0.37 s of it the trigger, the
rest the upload) and 6–14 ms on Linux, where close waits for the upload; vim saves work on both hosts (in place on
macOS, with a backup copy on Linux; the rename save is tested against the fake only); an NSDocument save works
through `FileManager.replaceItemAt` while TextEdit itself sat behind a dialog that needs the user; a Finder
copy of 50 images made 50 uploads, 0 sidecar uploads and 0 REST requests in 6.6 s; a daemon killed mid-multipart
upload resumed and the file arrived once on both hosts; a 12.2 MB file went up in parts in 3.6–5.2 s.

### Phase 4: Metadata, tags, groups, dates (≈ 3 days)

28. xattrs on Linux FUSE (`internal/vfs/xattr.go`, go-fuse `Getxattr`/`Listxattr`/`Setxattr`/`Removexattr`): read-only
    `user.uc.uuid`, `.cdn_url` (with the operations path for a variant), `.mime`, `.md5`, `.width`, `.height` (images
    only), `.stored` (`true`/`false`); read-write `user.uc.meta.<key>` (one metadata `PUT`, `removexattr` one `DELETE`)
    and `user.uc.tags`, comma-separated as the Upload API's `tags` field (a tag cannot hold a comma), written with one
    `PUT /files/{uuid}/tags/` that replaces the list; the API's normalised answer goes to the index. Every successful
    write updates the index at once. Limits: a key over 64 characters is `ENAMETOOLONG`, other than `[\w\-\.\:]+` or an
    empty or non-UTF-8 value `EINVAL` (the API routes such keys to a 404 page and refuses empty values), a value over
    512 characters (the API counts characters, not bytes) `E2BIG`, a tag over 100 characters `ENAMETOOLONG`, a 51st key
    or tag `ENOSPC` (the 50-key limit is the API's 400, which `internal/api` maps by message, since the index may not
    know keys other clients added). `user.uc.meta.ucfs:path` is `EPERM`: `mv` moves files, and keeps foreign files out
    of reach without `--allow-foreign`. Other read-only names are `EPERM`, names outside `user.uc.*` `ENOTSUP` on set
    and `ENODATA` on get without any lookup (go-fuse answers `security.capability`, which the kernel asks before every
    write). `XATTR_CREATE`/`XATTR_REPLACE` are honoured; `removexattr` of a missing key is `ENODATA`, decided from the
    file's current info because the API answers `DELETE` of a missing key with 204. `md5` is the CDN's ETag, one `HEAD`
    on first read and kept; for multipart uploads that ETag is S3's hash of part hashes (`…-3`), so uploads record their
    MD5 from the spool, and other files list `md5` only when known or under 10 MiB, the multipart minimum. A file still
    being written lists `user.uc.uuid` with an empty value and queues `setxattr` of metadata and tags into its upload
    form (persisted with the spool, `EAGAIN` while the upload is in flight), so `cp --preserve=xattr` into the mount
    carries metadata. Read-only attributes set or removed on such a file, or on one the mount uploaded within the last
    minute (rsync sets attributes after closing, which uploads), are ignored, so `cp --preserve=xattr` and `rsync -X`
    of files from the mount succeed. xattrs on files in every view change the file's metadata; a `--read-only` mount is
    `EROFS`.
29. Views (`internal/vfs/views.go`), all read-only (`EROFS` on create, write, `rm`, `mkdir`, `mv` in or out):
    `by-date/YYYY/MM/DD/<uuid>_<name>` from `datetime_uploaded` in `--by-date-zone` (default the local zone), each
    year, month or day found with one index probe; `tags/<tag>/<uuid>_<name>`, where `tags/` lists every tag a present
    file in the index carries (`json_each` over the `tags` column; `.` and `..` skipped); `groups/<id>/<index>_<name>`,
    where `<id>` is the group id `<uuid>~<count>` and the index counts from 0 as the CDN's `/nth/` does, zero-padded to
    the width of the last index so `ls` order is group order, keeping gaps for removed members. Group sync lists
    every group (`GET /groups/`, one request per thousand) with every sweep (`--sweep-interval`, 10 min) when the
    view is mounted, and since Phase 5 drops the groups it no longer lists; a group's
    members come from `GET /groups/{id}/` on its first listing (uploadcare-go's `group.Info` has no files, so the client
    sends that request itself) and are kept, since groups never change; a group id not synced yet is read on lookup, and
    a 404 drops it. The API lists no deleted groups, which is how a deleted group leaves `groups/` within a sweep. View nodes have NFS handles like variants: directories and `tags/`/`groups/` files by path through
    `node_ops`, `by-date/` files by uuid alone.
30. `ucfs resolve <path> [--json]` prints metadata and tags too, plus the CDN URL (uuid, CDN URL and pending state came
    in Phase 3); for a pending upload it shows what was queued on it. It refreshes info older than `--meta-ttl` like an
    xattr read, so macOS, which has no xattrs over NFSv3, sees other clients' changes the same way.
31. Lazy metadata refresh: `getxattr` and `ucfs resolve` re-fetch file info (`GET /files/{uuid}/`, recorded without
    moving the sync cursor) when the index entry is older than `--meta-ttl` (5 min); concurrent readers share one
    request, a failed refresh serves the index's copy, and a file found removed is `ENOENT`. `listxattr` never refreshes:
    coreutils' `ls -l` lists the attributes of every file. `ucfs status` counts refreshes, xattr calls, md5 `HEAD`s and
    group reads.
32. `--views` selects the mounted views (default `files,tree,by-date`; `tags` and `groups` opt-in), and the root
    listing shows only those; an unknown name is an error at mount.

Acceptance: `setfattr -n user.uc.meta.camera -v Canon tree/x.jpg` is visible in the dashboard; `tags/approved/` lists
exactly the search result for `tags.all=[approved]`.
Measured in [docs/phase4.md](docs/phase4.md) against `alex-gusev-test`: on Linux the `setfattr` is one metadata `PUT`
(0.21–0.29 s) that the API and `uploadcare metadata list` show, `getfattr -d` matches the API and the file (md5 equal to
`md5sum`), `setfattr -x` deletes the key, and `tags/p4-approved/` lists the same four files as the search API once its
asynchronous index settles; `groups/<id>/` lists a group in creation order for one request; `ls -lR by-date/` costs 0 CDN
bytes and 0 REST requests on both hosts; `getfattr -d` costs 1 REST request (0.21 s) on an entry past the TTL and none
(7–8 ms) within it, and a CLI change shows by `getfattr` on Linux and `ucfs resolve` on macOS once the entry is a TTL
old. Measuring found that a multipart upload's ETag is no MD5 and that `cp --preserve=xattr` and `rsync -X` failed on
the read-only attributes; both are fixed.

### Phase 5: Hardening (≈ 3 days)

33. Traffic budget `--traffic-budget` (`500MB`, `2GiB`, `500MB/day`) caps the CDN bytes read per calendar day in the
    local zone (`cache.Budget`). Only range bodies count: `HEAD`s carry none and keep working, REST requests are not CDN
    traffic, and the CDN's 1 KiB error pages are left out. A block is counted before it is fetched and given back if
    the fetch fails, so the count never passes the limit; prefetch stops while less than a reserve remains (a tenth of
    the budget, at most 16 MiB), which keeps the last bytes for reads someone waits for. The count lives in the index,
    flushed every 5 s, so a remount continues the day and mounts of one project share it. A read past it fails with
    `EDQUOT` ("Disc quota exceeded" on macOS through `NFS3ERR_DQUOT`); the log says so once per window and `ucfs status`
    shows used, remaining, the reset time and reads refused. Linux 7.x reports a failed buffered FUSE read as `EIO`, so
    a file larger than what is left is opened with direct I/O, where the reader gets `EDQUOT`.
34. One errno table (`internal/api/errno.go`) for REST, Upload API and CDN failures, the CDN client returning typed
    errors: 404 `ENOENT`; 401, 403 `EACCES`; 429 after the Retry-After retries `EAGAIN` (`NFS3ERR_JUKEBOX` over NFS,
    which macOS retries for 28 s before `ETIMEDOUT`); 5xx, network errors and a changed ETag `EIO`; a CDN 400 `EINVAL`;
    errnos the filesystem returns (`EROFS`, `EOPNOTSUPP`, `EDQUOT`) pass through. REST requests are bounded at 4 in
    flight. The client keeps the REST state (ok, denied, throttled, since when); denied or throttled for a sync
    interval, the mount is degraded: it serves the index and the block cache, skips info refreshes, and refuses what
    needs the API (new spools, `rm`, `mv`, metadata and tags of uploaded files) with `EACCES` or `EAGAIN`; `ucfs status`
    says `API denied since <time>` or `throttled since <time>`. Failed syncs back off, doubling up to 10 minutes, with
    one warning per streak. A mount whose index was synced once starts without the API. Every range's ETag is compared
    with its object's (weakly, RFC 9110); on a mismatch the object's blocks go and the read fails with `EIO`, and so does
    a read that continues a stream begun before a content change (§4.4).
35. Backpressure: `--max-inflight` (32) CDN requests per mount (`cdn.Limiter`). Reads someone waits for and `HEAD`s go
    before prefetches, prefetches hold at most three quarters of the slots, and within each class objects take turns;
    a queued prefetch costs no goroutine and joins a fetch only once it has its slot. `FetchTimeout` starts at the slot.
36. Security defaults: a config file group or others can read is refused (`--insecure-config` overrides); cache,
    blocks, spool and shadow dirs 0700 (fixed at mount), index files and the control socket 0600, sockets that do not
    fit in the cache dir in a per-user 0700 temp dir; `nosuid,nodev` on every mount, no `allow_other`; key values are
    replaced in logs and error messages. The NFS server offers only `AUTH_UNIX` and grants ACCESS only to its own uid and
    root, so other users cannot read through the mount; the loopback NFS port itself is open to any local process
    (docs/phase5.md, *Security*). Node ids are checked for collisions where they are handed out and salted (§4.3).
37. Benchmarks against the fake (`make bench`, a Markdown table from `spikes/benchtable`): cold `cat` of 5 MB,
    `ls -l` of 10,000 entries, variant readers and small reads beside bulk ones (p95 alone and beside). CI
    (`.github/workflows/ci.yml`): lint for linux and darwin, build, `make race` with FUSE mounted on `ubuntu-latest`,
    and the benchmarks with their table, compared with the previous run's, in the job summary. Nothing needs the
    project's keys.
38. Soak (`make soak`, `SOAK=24h`): a real mount of a fake project syncing every second, with uploads and deletes
    through the mount, other clients adding, removing and changing files and groups, and readers of originals and
    variants, sampled into a CSV (goroutines, descriptors, heap, RSS, cache, index); `UCFS_SOAK_LIVE=<project>` runs it
    against a real project, `tree/` only.

Acceptance: a `cat` that crosses the budget fails with "Disc quota exceeded"; a mount started while REST answers 403
serves `ls` and cached reads and reports the denial; a changed ETag never returns mixed bytes; a small read beside bulk
readers keeps its p95; a 0644 config is refused; no key in the logs; benchmarks in CI; no growth in a soak.
Measured in [docs/phase5.md](docs/phase5.md): the budget `cat` fails after exactly 2 MiB on macOS live ("Disc quota
exceeded") and Linux FUSE live ("Disk quota exceeded"); the denied mount lists, reads, refuses writes with EACCES and
says so on macOS NFS, Linux FUSE and Linux NFS (fake); p95 of a cold 64 KiB read 37 ms alone and 34 ms beside four bulk
readers, 304 ms with a plain bound (fake), 420 and 727 ms live, where no read waited for a slot and the downlink was
shared; 0 key hits in the live debug logs. Soaks of 3 hours against the fake (macOS NFS and Linux FUSE, about
160,000 operations and 5,300 uploads each) and 2 hours live kept goroutines and descriptors flat and the cache at its
limit; the index grows with churn, keeping removed files' rows; RSS rose from 86 to 108 MiB on macOS with a flat Go
heap, and the Linux heap from 46 to 80 MiB, mostly go-fuse nodes for entries the kernel still caches. The soaks found
a connection leaked per control request and interrupted FUSE requests answered with EIO, both fixed. The 24-hour run
is `make soak`.

### Phase 6: Packaging and docs (≈ 3 days)

39. Decided on 2026-11-08 and recorded in [docs/release.md](docs/release.md): the repository stays `alxgsv/ucfs`,
    the licence is MIT ([LICENSE](LICENSE)), and 0.1.0 ships without Homebrew: the formula is built but not pushed
    (`skip_upload: true`) until the tap `alxgsv/homebrew-tap` and `HOMEBREW_TAP_TOKEN` exist. The binaries are not
    signed or notarized. The doc has the owner's steps for 0.1.0 (make the repository public, fix the Actions
    billing, push `v0.1.0`), how to test on a fresh Mac with Gatekeeper on, the Homebrew steps, and every place to
    change for a later move to the `uploadcare` organization. None of the steps is done.
40. `.goreleaser.yaml` (goreleaser v2): `CGO_ENABLED=0` builds for darwin and linux on amd64 and arm64 with
    `-trimpath`, version, commit and date in `ucfs version`; `ucfs_<version>_<os>_<arch>.tar.gz` archives with the
    README and `contrib/`; `checksums.txt`; a Homebrew formula with a `test do` (`ucfs version`), caveats and
    `contrib/` in `pkgshare`, for the tap with `HOMEBREW_TAP_TOKEN` but not pushed for 0.1.0 (`skip_upload: true`). It uses the
    deprecated `brews` section, as casks have no test block and are quarantined: goreleaser 2.20's `check` exits 2
    on it. `.github/workflows/release.yml` runs the tests and goreleaser on a `v*` tag; `make snapshot` builds
    `dist/` locally. `scripts/install.sh` follows uploadcare-cli's: `VERSION`, `INSTALL_DIR`, `UNINSTALL` and flags
    of the same names, checksum check, `sudo` only when the directory needs it, plus `--base-url`/`UCFS_BASE_URL`
    for a local `dist/`; it picks the archive from `checksums.txt`, so the latest release needs no API call.
41. `ucfs mount --daemon` (`cmd/ucfs/daemon.go`) runs the same command again in its own session, stdin from
    `/dev/null`, output appended to `<cache dir>/mount-<hash>.log`, and returns with the pid once the control socket
    answers; a daemon that exits first fails the command with what it logged, and one still starting after 10 minutes
    keeps running. `contrib/launchd/com.uploadcare.ucfs.example.plist` (a user agent: `RunAtLoad`, `KeepAlive` on
    failure only, `ExitTimeOut` 60 s, the most launchd allows an agent, for the uploads an unmount finishes) and `contrib/systemd/ucfs@.service` (a
    user unit per escaped mount path: `Type=simple`, `ExecStop=-ucfs umount`, `Restart=on-failure`, flags in a
    drop-in), each with a README. After a crash both restart into the stale mount until `ucfs umount <dir>` clears
    it (item 52).
42. `README.md` and `docs/`: [mount-options.md](docs/mount-options.md) (every command and flag, and the
    `status --json` fields, Phase 5's included), [finder.md](docs/finder.md), [costs.md](docs/costs.md),
    [semantics.md](docs/semantics.md) (results and errnos per operation, place and front-end, from the code, with
    those seen on a mount marked), [troubleshooting.md](docs/troubleshooting.md), [release.md](docs/release.md).
43. `AGENTS.md`: build, test and lint, the mount tests and live checks, test uploads marked `ucfs:test=1`, leaving no
    mount or registry record behind, secrets, commit signing and one PR per phase; `CLAUDE.md` imports it.

Result: the install script and `ucfs mount ~/uc` on a fresh Mac once 0.1.0 is released; `brew install
alxgsv/tap/ucfs` from a later release (step 39). Measured in [docs/phase6.md](docs/phase6.md): `goreleaser release --snapshot` builds the four archives
(5.3–5.9 MB); `install.sh` installs from a local server on macOS and on Linux (dash); the generated formula installs
through a local tap and passes `brew test`; `ucfs mount --daemon` returns in 1.0 s with the mount up, and
`ls -lR` through it costs 0 CDN bytes; the launchd agent and the systemd unit mount, stay down after `ucfs umount`,
restart after a failure, and leave nothing behind; after the Phase 5 merge the same `--daemon`, launchd and systemd
mounts came up again in 0.1–2 s, read-only, for 2 REST requests and no CDN bytes.

### After V1 (release follow-ups)

Work found during the phases that does not block the V1 release. Phases append here what they defer past it.

44. The 24-hour soak, unless Phase 6 ran it: `make soak` (fake, NFS on macOS and FUSE on Linux) and a live run with
    `UCFS_SOAK_LIVE`, on a machine that does not sleep. Phase 5 ran 3 hours per platform and 2 hours live
    (docs/phase5.md, *Soak*). Done: the CSVs of a full day in `docs/`, and each gauge flat or levelled off, or the cause
    of its growth named.
45. Keep other local processes off the loopback NFS port. The server answers anyone who connects to `127.0.0.1:<port>`,
    and AUTH_UNIX proves nothing: in Phase 5 a second user read `files/` over raw NFS RPC, with its own uid and with a
    forged one, and could have written as the mount may. ACCESS now keeps other users out of the mount point, not out
    of the port. Fix with a front-end without a network port (FUSE-T, V1.1) or, on macOS, by checking the owner of the
    peer socket (the TCP socket table, `sysctl net.inet.tcp.pcblist_n`) and refusing other uids. Done: the Phase 5
    probe (`go-nfs-client` as another user) is refused, and the owner's mount works.
46. Tell the FUSE kernel about removed files. go-fuse keeps a node for every entry the kernel still caches, including
    files removed since, until the kernel forgets it; in a 3-hour Linux soak with heavy churn the heap grew from 46 to
    80 MiB that way. On the sweep (and after a CDN 404), invalidate the removed entries (`NotifyDelete` or
    `NotifyEntry` on their parents) so go-fuse can drop the nodes. Done: a churn soak whose Linux heap levels off.
47. Prune index rows of files removed long ago. The index keeps a row per removed file (about 1 KB each; 9,000 rows
    in a 3-hour soak), which `by-date/` and `ucfs resolve` of old paths do not need after a while. Done: rows removed
    longer than a set age (30 days) go with the sweep, and the index size of a churn soak levels off.
48. Bandwidth-aware prefetch. The limiter keeps a slot free for reads, but on a slow downlink 24 concurrent 1 MiB
    prefetches still take the bandwidth: live, a small read's p95 went from 420 to 727 ms beside one bulk reader with no
    wait for a slot. Size the prefetch concurrency per stream from the measured throughput instead of a fixed share
    of `--max-inflight`. Done: the live check of Phase 5 keeps the small read's p95 within 1.2× of alone, without
    slowing a single bulk reader.
49. Explain the macOS RSS growth. In the 3-hour NFS soak RSS rose from 86 to 108 MiB while the Go heap stayed at
    20–32 MiB. Candidates: SQLite's allocations in modernc's libc, outside the Go heap, and pages the runtime keeps after
    peaks. Done: the cause named from a 24-hour run with RSS split by source (heap profile, `runtime/metrics`, the
    SQLite memory counters), and fixed or bounded.
50. Observe a live 429. Throttling (Retry-After retries, the degraded mode, JUKEBOX over NFS) is tested against the
    fake only; the test projects never throttled. Done: a 429 from the live API seen and recorded (headers, Retry-After
    values), and the fake's throttle matched to it.
51. Offer the go-nfs patches upstream: errnos as NFS statuses, COMMIT reaching the filesystem with UNSTABLE writes,
    exclusive CREATE as guarded, and the ACCESS filter (`third_party/go-nfs/README.ucfs.md`). Needs the user's OK
    before anything is sent. Done: pull requests open on `willscott/go-nfs`, and, once merged, the `replace` in
    `go.mod` and the copy removed.
52. Let a restarted daemon clear the stale mount at its own directory. After a daemon is killed (SIGKILL from launchd
    past `ExitTimeOut`, the OOM killer, a crash), its mount point stays attached to a server that is gone, and the
    restarts the launchd and systemd examples make fail on it ("operation timed out" over NFS, "device or resource
    busy" over FUSE) until someone runs `ucfs umount <dir>` (docs/phase6.md, *launchd* and *systemd*). In `runMount`,
    before mounting: when the registry record for the directory names a dead pid and the directory is still a mount,
    unmount it as `ucfs umount` does. Done: the Phase 6 crash checks (`kill -9` of the daemon under launchd and under
    systemd) mount again by themselves, with no `ucfs umount`.
53. Make `make build` stamp the commit and date. goreleaser sets `main.commit` and `main.date`; the Makefile sets only
    `main.version`, from `git describe`, so a source build prints a `v`-prefixed version and takes the commit from the
    Go build info, which a build outside a checkout lacks and a build in a git worktree got wrong (docs/phase6.md,
    *After the Phase 5 merge*). Pass `-X main.commit=` and `-X main.date=` from git and drop the `v`. Done: `make
    build` on a tagged commit prints the same `ucfs version` line as the release archive, apart from the build date,
    and a build in a worktree inside another checkout prints the worktree's commit without running git in the outer
    checkout.
54. Lint the workflows with actionlint, and run the release workflow once. Installing actionlint was refused in Phase
    6's session, so `release.yml` was only parsed as YAML, and it has never run. Done: `actionlint` reports nothing
    on both workflows, from CI or a make target, and the workflow has published a pre-release tag (`v0.1.0-rc.1`).
55. Publish to Homebrew. 0.1.0 ships without it (step 39): create the tap `alxgsv/homebrew-tap`, store a
    fine-grained `HOMEBREW_TAP_TOKEN` limited to it, and set `skip_upload: auto` ([docs/release.md](docs/release.md),
    *Homebrew later*). goreleaser deprecates the `brews` section the formula uses (2.20's `check` exits 2); move to
    casks only if a cask can keep the `test` and avoid the quarantine. Needs the owner. Done: `brew install
    alxgsv/tap/ucfs` on a fresh Mac installs the release, and `brew test ucfs` passes.
56. Sign and notarize the macOS binaries. They are unsigned (darwin/arm64 carries only the linker's ad-hoc
    signature), so a browser download is quarantined and refused until `xattr -d com.apple.quarantine`; the install
    script and Homebrew avoid the quarantine. Needs an Apple Developer account, a Developer ID Application
    certificate and an App Store Connect API key as repository secrets, for goreleaser's `notarize` section. Done: a
    release binary downloaded with Safari runs on a Mac with Gatekeeper on without a dialog, and `spctl --assess`
    accepts it.

## 4. Cross-cutting specifications

### 4.1 Index schema

```sql
files     (uuid TEXT PRIMARY KEY, name TEXT, size INTEGER, mime TEXT, is_image INTEGER, is_ready INTEGER,
           stored INTEGER, removed INTEGER, uploaded_at INTEGER, stored_at INTEGER, removed_at INTEGER,
           md5 TEXT, width INTEGER, height INTEGER, metadata TEXT /*json*/, tags TEXT /*json, null without the feature*/,
           tree_path TEXT, info_refreshed_at INTEGER,  -- md5 from an upload or the CDN's ETag, kept by listings;
           -- info_refreshed_at is when the row last came from the API, which the metadata TTL is measured from (Phase 4)
           tree_dir TEXT, tree_key TEXT, name_key TEXT);  -- NFC keys of a valid ucfs:path and of the name (Phase 3)
           -- indexes: uploaded_at, name, tree_path, removed, (tree_dir, tree_key), name_key
variants  (uuid TEXT, ops TEXT, size INTEGER, mime TEXT, etag TEXT, checked_at INTEGER, changed_at INTEGER,
           PRIMARY KEY (uuid, ops));                -- changed_at: first HEAD with this size and ETag (Phase 2)
node_ops  (id INTEGER PRIMARY KEY, uuid TEXT, ops TEXT);  -- operations path behind a variants view node id (Phase 2)
groups    (uuid TEXT PRIMARY KEY, created_at INTEGER, files TEXT);  -- uuid is the group id <uuid>~<count>, created_at
           -- in ns (to the second, as uploadcare-go reads it); files is a JSON array of member uuids, "" for a removed
           -- member, NULL until the group's info was read (Phase 4)
aliases   (view TEXT, name TEXT, uuid TEXT, expires_at INTEGER, PRIMARY KEY (view, name));  -- index on uuid
tree_dirs (path TEXT PRIMARY KEY, created_at INTEGER);  -- tree directories kept while empty, local only
shadow    (path TEXT PRIMARY KEY, blob_ref TEXT, size INTEGER, mtime INTEGER, ino INTEGER UNIQUE, dir INTEGER);
objects   (ino INTEGER PRIMARY KEY, uuid TEXT UNIQUE, key TEXT);  -- inode number of a file the mount wrote (Phase 3)
spools    (id TEXT PRIMARY KEY, ino INTEGER, key TEXT, path TEXT, state TEXT, replaces TEXT, upload_uuid TEXT,
           created_at INTEGER, committed_at INTEGER, attempts INTEGER, error TEXT,
           queued TEXT);  -- writes not uploaded yet; queued: JSON of metadata and tags set before the upload (Phase 4)
sync_state(key TEXT PRIMARY KEY, value TEXT);  -- list cursor, last full scan, last removed sweep, traffic_day and
           -- traffic_used of the traffic budget (Phase 5)
node_salts(key TEXT PRIMARY KEY, salt INTEGER);  -- node keys whose plain id collided (Phase 5, §4.3)
```

### 4.2 Sync algorithm

- Full scan: `GET /files/?ordering=datetime_uploaded&limit=1000&from=<cursor>` until exhausted; upsert; cursor =
  last `datetime_uploaded` (re-fetch of equal timestamps is deduplicated by uuid).
- Incremental: same call from the cursor every `--sync-interval` (default 30 s); the first runs as the mount starts, in
  the background when the index was synced before. Failures back off, doubling up to 10 minutes (Phase 5).
- Removals: `removed=true` listing every 10 min marks `removed`; a CDN 404 on read does the same immediately.
- Metadata and tags changed by other clients are not visible in listings: refreshed lazily on `getxattr` and
  `ucfs resolve` when older than `--meta-ttl` (5 min), by `ucfs index sync --full`, and for chosen files by
  `ucfs index refresh <uuid>...` (Phase 5). So `tags/` shows a tag set elsewhere on an already listed file only after such a refresh; tags set
  through the mount (`user.uc.tags`, uploads) show at once.
- Groups: every group, `GET /groups/?ordering=datetime_created` from the start, with every removed sweep (10 min) when
  `groups/` is mounted; groups no longer listed are dropped (Phase 5); members on first listing of a group.
- Search API is used only by `ucfs resolve --search` and by tree repair, never on the request path.

### 4.3 Node ids and NFS file handles

`NodeID` is a 64-bit FNV-1a hash of `(view, uuid, ops-or-path)`, or an object's inode number (tree and shadow files).
Since Phase 5 the front-ends take ids from `FS.ID`, which detects collisions where ids are handed out: it records the
key each id was first handed out for, by a second hash (FNV-1), and for nodes known by path also checks the path
`node_ops` recorded in an earlier run. A key whose id is taken gets the hash of the key and a salt, and the salt goes
to `node_salts (key, salt)` in the index, so the key keeps its id across restarts. Object inode numbers cannot move, so
a hashed id that meets one is the one salted. Ids kept for the check are bounded at 2^18 (salts are kept for good); the
check costs about 94 ns per id. Without it a collision (about 3·10⁻⁸ at 1 M files) would have made go-fuse serve one
node for another, and an NFS handle of a path node resolve to the other path. NFS file handles are self-describing (`version:1 byte, kind:1 byte, reserved:2 bytes,
uuid:16 bytes, hash:8 bytes`), so a restarted daemon serves old handles without a path cache and never returns
`ESTALE` for files that still exist. Handles must be a multiple of 4 bytes long: go-nfs reads them with
`xdr.ReadOpaque`, which skips no XDR padding, and a 26-byte handle made every ACCESS reply deny everything.
A variants view node (`-`, an operation, an argument, the variant) hashes its operations path, such as
`-/resize/1200x/`, which does not fit in the handle. The first handle handed out for such a node records
`(id, uuid, ops)` in `node_ops` (`INSERT OR IGNORE` behind an in-memory set), and decoding looks the path up by id,
so these handles also survive a restart; an id never handed out is `ESTALE`. The MOUNT reply offers `AUTH_UNIX` only,
so clients send the caller's uid, and ACCESS grants nothing to uids other than the daemon's and root (Phase 5).

### 4.4 Read path

Open → sequential detector per object (streams per reader) → block cache (`<cache>/blocks/<uuid>/<blockno>` for
originals, `<cache>/blocks/<uuid>~<hash(ops, etag)>/<blockno>` for variants) → singleflight → traffic budget → a slot of
the mount's `cdn.Limiter` (reads before prefetches) → ranged `GET` → ETag check → block stored. Prefetch window starts
at 1 MiB and doubles while reads stay sequential, up to 16 MiB; a prefetch waits in the limiter's queue without a
goroutine and stops while the budget is down to its reserve. Originals never revalidate. A variant's HEAD answer
expires after 24 h; the next access sends a new `HEAD`, and a changed size or ETag drops the old blocks.

Since Phase 5 every range's ETag is compared with its object's, weakly (the CDN sends `W/` on variants): a variant's is
its `HEAD`'s, an original's the first its ranges returned in this process; the CDN answered the same ETag on `HEAD`,
`206` and `416` (measured). A mismatch drops the object's blocks and fails the read with `EIO`, and the next access to
a variant sends a new `HEAD`. A reader that already received old blocks cannot be helped by a retry, and NFS has no
open handle, so the cache keeps an epoch per object that every content change bumps: a read that continues a stream
begun in an earlier epoch fails once (a read from offset 0 starts over), a read during which any fetch saw a change
fails, and an original's block fetched before a change is refused when stored after it. So no reader gets a file made
of two renderings, also across a variant's daily `HEAD` that finds new content.

### 4.5 Write path

Create (a new object, or an overwrite that keeps the replaced file's object) → spool → writes → commit trigger
(FUSE `Flush`/`Fsync`; NFS `COMMIT` or stable `WRITE` + 300 ms, or 3 s idle) → upload (base ≤ 10 MiB, parts above;
metadata: replaced file's current metadata, `--upload-metadata`, `ucfs:path`) → object row, index row without moving
the cursor, overwrite policy (soft-delete the replaced file unless `keep`), alias for a name written in `files/`,
journal line, block cache seeded → spool removal. A write during the upload cancels it; a rename moves the spool and
uploads it under the new name. Upload errors return their errno on FUSE `Flush` (`EPERM` for a rejected type, `EIO`
for network and 5xx); on NFS they are logged and the spool is kept for `ucfs spool retry`. Spools queued or uploading
when the daemon died are uploaded again by the next mount (item 27).

NFS identity: a tree file's handle carries its object's inode number and key (a uuid or zero), so the client's handle
from CREATE stays valid after the upload and after renames; a tree or shadow directory's handle id is mapped to its
current path in `node_ops`, which a rename rewrites.

### 4.6 Mount options (confirmed by Spike A)

macOS, run as the user: `mount_nfs -o port=P,mountport=P,vers=3,tcp,locallocks,noresvport,rsize=1048576,
wsize=1048576,readahead=0,nfc,soft,intr,acregmax=3600,acdirmax=5,nosuid,nodev,nobrowse[,rdonly] 127.0.0.1:/ <dir>`
(`nosuid,nodev` since Phase 5; macOS forces them on such a mount anyway). Without
`rsize`/`wsize` the client negotiates 32 KiB. `readahead=0` is a traffic choice: at 1 MiB blocks the client's default
read-ahead turns a 4 KiB read into 16 MiB of traffic. Re-measured in Phase 1 with the daemon's concurrent prefetch:
cold `cat` of 16 MB takes the same 1.4–1.5 s at `readahead` 0 and 2, while `head -c 4096` costs 1 MiB at 0 and 2 MiB
at 2, so 0 stays the default and `--readahead` overrides it. `127.0.0.1`, not `localhost`, which can
resolve to `::1`. `nobrowse` keeps the mount out of the Finder sidebar; `rdonly` with `--read-only`. Linux FUSE:
`fsname=ucfs`, `noatime`, `nosuid`, `nodev`, `max_read=1048576`, no `allow_other`, `ro` with `--read-only`; Linux NFS,
the fallback, which needs root: `nfsvers=3,proto=tcp,mountproto=tcp,nolock,soft,rsize/wsize=1048576,acregmax=3600,
acdirmax=5,nosuid,nodev[,ro]` (mounted for the first time in Phase 5, in a VM with root).

## 5. Testing strategy

- Unit tests for `vfs`, `cache`, `spool`, `index`, `api`, `cdn` against `internal/fake` (REST with cursor pagination,
  metadata, storage, search, groups, and since Phase 5 a REST denial with the live 403 body, persistent throttling and
  REST latency; Upload with base and multipart; CDN with content generated by chunk, `Range` and `HEAD` for originals
  and variants, latency, failures by status and an original that changes under its uuid). `make race` runs them
  under the race detector (cgo); every package passes on macOS and, as root with FUSE mounted, in an Ubuntu 26.04 VM.
- Command tests (`cmd/ucfs`) run the `ucfs` command in-process against the fake (`apiTransport`) and, with
  `UCFS_MOUNT_TESTS=1`, mount for real: NFS on macOS as the user, FUSE on Linux, NFS on Linux as root
  (`UCFS_FRONTEND=nfs`). The degraded API, the traffic budget and throttled writes are tested this way;
  `TestServeFake` keeps a fake project mounted for checks by hand (Finder, other users).
- Front-end tests: Linux FUSE mounted in CI (`ubuntu-latest` with `fuse3`; with `UCFS_MOUNT_TESTS=1` a test that cannot
  mount fails rather than skips); NFS over RPC with `go-nfs-client`. macOS runners are left out of CI: the repository
  is private, and macOS minutes cost ten times as much.
- Live measurements by hand against a test project, as the `docs/phaseN.md` reports describe, with `spikes/measure`,
  `spikes/rest` and `spikes/keygrep`; no live test runs in CI, which holds no keys.
- Benchmarks (`make bench`) run in CI on every pull request, with the table compared against the previous run's
  medians in the job summary; `bin/bench.txt` of two runs also goes into `benchstat`.
- Soak: `make soak` (24 h against the fake; `UCFS_SOAK_LIVE` for a real project).

## 6. Risks and where they are retired

| Risk | Phase that answers it | Fallback |
|---|---|---|
| Non-root NFS mount on macOS | Retired in 0 (Spike A): works as the user | — |
| Unpublished rate limits during full scans | 5, as far as ucfs can without a live 429: Retry-After retries (5, up to 60 s each), at most 4 REST requests in flight, and a sync loop that backs off to 10 minutes; throttled for a sync interval, the mount serves the index and refuses writes with `EAGAIN`. Spike E ran on a 33-file project and saw no 429 | resume from cursor (implemented) |
| API access withdrawn (a project over its usage limit answers every REST request 403 `permission_error`, Phase 3) | Retired in 5: a mount with an index starts and serves the index and the block cache, refuses writes with `EACCES`, backs off its syncs and says `API denied since` in `ucfs status` (measured against the fake on all three front-ends) | — |
| `original_filename` mangling | Retired in 0 (Spike D), and in 5 for names with `\`: they upload with underscores and whole in `ucfs:name`, which the index shows | — |
| Finder/Spotlight traffic | 0 (Spike C), 3, 5. Spotlight is off on NFS mounts by default; a Finder window read every visible test image in full on first visit (JPEGs without an embedded thumbnail, also with `readahead=0`; Quick Look caches thumbnails across mounts, docs/phase1.md), and `.ql_disablethumbnails` does not stop it. Bounded in 5 by `--traffic-budget`: a `by-date/` window past the budget got `EDQUOT` once per photo, without retries; files the mount uploaded cost nothing, being in the block cache | `nobrowse`, `rdonly`, `readahead=0`, shadow files, traffic counter and budget (implemented) |
| NFS has no close event | 3: macOS sends COMMIT, or a single stable WRITE for a small file, right after the data once go-nfs answers WRITE with `UNSTABLE`; the upload starts 300 ms later, and upload errors cannot reach the writer | 3 s idle commit; FUSE-T front-end in V1.1 for strict semantics |
| No xattrs over NFSv3 on macOS | 4: `ucfs resolve [--json]` prints metadata and tags, refreshed after `--meta-ttl` | the CLI for writes; xattrs on macOS with the FUSE-T front-end in V1.1 |
| go-nfs maturity ("minimally tested") | 1, 2, 5: a 3-hour soak on macOS NFS (168,000 operations, 5,344 uploads) and 2 hours live with Finder open saw no NFS error other than ENOENT and ESTALE for files removed meanwhile. Phase 5 also found that it offered only `AUTH_NULL` and granted every ACCESS | fork and fix, taken in Phase 2: `third_party/go-nfs` passes errnos through (LOOKUP could only fail with `NOENT`), COMMIT (3) and an ACCESS filter (5); offer the patches upstream. rclone uses the same library in production |
| Other local users reading the project | 5: through the mount point, retired (ACCESS grants only the daemon's uid and root; FUSE has no `allow_other`). Through the loopback NFS port, open: any local process can speak NFS to it and claim any uid (measured) | FUSE-T front-end in V1.1 (no network port), or checking the owner of the peer socket on macOS |
| Mixed bytes from a variant re-rendered while read | Retired in 5: every range's ETag is checked, and a read that could mix contents fails with `EIO` | — |
| Node id collisions | Retired in 5: detected where ids are handed out and salted, salts kept in the index | — |
| Stale index after foreign writes | 1, 4: metadata and tags refresh lazily on `getxattr` and `resolve` (`--meta-ttl`); `tags/` follows only refreshed files. 5: `ucfs index refresh <uuid>`, and deleted groups leave `groups/` | 30 s sync, `ucfs index sync --full` |

## 7. Strategic notes

**Do Phase 0 spikes before writing the VFS.** Spike A decides the macOS story and Spike D decides the naming story;
both are cheap and both can change code shape.

**Build the NFS front-end first, FUSE second.** NFS is the one that runs on the development machine and the one with
the awkward semantics; FUSE is the easy port.

**Keep the core front-end-agnostic.** Every operation lives in `internal/vfs` and is tested against the fakes without
a mount; front-ends are thin adapters. That is also what keeps Windows and FUSE-T possible later without touching the
core.

**Measure traffic from day one.** The byte counter in Phase 1 is what makes the Finder findings, the prefetch tuning
and the budget feature possible; it is not a polish item.
