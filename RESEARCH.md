# ucfs — Uploadcare Filesystem: feasibility research

Date: 2026-11-08. Implementation plan: [PLAN.md](PLAN.md). Scope: can an Uploadcare project be mounted as a local filesystem, and what would it be good for.
Facts marked **(verified)** were checked today against the OpenAPI specs in `uploadcare-api-references` and against the
`uploadcare-go-test` project with the installed `uploadcare` CLI and `curl`. Everything else is from public docs or prior knowledge and is marked where it matters.

## 1. Verdict

**Yes, it is possible, and the useful version is a *read-mostly, write-once* filesystem**, in the same family as
AWS Mountpoint for S3: full support for listing, metadata, random-access reads and streaming; create-whole-file writes
that become uploads on close; deletes; and a metadata-driven virtual directory tree. In-place edits, appends and true
renames are not possible because Uploadcare files are immutable and `original_filename` cannot be changed; they have to
be emulated (re-upload under a new UUID, or path kept in file metadata) or rejected with `EOPNOTSUPP`, exactly as
Mountpoint does.

Two things make this more than "yet another object-store mount":

1. **CDN transformations as paths.** The CDN answers `HEAD` with a real `Content-Length` and serves `Range` requests
   (HTTP 206) for *transformed* variants too **(verified)**. So `files/<uuid>/-/resize/1200x/-/format/webp/photo.webp`
   can be a lazily materialised, seekable, read-only file. No other storage mount offers an image pipeline in `cp`.
2. **File metadata ↔ extended attributes.** Uploadcare metadata (≤ 50 keys, key ≤ 64 chars, value ≤ 512 chars
   **(verified)**) maps one-to-one onto xattrs, and the new search API can resolve `exact metadata[key]=value`
   **(verified)**, which is what makes a writable virtual hierarchy stored in metadata feasible.

The main engineering risks are not API gaps but (a) the mount layer on macOS (no kexts; NFS loopback or FUSE-T or
FSKit), (b) CDN traffic bills caused by Spotlight, thumbnailers and `grep -r`, and (c) unpublished REST rate limits,
which force an index-first design, and (d) per-request latency, which section 7 addresses with prefetch and caching rather than being a blocking problem.

## 2. What Uploadcare gives us (verified against spec and live project)

| Capability | Endpoint / behaviour | Relevant limits |
|---|---|---|
| List files | `GET /files/` | `ordering` only `±datetime_uploaded`; cursor `from=<ISO8601>`; `limit` ≤ 1000; filters `stored`, `removed`; `include=appdata` |
| Search | `POST /files/search/` | Full text (≥ 4 chars) on `original_filename`, `metadata`, `detected_mime_type`; `exact` on `uuid`, `original_filename`, `detected_mime_type`, `metadata[<key>]`; `tags.any/all/none`; size and date ranges; `is_image`; `limit` ≤ 100, `offset+limit` ≤ 1000; index is async, excludes removed files |
| File info | `GET /files/{uuid}/` | `original_filename`, `size`, `mime_type`, `content_info` (image/video details), `metadata`, `tags` (when the tags feature is on), `variations`, `datetime_uploaded/stored/removed`, `is_ready` |
| Metadata | `GET/PUT/DELETE /files/{uuid}/metadata/{key}/` | ≤ 50 keys; key ≤ 64 chars matching `[\w\-\.\:]+`; value ≤ 512 chars |
| Tags | `/files/{uuid}/tags/` | per-file, normalised (lowercase, trimmed); project feature flag |
| Store / delete | `PUT/DELETE /files/{uuid}/storage/`, batch `PUT/DELETE /files/storage/` | ≤ 100 UUIDs per batch; delete is soft (`datetime_removed`, info stays, content gone) |
| Copy | `POST /files/local_copy/` | `source` = UUID **or CDN URL with transformations**; ≤ 100 MB with operators, ≤ 5 GB without; size/MIME arrive asynchronously |
| Groups | `GET /groups/`, `GET/DELETE /groups/{uuid}/` | membership immutable; created via Upload API |
| Webhooks | `file.uploaded`, `file.infected`, `file.stored`, `file.deleted`, `file.info_updated` | needs a public HTTPS endpoint |
| Upload | `POST /base/` (multipart form, ≤ 100 MB, `metadata[k]=v`, `UPLOADCARE_STORE=auto|0|1`); multipart `/start`→S3 parts→`/complete` for > 10 MB (mandatory > 100 MB), parts ≥ 5 MiB; `/from_url/` | the multipart part filename becomes `original_filename`; `from_url` sanitises filenames to `[A-Za-z0-9._]` |
| Project | `GET /project/` | `autostore_enabled` decides whether uploads need an explicit store |
| Rate limits | not published | 429 + `Retry-After`; `uploadcare-go` has opt-in `RetryConfig` with backoff |

CDN, measured on `55317o72r5.ucarecd.net` (per-project CDN host, the CLI derives it from the public key) and on
`ucarecdn.com`:

| Request | Result |
|---|---|
| `HEAD <uuid>/` | 200, `Content-Length`, `Accept-Ranges: bytes`, strong `ETag` equal to the content MD5, `Last-Modified`, `cache-control: public, max-age=31556926, immutable` |
| `GET <uuid>/` with `Range: bytes=0-9` | 206, `Content-Range: bytes 0-9/70` |
| `HEAD <uuid>/-/resize/50x/` | 200, `Content-Length: 85`, `Content-Type: image/png` (variant generated on demand) |
| `GET <uuid>/-/preview/100x100/-/format/webp/` with `Range` | 206, weak `ETag` |
| SVG with `-/format/png/-/resize/64x/` | 400 (SVG processing is a project-level setting) |

Takeaway: `stat()` on a variant costs one `HEAD`; reads are seekable everywhere; originals are immutable so a block
cache never needs revalidation. The strong ETag on originals is the content MD5 (checked on one file), so hash-based dedupe and
`rsync --checksum`-style comparisons cost nothing.

## 3. Filesystem operations mapped onto Uploadcare

| POSIX op | Implementation | Status |
|---|---|---|
| `readdir`, `stat` | local SQLite index built from `GET /files/` (1000/page, cursor `from`) | native |
| `open(O_RDONLY)`, `read`, `pread`, `mmap` | CDN `GET` with `Range` through an on-disk block cache | native |
| `create` + sequential `write` + `close`/`fsync` | spool to a temp file; on release upload via `/base/` (≤ 100 MB) or multipart; `UPLOADCARE_STORE=1` unless `autostore_enabled` | native, write-once |
| overwrite existing file | upload new UUID, copy metadata and tags, then soft-delete the old UUID (configurable: keep both) | emulated |
| `append`, random `write`, `truncate` to non-zero | impossible on immutable objects; either full download → edit → re-upload (opt-in) or `EOPNOTSUPP` | emulated / rejected |
| `unlink` | `DELETE /files/{uuid}/storage/` | native (soft delete) |
| `rename` | `original_filename` is immutable. In the writable `tree/` view the path lives in metadata (`ucfs:path`), so rename = one `PUT metadata`. In UUID-keyed views: rejected | emulated |
| `mkdir`, `rmdir` | virtual; directories exist while some file's `ucfs:path` has that prefix (plus an in-index placeholder for empty dirs) | emulated |
| `getxattr/setxattr/listxattr/removexattr` | `user.uc.meta.<key>` ↔ metadata; read-only `user.uc.uuid`, `.mime`, `.width`, `.height`, `.tags`, `.cdn_url`, `.stored` | native |
| `chmod`, `chown`, `utimes` | accepted and ignored (or mirrored into metadata) | ignored |
| hard links, symlinks, locks | none; locks are process-local | rejected |
| `statfs` | fake block counts; optionally project usage from the Project API | fake |

A subtle but important point for the write path: most editors and all office suites save by **writing a temp file and
renaming it over the original**. The writable view therefore must support rename of a *not yet uploaded* file (trivial:
rename the spool) and rename of an uploaded file (metadata update). Without that, "drag a file into the mount" works but
"save from an app" does not.

## 4. Namespace: turning a flat UUID space into a tree

Uploadcare has no directories; `original_filename` is free text (and sanitised on some paths), so paths must not be
encoded in filenames. Proposed read-only *views* plus one writable *tree*:

```
<mount>/
  files/<uuid>_<original_filename>            canonical, unique, readable (optionally files/<uuid>/<name>)
  files/<uuid>/-/<op>/<args>/.../<name>       on-the-fly CDN variants, read-only, size via HEAD, lazily cached
  by-name/<original_filename>                 collisions get a "~<uuid8>" suffix
  by-date/YYYY/MM/DD/<uuid>_<name>            from datetime_uploaded
  tags/<tag>/<uuid>_<name>                    when the tags feature is enabled
  groups/<group-uuid>/<index>_<name>          immutable collections
  tree/<any>/<path>/<name>                    WRITABLE: hierarchy stored in metadata ucfs:path="/any/path/name"
  .unstored/ .removed/                        optional diagnostic views (removed files expose info only)
  .ucfs/uploads.log                           journal: one line per completed upload (time, path, uuid, size)
```

`tree/` is the interesting one. Each file carries `ucfs:path`; the local index answers `readdir`; the search API's
`exact metadata[ucfs:path]` and `phrase metadata` can bootstrap or repair the index, with the caveat that the search
index lags writes, so the local index stays authoritative. Files uploaded by other clients (web uploader, SDKs) have no
`ucfs:path` and appear under `tree/_inbox/` until moved.

Operation grammar for variants should be validated against the URL API operation list (the CLI already ships a
`url-api` reference) so typos return `ENOENT` instead of a 400 from the CDN.

### 4.1 Write path: the name you wrote versus the UUID you got

POSIX gives a filesystem no way to answer a `create` with a different name. `cp` opens the destination path, writes
into the descriptor, sets mode and times on the descriptor, closes it and never asks for the name again. The UUID exists
only when the upload completes, at close on FUSE or when the idle timer fires on NFS. So the daemon cannot redirect the
copy, but it decides which names exist afterwards, and that differs per view:

| Write target | After `cp photo.jpg <view>/` | Notes |
|---|---|---|
| `tree/` (recommended) | `tree/photo.jpg` stays. Server `original_filename` = `photo.jpg`, `ucfs:path` metadata set, UUID in `user.uc.uuid` | Same model rclone uses for Google Drive, which also assigns server IDs. Overwriting the name = new UUID, previous one soft-deleted by default |
| `by-name/` | Written name stays for the newcomer; an older file with the same name is listed with a `~<uuid8>` suffix from then on | A convenience view; its collision rule is why `tree/` is the recommended write target |
| `files/` | `files/photo.jpg` becomes an alias of `files/<uuid>_photo.jpg` for a TTL (until the next listing plus a grace period). `ls` shows the canonical name, both paths resolve | Keeps `cp x dest && use dest` working |
| `files/` with symlink handoff (option) | The written entry is replaced by a symlink to the canonical name, so `ls -l` shows `photo.jpg -> <uuid>_photo.jpg` | Mapping is visible without tooling. A repeat `cp` follows the link and overwrites, which means another new UUID |

Rejected as default: a hard rename after upload using FUSE `notify_inval_entry`. The copying process is unaffected,
because its descriptor is bound to the inode and not the name, but every later reference to the written path fails. Opt-in
at most.

Handing the UUID back to the caller, since none of the above prints it:

- `user.uc.uuid` xattr on the written path. Empty until the upload completes; over NFS that is a few seconds after `cp`
  returns.
- `.ucfs/uploads.log`, one line per completed upload: time, path, UUID, size, MIME type. The natural hook for CI steps
  and agents.
- `ucfs resolve <path>` as a CLI helper that reads the xattr or the index.

Two write-path details that follow from this. Finder and `cp` on macOS use `copyfile(3)`, which over NFSv3 writes
AppleDouble `._photo.jpg` sidecars for xattrs and resource forks; the daemon must absorb those locally instead of
uploading them. And a second write to an existing name is an overwrite on an immutable store: `tree/` treats it like
any filesystem overwrite (new UUID, old one soft-deleted, metadata and tags copied), while `by-name/` keeps both.

## 5. Mount layer options

| Platform | Option | Install burden | Notes |
|---|---|---|---|
| Linux | FUSE 3 (`libfuse`, Go: `cgofuse`/`go-fuse`, Rust: `fuser`) | none (kernel built in) | best semantics: open/release lifecycle, xattrs, `notify`. CI and containers friendly; CSI-style driver possible later |
| Linux | loopback NFSv3 (`willscott/go-nfs`) | none, but `mount` needs root | same engine as macOS path; fine for servers |
| macOS | **loopback NFS via built-in client** (`mount_nfs`) | none | what rclone `nfsmount`, FUSE-T and XetHub do. `go-nfs` is NFSv3 (read/write, "minimally tested", no hard links); no xattr op in v3, so macOS falls back to `._` AppleDouble sidecars; NFS is stateless, so "upload on close" becomes "upload on COMMIT or idle timeout". rclone marks its NFS mount read-only without a VFS cache for this reason |
| macOS | **FUSE-T** (kext-less FUSE over NFSv4 / SMB3 / FSKit) | user installs a .pkg | gives a real FUSE API with xattrs and release callbacks; `cgofuse` has a fuse-t fork; a FUSE-based ucfs gets macOS "for free" |
| macOS | macFUSE | kext → Reduced Security on Apple Silicon | non-starter on managed Macs; it now has an opt-in FSKit backend (`-obackend=fskit`) that its maintainers call not yet on par with the kernel VFS |
| macOS | **FSKit** (native user-space FS, macOS 15.4+) | app bundle with signed extension | Apple's sanctioned direction, `FSUnaryFileSystem` only, Swift/ObjC, Mac App Store compatible; too immature and heavyweight for a PoC, right target for a product |
| macOS | File Provider extension | signed app | the Dropbox/Drive/OneDrive model: `~/Library/CloudStorage/Uploadcare`, on-demand hydration, Finder badges. Sync semantics rather than a mount; best end-user UX, biggest build |
| Windows | WinFsp (via `cgofuse`) | user installs WinFsp | mature, what rclone uses. NFS client exists only on Pro/Enterprise |
| any | `ucfs serve webdav` + OS WebDAV client | none | zero-install fallback; macOS WebDAV client is slow, Windows caps file size by default, no xattrs |

This Mac (macOS 27.0.1) has no FUSE of any kind installed, has `/sbin/mount_nfs`, Go 1.26.1, and FSKit modules only
from Apple, which is representative of a developer laptop: the zero-install path is NFS loopback.

**Recommendation:** one engine (index, cache, API client) with two front-ends. Front-end A: FUSE through `cgofuse`
(Linux native, macOS via FUSE-T, Windows via WinFsp). Front-end B: loopback NFSv3 through `go-nfs` for zero-install macOS
and servers. rclone proves this exact pairing works (`rclone mount` = cgofuse, `rclone nfsmount`/`serve nfs` = go-nfs).

## 6. Architecture sketch

```
            ┌─────────────────────────── ucfsd (Go) ───────────────────────────┐
 FUSE/NFS ──┤ VFS layer: views (files/, tree/, tags/, -/variants), xattr map   │
            │ Index: SQLite (uuid, name, size, mime, dates, metadata, tags,    │
            │        stored/removed, content_info, ucfs:path)                   │
            │ Sync: full scan GET /files/?limit=1000 ascending; incremental     │
            │       from=<last seen>; removed=true sweep; optional webhooks     │
            │ Reader: HEAD for variant sizes; GET Range → block cache (LRU,     │
            │         bounded, immutable so no revalidation)                    │
            │ Writer: spool → /base/ or multipart → index insert → optional     │
            │         delete of replaced UUID; retries with Retry-After         │
            │ Auth: UPLOADCARE_PUBLIC_KEY/SECRET_KEY or ~/.uploadcare/config.yaml│
            └───────────────────────────────────────────────────────────────────┘
```

Reuse: `uploadcare-go` (REST, Upload, metadata, tags, search clients, throttle retry) and `uploadcare-cli` (config
loading, CDN base derivation, URL API reference). A natural home for the finished tool is `uploadcare fs mount <dir>`
inside the CLI, or a separate `ucfs` binary that imports the same packages.

Sizing: a project with 1 M files needs ~1000 list requests for the initial scan (minutes), and the SQLite index is a few
hundred MB at most. Steady state is one list call per sync interval plus HEADs for variants on first `stat`.

## 7. Concurrency and latency

Processes on the mount do not block each other by design. FUSE and the NFS clients hand the daemon independent
requests per process, and a Go daemon serves each in its own goroutine. Serialisation appears only at specific choke
points; the table lists each with what the implementation must do about it.

| Layer | Default behaviour | Required handling |
|---|---|---|
| go-nfs server | Handles the requests of one connection sequentially; the library's own comment warns that a single slow operation stalls the whole mount because every process shares one TCP connection | Set `Server.ConcurrentHandlers` to ~128. Replies carry the request XID, so out-of-order completion is legal |
| macOS NFS client (measured on this Mac) | `nfsiod_thread_max` 16 async I/O threads, `max_async_writes` 128, `iosize` 1 MiB; every process also issues its own synchronous reads | Nothing; enough parallelism for Finder plus several shells. Tune `acregmin/acregmax` for attribute caching |
| FUSE (libfuse, cgofuse, go-fuse) | Multithreaded loop by default; the kernel sends each process's requests independently | Never pass `-s`. Raise `max_read`/`max_pages` toward 1 MiB. Long `attr_timeout`/`entry_timeout` for immutable `files/` entries, short for directories |
| Linux VFS | Writes to the *same* file serialise on the inode lock; reads and other files do not | Accept; identical to local disks |
| Block cache | A global mutex would serialise everyone | singleflight keyed by (uuid, operation chain, block index): identical reads coalesce, everything else runs in parallel |
| SQLite index | One writer at a time | WAL mode; request path only reads; index writes happen on the sync goroutine |
| Go HTTP transport | `MaxIdleConnsPerHost` = 2 | Raise it; the CDN speaks HTTP/2 so streams multiplex on few connections |
| Upload on close | `flush` blocks until the upload completes | Only the closing process waits, and it gets the error. Over NFS there is no close: an idle timer triggers the upload, nothing blocks, errors cannot reach the writer |

Different variants of different files are the easy case. Each variant is its own CDN URL and its own HTTP stream, so
a process waiting for the CDN to render a large resize never delays a process reading an original. The waiting process
pays the generation time once, on its first `stat` or read of that variant, because the daemon has to `HEAD` the URL to
learn the size. Cache variant sizes in the index and optionally pre-warm variants that a view exposes (for example the
`-/preview/` thumbnails of a directory being listed).

The real slowness is round trips per process, not cross-process blocking:

| Scenario (50 ms CDN RTT) | Naive implementation | With prefetch and attribute caching |
|---|---|---|
| `cat` of a 5 MB original | ~40 sequential 128 KiB reads ≈ 2 s | 1 round trip + transfer time |
| `ls -l` of 10 000 entries | 10 000 `stat` calls into the daemon | served from the index; repeats answered by the kernel's attribute cache |
| first `stat` of a new variant | one `HEAD`, waits for CDN processing | same on first touch; cached afterwards |
| second read of anything | network again | block cache hit |

Design rules that follow:

- Sequential-read detection with adaptive prefetch (the Mountpoint and rclone VFS approach): start with a 1 MiB range,
  grow while the reader stays sequential, stop at the per-mount traffic budget.
- Block cache on disk, bounded, keyed by (uuid, operation chain, block); originals are immutable so cached blocks never
  need revalidation; variants carry a weak ETag and get a TTL.
- Kernel-side caching of attributes and directory entries with long timeouts for immutable entries; invalidate from the
  sync goroutine when the index changes (FUSE `notify_inval_entry`, or short `acdirmax` on NFS).
- Per-request goroutines everywhere; no worker loop may hold a lock that `readdir` or `lookup` needs.
- Prefer FUSE (FUSE-T on macOS) for the writable product: it has a close event, so upload errors reach the writer.
  NFS loopback stays the zero-install read-mostly path.

Expected steady state after these rules: listings and `stat` at local speed from the index, cold reads at one CDN round
trip plus bandwidth, first touch of a new variant at CDN processing time, and no cross-process stalls.

## 8. Costs and risks

- **CDN traffic is billable and the OS is greedy.** Spotlight (`mdworker`), Finder Quick Look, antivirus, IDE indexers
  and `grep -r` will download everything they can see. Mitigations: mount with `nobrowse`/`noappledouble`, drop a
  `.metadata_never_index` marker, default to read-only, expose previews as `-/preview/` variants in Finder-facing views,
  bounded block cache, per-mount traffic budget with an `ERANGE`-style refusal above it, and a `--dry-traffic` counter in
  the PoC so we can see what each tool pulls.
- **Rate limits are unpublished.** Every `stat` must be served from the index, never from the API. Backoff on 429.
- **Deletes and foreign writes are eventually visible.** Deletion detection needs a periodic `removed=true` sweep (there is no
  "removed since" cursor) or webhooks. Search must not be used for read-after-write.
- **Immutability surprises users.** Saving a file produces a new UUID, so CDN URLs change. Must be documented and visible
  (`user.uc.uuid` xattr, `files/` view).
- **Unstored uploads disappear after 24 h** (or sooner with a project `file_lifetime`); the daemon must store explicitly
  when `autostore_enabled` is false.
- **Secrets.** The secret key sits on the laptop; the mount is visible to every local process. Default `allow_other` off;
  read-only mounts should work with a key restricted to read scopes once the platform offers them.
- **Authenticated (signed) CDN URLs.** Projects with secure delivery need token generation in the reader; not covered by
  `uploadcare-go` today.
- **Large projects on `by-name/`**: collisions and sort cost; make non-canonical views opt-in.
- **NFS-specific**: no close notification, so write completion relies on `COMMIT` plus an idle timer; NFSv3 has no xattr
  op. Acceptable for a read-mostly PoC, a reason to prefer FUSE-T/FUSE for the writable product.

## 9. How it can be useful (ranked)

1. **AI agents and coding assistants.** Claude Code, Cursor, shell agents already have a filesystem toolset. A mount lets
   them list, read, grep, transform and upload project files with zero API knowledge. This complements the Uploadcare MCP
   proposal (`UPLOADCARE_MCP_PRD.md`): MCP exposes intent-level tools; ucfs exposes everything that already speaks files.
2. **Bulk operations with standard tools.** `rsync`/`rclone` migrations in and out, `find -newer`, `du`, `exiftool`,
   `clamscan`, `file`, dedupe by hash, mass metadata edits via `xattr`/`setfattr` scripts, audits. Today each of these
   needs a custom script against the REST API.
3. **Transformations as a build-time pipeline.** `cp files/<uuid>/-/resize/1200x/-/format/webp/hero.webp public/` in a
   static-site build, ML preprocessing or asset packaging, with no SDK. `cp` of a variant *into* `files/` can map to
   `local_copy`, persisting a derivative as a real file.
4. **Designers and content teams.** Browse a project in Finder/Explorer, previews from `-/preview/` variants, drag-and-drop
   upload, tags as folders. Product-grade here eventually means File Provider or FSKit, but FUSE-T/NFS is enough to learn
   what they do.
5. **Backups and e-discovery.** Mount read-only and point `restic`, `borg` or Time Machine-like tools at it. An
   Uploadcare-to-cold-storage exporter without writing an exporter.
6. **CI/CD and containers.** FUSE on Linux runners to pull assets or publish build artifacts; later a CSI driver the way
   Mountpoint has one.
7. **Data science.** Image datasets consumed by `torchvision.datasets.ImageFolder`-style loaders by path; `-/resize/`
   variants give downsampled training sets for free; `Range` support makes video frames seekable.
8. **Internal support and operations.** Inspect a customer project (with consent) using ordinary tools; `content_info`
   as xattrs beats paging through JSON.
9. **Legacy integrations.** CMS plugins, DAM tools and media servers that only understand files (Range support makes
   streaming video through the mount realistic).

Anti-uses worth stating up front: databases, log files, anything that appends or edits in place, and anything that scans
the whole tree on a schedule.

## 10. Alternatives considered

- **rclone backend for Uploadcare.** Highest leverage per line of code: one Go backend implementing `fs.Fs` gives
  `mount`, `nfsmount`, `serve webdav|sftp|nfs|s3|http`, `sync`, `copy`, `ls`, VFS caching, Windows support and rclone's
  user metadata framework (maps onto Uploadcare metadata). Loses the Uploadcare-specific features (variants as paths,
  derived xattrs, tags/groups views) unless bolted on, and directory emulation still has to be invented. Upstreaming needs
  a committed maintainer; a fork is the fallback. Worth prototyping as a second track; the engine written for ucfs can back
  both.
- **WebDAV server only.** Zero-install on every OS and a good fallback mode for `ucfs serve`, but a poor primary UX.
- **Sync client (File Provider).** A different, bigger product; the right eventual answer for designers.
- **MCP server.** Complementary, not competing.

## 11. Suggested plan

- **Phase 0, PoC (1–2 days):** Go module `ucfs`; read-only; `files/<uuid>_<name>` and `-/` variants; SQLite index with
  full scan plus incremental `from`; block cache; loopback NFSv3 via `go-nfs` on macOS and `cgofuse` on Linux. Exit
  criteria: `ls`, `cat`, open an image in Preview, `rsync` a subset out, a traffic counter showing what Finder pulled,
  and two processes reading different variants with no mutual stall (go-nfs `ConcurrentHandlers` set, adaptive
  prefetch and kernel attribute caching in from day one, see section 7).
- **Phase 1:** create/upload-on-close, delete, xattr ↔ metadata (macOS needs FUSE-T or NFSv4 for xattrs), `tags/`,
  `groups/`, `by-date/`.
- **Phase 2:** writable `tree/` with `ucfs:path`, rename, `local_copy` on `cp` of variants, webhook or sweep-based
  invalidation, conflict handling.
- **Phase 3:** packaging (Homebrew, launchd/systemd), `uploadcare fs mount` in the CLI, decision on the rclone backend,
  FSKit/File Provider spike for the designer audience.

## 12. Open questions

- Actual REST rate limits for list-heavy scans (ask platform; the design assumes "index once, poll rarely").
- Non-root `mount_nfs` of a localhost server on macOS 27 (rclone `nfsmount` suggests yes; verify in the PoC).
- Does direct upload preserve `original_filename` exactly, or sanitise like `from_url`? Test before relying on names.
- Metadata key for the writable tree: `ucfs:path` (colon allowed) vs `ucfs.path`.
- Signed/authenticated CDN URL support in the reader.
- Product stance on CDN traffic generated by mounts (warn, cap, or bill as usual).
- Whether removed files should be visible at all (`.removed/` info-only view).

## 13. Sources

Verified locally: `uploadcare-api-references/specs/rest-api/v0.7.0/index.yaml`, `specs/upload-api/index.yaml`,
`uploadcare-cli` (`file list`, `file info`, `project info`), `uploadcare-go/file/search.go`, `uploadcare-go/ucare/retry.go`,
`curl` against `ucarecdn.com` and `55317o72r5.ucarecd.net`.

- FUSE-T: https://github.com/macos-fuse-t/fuse-t and https://fuse-t-org.pages.dev/
- rclone mount with FUSE-T via FSKit: https://forum.rclone.org/t/macos-rclone-mount-with-fuse-t-via-fskit/53608
- macFUSE FSKit backend discussion: https://forums.macrumors.com/threads/macfuse-veracrypt.2485302/
- Apple Reduced Security for kexts: https://support.apple.com/guide/security/sec8e454101b
- Apple FSKit: https://developer.apple.com/documentation/fskit and https://developer.apple.com/documentation/fskit/fsunaryfilesystem
- rclone nfsmount: https://rclone.org/commands/rclone_nfsmount/ and serve nfs: https://rclone.org/commands/rclone_serve_nfs/
- go-nfs (Go NFSv3 server): https://github.com/willscott/go-nfs
- nfsserve (Rust NFSv3 server, XetHub): https://docs.rs/crate/nfsserve/latest and https://docs.rs/nfs3_server
- cgofuse (cross-platform Go FUSE): https://github.com/winfsp/cgofuse ; go-fuse: https://github.com/hanwen/go-fuse
- Mountpoint for S3 semantics: https://github.com/awslabs/mountpoint-s3/blob/main/doc/TROUBLESHOOTING.md and
  https://scality.github.io/mountpoint-s3-csi-driver/1.2/concepts-and-reference/filesystem-semantics
- Uploadcare Upload API errors (429): https://uploadcare.com/docs/api/upload/errors.md
- Uploadcare file search guide: https://uploadcare.com/docs/file-search/
