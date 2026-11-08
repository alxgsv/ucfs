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
| macOS front-end | Loopback NFSv3 server (`github.com/willscott/go-nfs` at master `c092ae79c64e` for `Server.ConcurrentHandlers`, which v0.0.4 lacks; carried in `third_party/go-nfs` since Phase 2 with a patch that passes errnos through as NFS statuses, and since Phase 3 one that passes COMMIT to the filesystem) mounted with the built-in `mount_nfs` as the user, no `sudo` (Spike A) | Zero install, no kext, works on managed Macs. Proven by rclone `nfsmount` |
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
scripts/install.sh             installer for release archives (Phase 6)
contrib/                       launchd and systemd examples (Phase 6)
Makefile  .goreleaser.yaml  .github/workflows/{ci,release}.yml  CLAUDE.md  AGENTS.md
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
    its start. Up to 8 uploads run at once (`--upload-concurrency`). Names containing `\` fail with `EINVAL`, since
    base upload keeps only what follows the last `\` (Spike D), until the `ucfs:name` fallback exists. Errors:
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
    one metadata `PUT`; a directory rename is one `PUT` per file below it, with a warning above 1000. Files that share
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
    `GET /groups/` from a cursor with every sweep (`--sweep-interval`, 10 min) when the view is mounted; a group's
    members come from `GET /groups/{id}/` on its first listing (uploadcare-go's `group.Info` has no files, so the client
    sends that request itself) and are kept, since groups never change; a group id not synced yet is read on lookup, and
    a 404 drops it. A deleted group whose members were already read stays listed: the API lists no deleted groups. View nodes have NFS handles like variants: directories and `tags/`/`groups/` files by path through
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

33. Traffic budget `--traffic-budget` (bytes per day); reads beyond it fail with `EDQUOT` and `ucfs status` says so.
34. Error → errno table applied everywhere: 404 `ENOENT`, 401/403 `EACCES`, 429 after retries `EAGAIN`, 5xx/network
    `EIO`, unsupported `EOPNOTSUPP`/`EROFS`, bad variant `EINVAL` (done in Phase 2; NFS carries errnos through the
    patched go-nfs, and NFSv3 has no status for `EAGAIN`, so 429 needs `NFS3ERR_JUKEBOX` there). Check the ETag of every
    variant 206 against the recorded one and fail the read on a mismatch (§4.4).
35. Backpressure: bounded in-flight CDN requests per mount (default 32) with a fair queue, so a bulk `rsync` cannot
    starve interactive `stat`/`readdir`.
36. Security defaults: `nosuid,nodev`, no `allow_other`, config file permission check, secrets never logged, cache dir
    `0700`.
37. Benchmarks in CI: cold `cat` 5 MB, `ls -l` 10 000 entries, two concurrent variant readers (p95 of the second
    reader unchanged by the first).
38. Soak: 24 h mount with Finder open and a sync loop running against the test project; no growth in goroutines,
    file descriptors or cache beyond the configured size.

### Phase 6: Packaging and docs (≈ 3 days)

39. Where the public repository lives is the owner's decision, prepared in [docs/release.md](docs/release.md):
    `alxgsv/ucfs` with the tap `alxgsv/homebrew-tap` (formula `alxgsv/tap/ucfs`), which everything is configured for,
    or `uploadcare/ucfs`, a module path change in `go.mod` and 47 Go files plus the places the doc lists; the licence
    (none yet; uploadcare-cli's MIT is the natural default); unsigned, unnotarized binaries; and the steps once
    decided: make the repository public, create the tap, add `HOMEBREW_TAP_TOKEN`, fix the Actions billing, push
    `v0.1.0`. None of them is done.
40. `.goreleaser.yaml` (goreleaser v2): `CGO_ENABLED=0` builds for darwin and linux on amd64 and arm64 with
    `-trimpath`, version, commit and date in `ucfs version`; `ucfs_<version>_<os>_<arch>.tar.gz` archives with the
    README and `contrib/`; `checksums.txt`; a Homebrew formula with a `test do` (`ucfs version`), caveats and
    `contrib/` in `pkgshare`, pushed to the tap with `HOMEBREW_TAP_TOKEN` and skipped for pre-releases. It uses the
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
    it.
42. `README.md` and `docs/`: [mount-options.md](docs/mount-options.md) (every command and flag, the `status --json`
    fields, and the Phase 5 additions marked), [finder.md](docs/finder.md), [costs.md](docs/costs.md),
    [semantics.md](docs/semantics.md) (results and errnos per operation, place and front-end, from the code, with
    those seen on a mount marked), [troubleshooting.md](docs/troubleshooting.md), [release.md](docs/release.md).
43. `AGENTS.md`: build, test and lint, the mount tests and live checks, test uploads marked `ucfs:test=1`, leaving no
    mount or registry record behind, secrets, commit signing and one PR per phase; `CLAUDE.md` imports it.

Result: `brew install alxgsv/tap/ucfs && ucfs mount ~/uc` on a fresh Mac once the release exists (tap name follows
step 39). Measured in [docs/phase6.md](docs/phase6.md): `goreleaser release --snapshot` builds the four archives
(5.3–5.9 MB); `install.sh` installs from a local server on macOS and on Linux (dash); the generated formula installs
through a local tap and passes `brew test`; `ucfs mount --daemon` returns in 1.0 s with the mount up, and
`ls -lR` through it costs 0 CDN bytes; the launchd agent and the systemd unit mount, stay down after `ucfs umount`,
restart after a failure, and leave nothing behind.

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
sync_state(key TEXT PRIMARY KEY, value TEXT);  -- list cursor, last full scan, last removed sweep, groups cursor
```

### 4.2 Sync algorithm

- Full scan: `GET /files/?ordering=datetime_uploaded&limit=1000&from=<cursor>` until exhausted; upsert; cursor =
  last `datetime_uploaded` (re-fetch of equal timestamps is deduplicated by uuid).
- Incremental: same call from the cursor every `--sync-interval` (default 30 s).
- Removals: `removed=true` listing every 10 min marks `removed`; a CDN 404 on read does the same immediately.
- Metadata and tags changed by other clients are not visible in listings: refreshed lazily on `getxattr` and
  `ucfs resolve` when older than `--meta-ttl` (5 min), and by `ucfs index sync --full`; `ucfs index refresh <uuid>`
  was not built. So `tags/` shows a tag set elsewhere on an already listed file only after such a refresh; tags set
  through the mount (`user.uc.tags`, uploads) show at once.
- Groups: `GET /groups/?ordering=datetime_created&from=<cursor>` with every removed sweep (10 min) when `groups/` is
  mounted; members on first listing of a group.
- Search API is used only by `ucfs resolve --search` and by tree repair, never on the request path.

### 4.3 Node ids and NFS file handles

`NodeID` is a 64-bit FNV-1a hash of `(view, uuid, ops-or-path)`; collisions are detected at insert and resolved by
rehashing with a salt stored in the index (deferred to Phase 5: Phase 1 uses the plain hash, about 3·10⁻⁸ collision
chance at 1 M files). NFS file handles are self-describing (`version:1 byte, kind:1 byte, reserved:2 bytes,
uuid:16 bytes, hash:8 bytes`), so a restarted daemon serves old handles without a path cache and never returns
`ESTALE` for files that still exist. Handles must be a multiple of 4 bytes long: go-nfs reads them with
`xdr.ReadOpaque`, which skips no XDR padding, and a 26-byte handle made every ACCESS reply deny everything.
A variants view node (`-`, an operation, an argument, the variant) hashes its operations path, such as
`-/resize/1200x/`, which does not fit in the handle. The first handle handed out for such a node records
`(id, uuid, ops)` in `node_ops` (`INSERT OR IGNORE` behind an in-memory set), and decoding looks the path up by id,
so these handles also survive a restart; an id never handed out is `ESTALE`.

### 4.4 Read path

Open → sequential detector per object (streams per reader) → block cache (`<cache>/blocks/<uuid>/<blockno>` for
originals, `<cache>/blocks/<uuid>~<hash(ops, etag)>/<blockno>` for variants) → singleflight → ranged `GET`. Prefetch
window starts at 1 MiB and doubles while reads stay sequential, up to 16 MiB, bounded by the traffic budget. Originals
never revalidate. A variant's HEAD answer expires after 24 h; the next access sends a new `HEAD`, and a changed size or
ETag drops the old blocks. A variant the CDN re-renders with other bytes inside those 24 h could mix blocks of both
renderings; comparing the ETag of every 206 with the recorded one would catch it (Phase 5, item 34).

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
wsize=1048576,readahead=0,nfc,soft,intr,acregmax=3600,acdirmax=5,nobrowse[,rdonly] 127.0.0.1:/ <dir>`. Without
`rsize`/`wsize` the client negotiates 32 KiB. `readahead=0` is a traffic choice: at 1 MiB blocks the client's default
read-ahead turns a 4 KiB read into 16 MiB of traffic. Re-measured in Phase 1 with the daemon's concurrent prefetch:
cold `cat` of 16 MB takes the same 1.4–1.5 s at `readahead` 0 and 2, while `head -c 4096` costs 1 MiB at 0 and 2 MiB
at 2, so 0 stays the default and `--readahead` overrides it. `127.0.0.1`, not `localhost`, which can
resolve to `::1`. `nobrowse` keeps the mount out of the Finder sidebar; `rdonly` with `--read-only`. Linux FUSE:
`fsname=ucfs`, `noatime`, `max_read=1048576`, no `allow_other`, `ro` with `--read-only`.

## 5. Testing strategy

- Unit tests for `vfs`, `cache`, `spool`, `index` against `internal/fake` (REST with cursor pagination, metadata,
  storage, search; Upload with base and multipart; CDN with deterministic bytes, `Range` and `HEAD` for originals and
  variants).
- Front-end tests: Linux FUSE mounted in CI (ubuntu runner with `fuse3`); macOS NFS mounted on a macOS runner as
  the user (Spike A showed non-root mounting works).
- Live tests gated by `UCFS_LIVE=1` against `uploadcare-go-test`: upload, read back, metadata round trip, delete.
- Golden files for `ucfs status --json` and `ucfs resolve --json`.
- Benchmarks from Phase 5 run on every PR and compared against the previous run.

## 6. Risks and where they are retired

| Risk | Phase that answers it | Fallback |
|---|---|---|
| Non-root NFS mount on macOS | Retired in 0 (Spike A): works as the user | — |
| Unpublished rate limits during full scans | Open after 0 (Spike E ran on a 33-file project; one request, no 429) | adaptive page delay, resume from cursor (implemented) |
| `original_filename` mangling | Retired in 0 (Spike D) except names with `\` (base upload strips through the last `\` or `/`) | store the written name in `ucfs:name` metadata and display it; `EINVAL` until then |
| Finder/Spotlight traffic | 0 (Spike C), 3, 5. Spotlight is off on NFS mounts by default; a Finder window read every visible test image in full on first visit (JPEGs without an embedded thumbnail, also with `readahead=0`; Quick Look caches thumbnails across mounts, docs/phase1.md), and `.ql_disablethumbnails` does not stop it | `nobrowse`, `rdonly`, `readahead=0`, shadow files, traffic counter and budget |
| NFS has no close event | 3: macOS sends COMMIT, or a single stable WRITE for a small file, right after the data once go-nfs answers WRITE with `UNSTABLE`; the upload starts 300 ms later, and upload errors cannot reach the writer | 3 s idle commit; FUSE-T front-end in V1.1 for strict semantics |
| No xattrs over NFSv3 on macOS | 4: `ucfs resolve [--json]` prints metadata and tags, refreshed after `--meta-ttl` | the CLI for writes; xattrs on macOS with the FUSE-T front-end in V1.1 |
| go-nfs maturity ("minimally tested") | 1, 2, 5 (soak) | fork and fix, taken in Phase 2: `third_party/go-nfs` passes errnos through (LOOKUP could only fail with `NOENT`); offer the patch upstream. rclone uses the same library in production |
| Stale index after foreign writes | 1, 4: metadata and tags refresh lazily on `getxattr` and `resolve` (`--meta-ttl`); `tags/` follows only refreshed files | 30 s sync, `ucfs index sync --full` |

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
