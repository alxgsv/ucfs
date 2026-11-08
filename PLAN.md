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
| macOS front-end | Loopback NFSv3 server (`github.com/willscott/go-nfs` at master `c092ae79c64e` for `Server.ConcurrentHandlers`, which v0.0.4 lacks; carried in `third_party/go-nfs` since Phase 2 with a patch that passes errnos through as NFS statuses) mounted with the built-in `mount_nfs` as the user, no `sudo` (Spike A) | Zero install, no kext, works on managed Macs. Proven by rclone `nfsmount` |
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
internal/spool/                write spool, commit (upload), overwrite policy, alias table, upload journal
internal/vfs/                  platform-neutral filesystem model: node ids, attrs, lookup/readdir/open/read/write/flush, xattrs, local-only shadow files
internal/vfs/views/            files, variants, tree, bydate, tags, groups (one file each; files and variants are vfs.go and variants.go in internal/vfs so far)
internal/frontend/nfs/         go-nfs Handler (stable file handles), billy.Filesystem adapter, mount_nfs/umount driver (darwin, linux)
internal/frontend/fuse/        go-fuse adapter (linux build tag)
internal/ctl/                  unix-socket control API used by `ucfs status`
internal/fake/                 httptest doubles for REST, Upload API and CDN used by tests
third_party/go-nfs/            go-nfs at the pinned commit with the errno patch (README.ucfs.md), wired in by a go.mod replace
testdata/                      golden files, fixtures
docs/                          mount options, troubleshooting, Finder notes
Makefile  .goreleaser.yaml  .github/workflows/ci.yml  CLAUDE.md  AGENTS.md
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
15. `ucfs mount <dir> --read-only` (default on in this phase; on macOS it adds `rdonly`, which stops Finder's `.DS_Store` writes and SETATTRs, Spike C), `ucfs umount <dir>`, `ucfs status` (counters over the control socket).
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

21. `internal/spool/`: spool files under the cache dir; `Create`/`WriteAt`/`Truncate(0)`, and truncate to the current
    length as a no-op (macOS `cp` issues one after every write, Spike C); commit = upload with
    `UPLOADCARE_STORE=1`, part filename = written name (`original_filename`), base upload ≤ 100 MB, multipart above.
    Base upload keeps only what follows the last `\` in a name (Spike D), so names containing `\` fail with `EINVAL`
    until the `ucfs:name` fallback exists. Projects can restrict upload file types: `uploadcare-go-test` rejects a
    random binary with "Uploading of these file types is not allowed" (validation error), which maps to `EPERM`.
22. Commit triggers: FUSE `Flush` (blocking, error returned to the writer); NFS `COMMIT` or 3 s idle after the last
    `WRITE`. Zero-length files stay local until written or removed (documented limitation; the API itself accepts
    empty files, Spike D).
23. `views/tree`: `tree/<path>/<name>` from metadata key `ucfs:path`; `mkdir`/`rmdir` via `tree_dirs`; `rename` of an
    uploaded file = one metadata `PUT`; directory rename = N metadata updates with a warning above 1000 entries; files
    without `ucfs:path` appear under `tree/_inbox/`.
24. Overwrite policy `--on-overwrite=replace|keep` (default `replace`: upload new, copy metadata and tags, soft-delete
    old, update index and aliases).
25. `unlink` → `DELETE /files/{uuid}/storage/`; aliases in `files/` for written names with TTL; `.ucfs/uploads.log`
    journal (time, path, uuid, size, mime).
26. Local-only shadow files: `.DS_Store`, `._*`, `.Spotlight-V100`, `.fseventsd`, `.Trashes`, `.TemporaryItems` are
    stored in the cache dir and never uploaded; `.metadata_never_index` always exists at the root.
27. Crash safety: spools that were being uploaded are retried on the next `ucfs mount`; `ucfs spool list|retry|drop`.

Acceptance: `cp photo.jpg tree/photos/` leaves `tree/photos/photo.jpg` with `original_filename` `photo.jpg` and a
journal line; saving from TextEdit and from `vim` (temp file + rename) works in `tree/`; `rm` soft-deletes; Finder
copy of a folder with 50 images uploads 50 files and zero sidecars; a killed daemon mid-upload resumes on restart.

### Phase 4: Metadata, tags, groups, dates (≈ 3 days)

28. xattrs on Linux FUSE: read-only `user.uc.uuid`, `.cdn_url`, `.mime`, `.md5`, `.width`, `.height`, `.stored`,
    `.tags`; read-write `user.uc.meta.<key>` → metadata API (50-key, 64/512-char limits mapped to `E2BIG`/`ENOSPC`).
29. `views/tags` (`tags/<tag>/<uuid>_<name>`), `views/groups` (`groups/<group-uuid>/<index>_<name>`),
    `views/bydate` (`by-date/YYYY/MM/DD/<uuid>_<name>`); group sync via `GET /groups/` cursor.
30. `ucfs resolve <path>` prints uuid, CDN URL and metadata for any path on the mount (via the control socket).
31. Lazy metadata refresh: `getxattr` re-fetches file info when the index entry is older than 5 min.
32. `--views` flag selects which views are mounted (default `files,tree,by-date`; `tags` and `groups` opt-in).

Acceptance: `setfattr -n user.uc.meta.camera -v Canon tree/x.jpg` is visible in the dashboard; `tags/approved/` lists
exactly the search result for `tags.all=[approved]`.

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

39. Decide where the public repo lives (`alxgsv/ucfs` or the `uploadcare` org), make it public, and name the Homebrew tap to match.
40. `.goreleaser.yaml`: darwin arm64/amd64, linux amd64/arm64; Homebrew tap formula; `install.sh` in the style of the CLI.
41. launchd plist and systemd unit examples for persistent mounts; `ucfs mount --daemon`.
42. `docs/`: mount options, Finder and Spotlight notes, cost guidance (what pulls CDN traffic), semantics table
    (what works, what returns `EOPNOTSUPP`), troubleshooting.
43. `CLAUDE.md`/`AGENTS.md` with build, test and live-test instructions.

Result: `brew install alxgsv/tap/ucfs && ucfs mount ~/uc` on a fresh Mac (tap name follows step 39).

## 4. Cross-cutting specifications

### 4.1 Index schema

```sql
files     (uuid TEXT PRIMARY KEY, name TEXT, size INTEGER, mime TEXT, is_image INTEGER, is_ready INTEGER,
           stored INTEGER, removed INTEGER, uploaded_at INTEGER, stored_at INTEGER, removed_at INTEGER,
           md5 TEXT, width INTEGER, height INTEGER, metadata TEXT /*json*/, tags TEXT /*json*/,
           tree_path TEXT, info_refreshed_at INTEGER);
           -- indexes: uploaded_at, name, tree_path, removed
variants  (uuid TEXT, ops TEXT, size INTEGER, mime TEXT, etag TEXT, checked_at INTEGER, changed_at INTEGER,
           PRIMARY KEY (uuid, ops));                -- changed_at: first HEAD with this size and ETag (Phase 2)
node_ops  (id INTEGER PRIMARY KEY, uuid TEXT, ops TEXT);  -- operations path behind a variants view node id (Phase 2)
groups    (uuid TEXT PRIMARY KEY, created_at INTEGER, files TEXT /*json array of uuids*/);
aliases   (view TEXT, name TEXT, uuid TEXT, expires_at INTEGER, PRIMARY KEY (view, name));
tree_dirs (path TEXT PRIMARY KEY, created_at INTEGER);
shadow    (path TEXT PRIMARY KEY, blob_ref TEXT, size INTEGER, mtime INTEGER);
sync_state(key TEXT PRIMARY KEY, value TEXT);  -- list cursor, last full scan, last removed sweep, groups cursor
```

### 4.2 Sync algorithm

- Full scan: `GET /files/?ordering=datetime_uploaded&limit=1000&from=<cursor>` until exhausted; upsert; cursor =
  last `datetime_uploaded` (re-fetch of equal timestamps is deduplicated by uuid).
- Incremental: same call from the cursor every `--sync-interval` (default 30 s).
- Removals: `removed=true` listing every 10 min marks `removed`; a CDN 404 on read does the same immediately.
- Metadata and tags changed by other clients are not visible in listings: refreshed lazily on `getxattr` (5 min TTL)
  and by `ucfs index refresh <uuid>`.
- Groups: `GET /groups/?ordering=datetime_created&from=<cursor>` every 10 min.
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

Create → spool → writes → commit trigger (FUSE `Flush`, NFS `COMMIT`/idle) → upload → index upsert → alias and
journal → spool removal. Replacement of an existing name follows `--on-overwrite`. Upload failures return `EIO` on
`Flush`; on NFS they are logged and the spool is kept for `ucfs spool retry`.

### 4.6 Mount options (confirmed by Spike A)

macOS, run as the user: `mount_nfs -o port=P,mountport=P,vers=3,tcp,locallocks,noresvport,rsize=1048576,
wsize=1048576,readahead=0,nfc,soft,intr,acregmax=3600,acdirmax=5,nobrowse[,rdonly] 127.0.0.1:/ <dir>`. Without
`rsize`/`wsize` the client negotiates 32 KiB. `readahead=0` is a traffic choice: at 1 MiB blocks the client's default
read-ahead turns a 4 KiB read into 16 MiB of traffic. Re-measured in Phase 1 with the daemon's concurrent prefetch:
cold `cat` of 16 MB takes the same 1.4–1.5 s at `readahead` 0 and 2, while `head -c 4096` costs 1 MiB at 0 and 2 MiB
at 2, so 0 stays the default and `--readahead` overrides it. `127.0.0.1`, not `localhost`, which can
resolve to `::1`. `nobrowse` keeps the mount out of the Finder sidebar; `rdonly` while read-only. Linux FUSE:
`fsname=ucfs`, `noatime`, `max_read=1048576`, no `allow_other`.

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
| NFS has no close event | 3 | idle commit; FUSE-T front-end in V1.1 for strict semantics |
| No xattrs over NFSv3 on macOS | 4 | `ucfs resolve` and the CLI cover metadata on macOS until V1.1 |
| go-nfs maturity ("minimally tested") | 1, 2, 5 (soak) | fork and fix, taken in Phase 2: `third_party/go-nfs` passes errnos through (LOOKUP could only fail with `NOENT`); offer the patch upstream. rclone uses the same library in production |
| Stale index after foreign writes | 1, 4 | 30 s sync, lazy refresh, `ucfs index refresh` |

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
