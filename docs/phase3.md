# Phase 3 acceptance: writes

Measured on 2026-11-08 against `uploadcare-go-test`, on the same two hosts as [Phase 1](phase1.md) and
[Phase 2](phase2.md):

- **macOS:** Apple Silicon Mac, macOS 27.0.1, NFS front-end, nothing installed.
- **Linux:** x86_64 host, Ubuntu 26.04.1 with kernel 7.0, FUSE front-end, mounted as a normal user through
  `fusermount3`.

Both mounts ran with `--upload-metadata ucfs:test=1`, so every upload carried that tag, and with an empty
`--cache-dir`. The test images are photo-like JPEGs from [`spikes/photogen`](../spikes/photogen/main.go):
`photo.jpg` (2000×1500, 458,158 B), 50 images of 57–151 KB (5,116,818 B in all) and `big12.jpg` (6700×4470,
12,192,312 B) for multipart. The project accepts text files (a probe `.txt` uploaded as `text/plain`), so the editor
saves use text. The measurement script is [`spikes/measure/writes.sh`](../spikes/measure/writes.sh); request counts
come from `ucfs status --json`, which now splits API requests into REST, Upload API and multipart part PUTs.

Before the first write the project listed 33 files, none with `ucfs:` metadata. Their uuids, names and metadata
were recorded, and after the last cleanup the listing was identical.

## Acceptance

| Criterion | macOS, NFS | Linux, FUSE |
|---|---|---|
| `cp photo.jpg tree/photos/` leaves `tree/photos/photo.jpg` readable | yes, same bytes (`cmp`) | yes, same bytes |
| `original_filename`, `ucfs:path` | `photo.jpg`, `/photos/photo.jpg` | `photo.jpg`, `/linux/photos/photo.jpg` |
| Journal line in `.ucfs/uploads.log` | `2026-11-08T08:31:53Z  tree/photos/photo.jpg  7fbc3d27-…  458158  image/jpeg` | the same form |
| `ucfs resolve tree/photos/photo.jpg` | prints uuid and CDN URL | same |
| `cp` returns after | 0.018–0.024 s | 0.93–2.40 s: close(2) waits for the upload |
| File has its uuid after `cp` returns | 1.8–4.3 s, of which 0.33–0.37 s is the trigger and the rest the upload | 6–14 ms (the `ucfs resolve` process) |
| Save from vim in `tree/` | works, saving in place; swap files kept local | works, with a backup copy; backup deleted |
| Save from TextEdit in `tree/` | blocked by a TextEdit dialog, not measured; an NSDocument save of the same kind works (below) | not applicable |
| `rm` soft-deletes | `datetime_removed` set, gone from `tree/` and the listing | same |
| Copy a folder of 50 images | Finder: 50 uploads, 0 sidecars uploaded, 51 shadow files and 62 shadow writes absorbed | `cp -R`: 50 uploads, no sidecars written |
| Wall clock of the 50-image copy | Finder done in 0.39 s, all 50 uploaded after 6.59 s | `cp -R` 33.2 s, uploaded when it returns |
| Requests of the 50-image copy | 50 Upload API, 0 REST, 0 part PUTs | 50 Upload API, 1 REST (the sync loop), 0 part PUTs |
| Daemon killed with SIGKILL mid-upload | next mount deletes the abandoned upload, uploads again; the file arrives once | same; the writer gets `Software caused connection abort` at close |
| Overwrite of a file written through the mount | new uuid, metadata copied, old one soft-deleted | same |
| Multipart upload of a file above 10 MB | 12.2 MB in 5.17 s (3 parts) | 12.2 MB in 3.99 s and 3.64 s |

## Writing a file and getting its uuid

`cp photo.jpg tree/photos/` on macOS returned in 18 ms: the NFS client buffers the data and writes it after `cp`
closes. The file then needs its upload. Five more copies of the same file into `tree/lat/`:

| Copy | `cp` | uuid after `cp` returned | of which the upload |
|---|---|---|---|
| 1 | 0.018 s | 4.30 s | 3.93 s |
| 2 | 0.020 s | 1.83 s | 1.46 s |
| 3 | 0.024 s | 2.95 s | 2.60 s |
| 4 | 0.022 s | 3.94 s | 3.60 s |
| 5 | 0.022 s | 3.73 s | 3.37 s |

The rest, 0.33–0.37 s, is the 300 ms settle after the commit trigger plus `ucfs resolve --wait` polling. The very first
copy, before a fix described under *Found while measuring*, took 5.93 s: it waited for the 3 s idle timer. The Mac's
uplink to the Upload API was slow and uneven that day (120–310 KB/s for one 458 KB upload); the Linux host sent the
same file in 0.8–1.3 s.

On Linux `cp` blocks in close(2) until the upload ends, so the uuid is there when `cp` returns:

| Copy | `cp` | uuid after `cp` returned |
|---|---|---|
| first | 2.40 s | 0.006 s |
| 1–5 | 1.31, 1.28, 1.10, 1.06, 0.93 s | 0.013–0.014 s |

The Linux mount listed the Mac's `tree/` as soon as its index synced: the tree lives in the metadata, so every
mount of the project sees the same tree.

## Editor saves

**vim on macOS** (9.1) saved in place: truncate to zero, write, COMMIT. Its system vimrc sets `backupcopy=yes`, and
`backupskip` holds `/private/tmp/*`, where the test mount lived, so it made no backup. ucfs uploaded the new version
(`ec7507af…` → `5e773f59…`), copied the old one's metadata and soft-deleted it. vim's swap files (`.vim.txt.swp`,
and `.vim.txt.swx`, which it creates to probe) and their `._` sidecars stayed local.

**vim on Linux** (vim.tiny 9.1) first copied the file to `vim.txt~`, then wrote `vim.txt` in place, then deleted the
backup. The result is right (`3620b7e7…` → `65b156d8…`, the old version soft-deleted), at the cost of one upload and
one delete for the backup, which is an ordinary name.

**vim's rename save** (`backupcopy=no`: rename the original to `name~`, create a new file under the name, remove the
backup) is the "temp file plus rename" flow; neither host's vim chose it on its own. It was going to be run live
with `backupcopy=no` on both hosts, but by then the API refused the project (below). It is tested against the fake
through FUSE on Linux (`TestWrites`) and over NFS RPC (`TestRenameBackupSaveOverRPC`, which also checks that the
new file does not get the renamed original's handle and that no `.nfs*` or `~` entry is left).

**NSDocument saves.** Osascript was allowed to drive TextEdit, but opening a document on the mount left TextEdit
behind a modal dialog: every Apple event to it timed out and it never touched the mount, so the dialog, probably a
macOS privacy prompt for network volumes, needs the user. TextEdit saves through NSDocument, which calls
`FileManager.replaceItemAt`; a 20-line Swift tool doing exactly that reproduces the RPCs. Traced on the go-nfs rig
first, it:

1. makes a scratch directory `(A Document Being Saved By <app>)` next to the file,
2. creates the new version in it, writes it, COMMITs,
3. renames it over the original,
4. removes the scratch directory.

Through ucfs the save produced one upload, under the final name: the rename reached the spool before its 300 ms
settle ended, so the spool moved and uploaded as `tree/notes/note.txt`. The old version was soft-deleted
(`7e11795d…` → `ddd0c583…`), and the scratch directory, a local tree directory, went away with its `rmdir`.

## Finder copy of 50 images

Finder, driven by osascript `duplicate`, copied `photos50c/` into `tree/`:

| | First run (sync every 5 s) | Second run (defaults) |
|---|---|---|
| Finder done | 0.59 s | 0.39 s |
| All 50 uploaded | 6.95 s | 6.59 s |
| Uploads, bytes | 50, 5,116,818 B | 50, 5,116,818 B |
| Upload API / REST / part PUTs | 50 / 2 / 0 | 50 / 0 / 0 |
| Shadow files created / shadow writes | 53 / 63 | 51 / 62 |

Each image got an AppleDouble `._IMG_….jpg` (Finder copies `com.apple.provenance`), the folder a `._photos50c`;
those, and `.DS_Store` and `._.DS_Store` in `tree/` on the first run, stayed in the cache dir. The two REST requests of
the first run were the 5 s sync loop. Uploads run 8 at a time.

`cp -R` of the same folder on Linux took 33.2 s for 50 uploads (1 REST, the sync loop): FUSE close blocks on the
upload, and `cp` closes one file before it opens the next. A first try copied onto the folder the Mac had already
written, which made it 50 overwrites: 58.9 s, 50 uploads and 102 REST requests (50 metadata reads of the replaced
files, 50 soft-deletes, 2 syncs).

## A daemon killed mid-upload

`cp big12.jpg tree/big/` on macOS. The multipart upload started 0.76 s after `cp` returned (its uuid,
`dc27fac1…`, recorded in the spool row), and the daemon got SIGKILL 1.5 s later, with parts in flight. Then:

- The API listed the abandoned upload like a file: `is_ready: false`, stored, its metadata already set, in `file
  list`. Left alone, the project would not have returned to 33 files.
- `ucfs umount` found no daemon and unmounted the dead NFS mount.
- The next `ucfs mount` synced, saw the spool in state `uploading` with a multipart uuid, asked the API about it (not
  ready), soft-deleted it, and uploaded again: 12.2 MB in parts in 5.17 s.
- The project listed one `big12.jpg` (`aad360bd…`); the abandoned uuid had `datetime_removed`; the CDN served the
  source bytes (same MD5).

On Linux the same test (`crash.jpg`, multipart uuid `b928eb4e…`, killed 1.5 s into the upload) went the same way, and
`cp` itself failed: `failed to close 'mnt/tree/linux/big/crash.jpg': Software caused connection abort`. The remount
uploaded in 3.64 s; one `crash.jpg`, same bytes.

## Overwrite

`photo.jpg` (`7fbc3d27…`, written through the mount) got `camera=canon-eos` from the CLI, as another client
would set it. `cp photo-v2.jpg tree/photos/photo.jpg` then produced `fc7e0c08…` with `camera`, `ucfs:path` and
`ucfs:test`, and the old file had `datetime_removed`. The tree node, and with it the NFS handle and inode number,
stayed the same. The same on Linux (`c61cd2a1…` → `5e4ccef0…`). The project has no tags feature (file info carries
no `tags`), so copying tags was tested against the fake only.

## Other checks, on both hosts

| Check | Result |
|---|---|
| `mv` of an uploaded file in `tree/` | 1 REST request (metadata PUT), 0 uploads; `original_filename` unchanged |
| `mv` of a directory of 5 (macOS) and 4 (Linux) files | 5 and 4 REST requests, 0 uploads |
| `cp photo.jpg files/alias.jpg && cmp` | reads back; `ls files/` shows only `<uuid>_alias.jpg` |
| `touch tree/empty.txt` | stays spooled (`pending`, `writing`), never uploaded |
| `cp x 'tree/back\slash.jpg'` | Invalid argument |
| `rm tree/_inbox/me.jpg`, `rm files/<uuid>_file.jpg` | Operation not permitted |
| `mv tree/_inbox/me.jpg tree/` | Operation not permitted |
| Create in `files/<uuid>/` | Read-only file system |
| Truncate an uploaded file to 5 bytes, append to it (Linux) | Operation not supported |
| Read back a file just written | from the block cache, 0 CDN bytes |

Cleanup went through the mounts: `rm -r` of the test directories in `tree/` soft-deleted 223 files on macOS, and
`rm files/linux-alias.jpg` on Linux the last one (the Mac's mount had not written it, so it refused). Live uploads
this phase came to about 80 MB, abandoned multipart parts included.

## Behaviour this phase defines

- **Write targets.** `tree/` is the recommended one. A name written in `files/` becomes an alias of
  `files/<uuid>_<name>` for an hour (`--alias-ttl`) and lands in `tree/_inbox/`. `files/<uuid>_<name>`,
  `files/<uuid>/…`, `.ucfs/` and `.metadata_never_index` are read-only; the root and `tree/_inbox/` take shadow files
  only.
- **Commit triggers.** FUSE close (and fsync) uploads and returns the upload's errno. On NFS the upload starts 300 ms
  after a COMMIT or a stable (`FILE_SYNC`/`DATA_SYNC`) WRITE, or 3 s after the last write (`--commit-idle`); a later
  write cancels an upload in flight. A failed upload stays spooled as `failed` for `ucfs spool retry`. Zero-length
  files are never uploaded until written.
- **Errors.** A rejected file type is EPERM, an oversized file EFBIG, a throttle after retries EAGAIN (NFS3ERR_JUKEBOX),
  network errors and 5xx EIO after three tries. Names with `\` are EINVAL, tree paths over 512 characters
  ENAMETOOLONG.
- **Tree layout.** `ucfs:path` holds the absolute path under `tree/`, name included, as written (NFC from macOS),
  at most 512 characters, so a rename is one metadata PUT and `original_filename` keeps the uploaded name. Files
  sharing a path are told apart by a uuid suffix (`photo~1a2b3c4d.jpg`), the newest keeping the plain name. Values
  that are not absolute, clean paths leave the file in `tree/_inbox/`. A directory exists on every mount while a
  file below it has a tree path; one made by `mkdir`, or emptied by `rm` or `mv`, is kept in the local index until
  `rmdir`.
- **Overwrite policy.** `--on-overwrite=replace` (default) copies the replaced file's current metadata (read over
  REST, one request) and tags into the upload form, then soft-deletes it; `keep` leaves it, under a suffixed name.
  Rename over an uploaded file follows the same policy without copying.
- **Deletes.** `rm` in `tree/` and of an alias soft-deletes. Files the mount did not write that have no tree path,
  `files/<uuid>_<name>` and `tree/_inbox/`, cannot be removed, overwritten or moved without `--allow-foreign`.
- **Identity.** A written file keeps one inode number and NFS handle through its upload, overwrites and renames
  (the `objects` table); directories keep theirs through renames.
- **Shadow files.** `.DS_Store`, `._*`, `.Spotlight-V100`, `.fseventsd`, `.Trashes`, `.TemporaryItems` and vim swap
  files are kept in the cache dir; renamed to an ordinary name in `tree/` or `files/` they upload.
- **Crash safety.** Spools that were queued or uploading are uploaded again by the next mount, after it deletes an
  abandoned multipart upload or finds that the upload completed; spools cut off while written stay for `ucfs spool`.
  Unmount uploads what is still being written and waits up to 2 minutes. A writable mount locks its cache dir, so a
  second writable mount of the same project with the default cache dir now fails; `--read-only` mounts and another
  `--cache-dir` still work.
- **Read-only.** Mounts are writable by default; `--read-only` opts out and still adds `rdonly` on macOS.

## Found while measuring

- **NFS clients never sent COMMIT.** go-nfs answered every WRITE with `FILE_SYNC`. With `UNSTABLE` answers, a
  trace of macOS `cp` of 15 MB showed one COMMIT right after the last data WRITE, no COMMIT mid-file.
- **A small file comes as one stable WRITE and no COMMIT.** A file that fits in one 1 MiB WRITE is sent `FILE_SYNC`
  and only its 4 KiB `._` sidecar gets COMMITs, so the first copy waited for the idle timer (5.9 s to uuid). go-nfs now
  treats a stable WRITE as a commit too; the copies after it took 0.33–0.37 s plus the upload.
- **An abandoned multipart upload is listed.** The API lists it as a file that is not ready, with its metadata.
  Recovery used to match spools by name and size and would have taken it for the finished upload; it now asks the
  API about the multipart uuid it recorded at the start, and tree listings hide multipart uploads in progress.
- **Metadata set by others after a file was listed never reaches the index**, as incremental listing moves on.
  An overwrite would have copied stale metadata, so it reads the replaced file's metadata over REST.
- **A renamed directory's old handle failed.** macOS kept using the handle of a directory it had just renamed and
  `ls` got ENOENT once. Handle ids of tree and shadow directories now follow renames.
- **Exclusive CREATE was refused** by go-nfs (NOTSUPP), which vim's swap files need; it is now served as guarded.

## Not covered

- **TextEdit itself**, blocked by its dialog; the NSDocument path it uses was measured with a tool.
- **vim's rename save live.** After the cleanup the REST API began answering `403 Operation denied` for the
  project, for the CLI as well as for ucfs (the local config file is unchanged; the CDN still serves), so the last
  live run could not start. The cleanup had finished and been verified by then.
- **Tags**, which the project lacks.
- **Linux NFS and FUSE under `-race`**, as before (no root, no cgo on the Linux host); `go test -race` ran on macOS
  for every package with tests.
- **Races with other clients.** A listing computed before a rename and applied after it puts the old tree path back
  in the index until the file is listed again; renames and deletes by other clients appear with the 30 s sync and
  the 10 min removed sweep.
- **Directory renames are not atomic**: a failure midway leaves some files moved (the error says which).
