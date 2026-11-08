# Filesystem semantics

What each operation does in each part of the mount, and the error it returns where it does not work. Linux uses the
FUSE front-end, macOS the NFS one; where they differ, both are given. Every entry comes from the code
(`internal/vfs`, `internal/frontend`, and the patched go-nfs in `third_party/go-nfs`); entries marked † were also
seen on a real mount in the phase reports. Over NFS the server answers with an NFS status, and macOS turns it into
the errno an application sees; that translation was measured only for the † entries.

Places:

| Short name | Paths |
|---|---|
| stored file | `files/<uuid>_<name>`, also reachable as `files/<uuid>/<name>` |
| new file | a file this mount wrote: a new name in `files/`, or a file in `tree/` written here |
| tree file | a file in `tree/`: one with `ucfs:path` metadata, or one this mount wrote |
| foreign file | a file with no `ucfs:path` that this mount did not write: `files/<uuid>_<name>`, `tree/_inbox/*` |
| variant | anything under `files/<uuid>/-/` |
| views | `by-date/`, `tags/`, `groups/` and everything below them |
| shadow file | `.DS_Store`, `._*`, `.Spotlight-V100`, `.fseventsd`, `.Trashes`, `.TemporaryItems`, vim swap files ([finder.md](finder.md)) |

A `--read-only` mount refuses every change with `EROFS` (the kernel refuses first: `ro` on FUSE, `rdonly` on macOS).
The tables below are for a writable mount.

## Errors at a glance

| Error | Meaning here |
|---|---|
| `ENOENT` | no such name; a file the REST API says is removed |
| `EROFS` | a change in `files/<uuid>…`, a variant, a view or `.ucfs/` †; any change on `--read-only` |
| `EPERM` | changing a foreign file without `--allow-foreign` †; an ordinary file at the root or in `tree/_inbox/`; `mkdir` outside `tree/`; a read-only extended attribute; an upload the project refuses (file type not allowed) |
| `EXDEV` | a rename between views, such as `tree/` to `files/` |
| `EOPNOTSUPP` | writing into an uploaded file without truncating it; hard links, symbolic links, special files (FUSE) |
| `EINVAL` | a variant argument the CDN refuses †; a name with `\` † (accepted with PR #6); a rename into itself |
| `EACCES` | the API refuses the keys (401, 403); with PR #6 also every change while the API keeps refusing (degraded mode) |
| `EAGAIN` | the API throttles (429) after retries; an attribute set on a file being uploaded; NFS sends it as `NFS3ERR_JUKEBOX`, which the macOS client retries for about 28 s and then fails with `ETIMEDOUT` (measured in Phase 5) |
| `EIO` | network errors and 5xx after retries; a failing index; with PR #6 a file that changed on the CDN during a read |
| `EDQUOT` | the day's traffic budget is used up (PR #6, [costs.md](costs.md#traffic-budget)) |
| `EFBIG` | a file too large for the Upload API |
| `ENAMETOOLONG` | a `tree/` path over 512 characters; a metadata key over 64; a tag over 100 |

## Reading

| Operation | Result |
|---|---|
| `ls`, `stat` | works everywhere from the index; a variant's first `stat` sends one `HEAD` |
| `stat` of a missing name | `ENOENT`; an unknown operation after `-/` is `ENOENT` †, a bad argument `EINVAL` † |
| read a stored, tree or view file | works, through the block cache; a CDN 404 is `ENOENT` once the REST API confirms the file is gone |
| read a variant | works; a CDN 400 is `EINVAL` † |
| read a new file before its upload | works, from the local spool |
| read a shadow file | works, from the cache dir |
| read `.ucfs/uploads.log` | the upload journal |

On FUSE, a read that fails after the kernel has buffered part of the file reports `EIO` whatever the cause; with
PR #6 a file larger than the traffic budget left is opened with direct I/O so that `EDQUOT` gets through.

## Creating and writing

| Where | Create, write | Notes |
|---|---|---|
| `tree/<dir>/` | works: spooled, then uploaded | Linux uploads on close and returns the upload's error there; macOS uploads 300 ms after a COMMIT or 3 s after the last write, and reports errors only in the log and `ucfs spool list` |
| `files/<new name>` | works: uploaded, then listed as `<uuid>_<name>` | the written name resolves for `--alias-ttl` |
| over an uploaded tree file | `O_TRUNC` or truncate to 0 works: a new upload replaces it (`--on-overwrite`); writing without truncating is `EOPNOTSUPP`, except within a minute of this mount's own upload, which continues it | a foreign file: `EPERM` without `--allow-foreign` † |
| `files/<uuid>…`, `files/<uuid>/` | `EROFS` † | |
| variants, views, `.ucfs/` | `EROFS` † | |
| the root, `tree/_inbox/` | `EPERM` | shadow files work there |
| any shadow name | works, kept local, never uploaded | |
| a name with `\` | `EINVAL` † | PR #6 uploads it with `_` and keeps the name in `ucfs:name` |
| an empty file | works, stays local until written | never uploaded while empty |

## Truncate

| Where | Result |
|---|---|
| a file being written | works |
| a tree file | to its size: no-op; to 0: starts an overwrite; to another size: `EOPNOTSUPP`, except within a minute of this mount's own upload |
| stored files, variants, views, `.ucfs/uploads.log` | to the same size: works on FUSE, `EROFS` on NFS (go-nfs opens the file for writing first); to another size: `EROFS` |
| a shadow file | works |

macOS `cp` truncates every file it writes to the length it wrote, so that case is a no-op.

## Removing

| Operation | Where | Result |
|---|---|---|
| `rm` | a tree file or new file | works: deletes the file from the project (one request) |
| `rm` | a foreign file | `EPERM` without `--allow-foreign` †; with it, deletes |
| `rm` | variants, `.ucfs/uploads.log`, `.metadata_never_index` | `EPERM` |
| `rm` | views | `EROFS` † |
| `rm` | a shadow file | works, local |
| `rmdir` | a `tree/` folder | works when empty (shadow files count); `ENOTEMPTY` otherwise |
| `rmdir` | `files/`, `files/<uuid>/`, `tree/_inbox`, `.ucfs` | FUSE: `EPERM`; NFS: `ENOTEMPTY` when not empty (go-nfs checks first) |
| `rmdir` | views | FUSE: `EROFS`; NFS: `ENOTEMPTY` when not empty |

## Folders

| Operation | Result |
|---|---|
| `mkdir` in `tree/` | works; the folder is local until a file below it is uploaded, and stays until `rmdir` |
| `mkdir` of a shadow name | works, local |
| `mkdir` at the root, in `files/`, in `tree/_inbox/` | `EPERM` |
| `mkdir` in `files/<uuid>/`, variants, views, `.ucfs/` | `EROFS` † |

## Renaming

| From | To | Result |
|---|---|---|
| an uploaded tree file | another `tree/` path | works: one metadata write of `ucfs:path` †; keeps its uuid and NFS handle |
| an uploaded tree file | `tree/_inbox/` | works: removes `ucfs:path` |
| a stored file `files/<uuid>_<name>` | a `tree/` path | works with `--allow-foreign`, `EPERM` without † |
| a file not uploaded yet | any `tree/` or `files/` path | works: uploads under the final name |
| any file | over an existing file | the target is replaced per `--on-overwrite` (`replace` deletes it; a foreign target needs `--allow-foreign`) |
| a `tree/` folder | another `tree/` path | works: one metadata write per file below it, not atomic; a warning above 1,000 files; into itself `EINVAL` |
| a shadow file | an ordinary name in `tree/` or `files/` | works: uploads it |
| an uploaded file | `files/`, the root, a shadow name | `EXDEV` |
| anything | `files/<uuid>/`, variants, `.ucfs/` | `EXDEV` |
| anything | to or from views | `EROFS` † |
| variants, view roots, `files/`, `tree/_inbox` | anywhere | `EPERM` |

On FUSE, `RENAME_EXCHANGE` is `EINVAL` and `RENAME_NOREPLACE` over an existing name `EEXIST`. Applications that save
by writing a temporary file and renaming it over the original (`NSDocument`, vim with `backupcopy=no`) work in `tree/`.

## Links, special files, attributes, locks

| Operation | Linux FUSE | macOS NFS |
|---|---|---|
| hard link (`ln`) | `EOPNOTSUPP` | refused (`NFS3ERR_ACCES` from go-nfs; the error macOS shows was not measured) |
| symbolic link (`ln -s`) | `EOPNOTSUPP` | refused (`NFS3ERR_ACCES`; not measured) |
| `mkfifo`, `mknod` | `EOPNOTSUPP` | refused (`NFS3ERR_ACCES`; not measured) |
| `readlink` | `EINVAL`: nothing is a link | `EINVAL`: nothing is a link |
| `fallocate` | `EOPNOTSUPP` | not available |
| `chmod`, `chown`, `touch -t`, `utimes` | succeed and change nothing | succeed and change nothing |
| `flock`, `fcntl` locks | kept by the kernel on this machine only (from go-fuse's defaults; not measured) | local to this Mac (`locallocks`) |
| extended attributes | `user.uc.*`, below | none: NFSv3 has no xattrs; macOS stores them in `._*` shadow files, which stay local; use `ucfs resolve` |
| `df` | all zeros | 2^62 bytes and files, none available on `--read-only` |

Attributes shown by `stat`:

- Owner: the user who mounted, on every file. Other users: FUSE keeps them out (no `allow_other`); on NFS, `main`
  answers every access check with yes, and PR #6 grants access only to the mounting user and root.
- Mode: stored files, variants and view files `0444`; tree files `0644` (`0444` on `--read-only`); writable folders
  (`files/`, `tree/` and its folders) `0755`, the rest `0555`.
- Times: a stored or tree file's modification time is its upload time; a file being written, the last write; a
  variant, when ucfs first saw it with its current size and ETag; a folder, the last index change. NFS reports the
  same time as access and change time; FUSE reports it as the change time and no access time.
- Link count 1 for files, 2 for folders; inode numbers are stable across remounts, and a tree file keeps its inode
  through upload, overwrite and rename.

## Extended attributes (Linux)

| Name | Access | Notes |
|---|---|---|
| `user.uc.uuid`, `user.uc.cdn_url`, `user.uc.mime`, `user.uc.stored` | read | `cdn_url` includes a variant's operations; `uuid` is empty while a file is being written |
| `user.uc.width`, `user.uc.height` | read | images only |
| `user.uc.md5` | read | the CDN's ETag, one `HEAD` on first read; listed only when known or the file is under 10 MiB, since a multipart upload's ETag is not an MD5 |
| `user.uc.meta.<key>` | read, write, remove | the file's metadata; one request each; `ucfs:path` is `EPERM` (use `mv`) |
| `user.uc.tags` | read, write, remove | comma-separated; writing replaces the list |

Reading a file's attributes fetches its info again when the index copy is older than `--meta-ttl` (5 min);
`listxattr` never does. Errors: a name outside `user.uc.*` is `ENOTSUP` to set and `ENODATA` to get; another
`user.uc.*` name is `EPERM` to set, but ignored on a file being written or uploaded by this mount in the last minute,
so `cp --preserve=xattr` and `rsync -X` of files from the mount work; a key over 64 characters or a tag over 100 is
`ENAMETOOLONG`; a key outside `[A-Za-z0-9_.:-]`, an empty value or one that is not UTF-8 is `EINVAL`; a value over
512 characters is `E2BIG`; a 51st key or tag is `ENOSPC` †; `XATTR_CREATE` on an existing name is `EEXIST`,
`XATTR_REPLACE` on a missing one `ENODATA`. On a file being written, metadata and tags are queued into its upload,
and `EAGAIN` is returned while the upload runs. Variants refuse changes with `EPERM`; folders and shadow files have no
attributes.

## Durability

- A file is in the project once its upload finished: `ucfs resolve <path>` shows its uuid, and `.ucfs/uploads.log`
  lists it. Until then it is a spool in the cache dir; a crash keeps it, and the next mount uploads it again.
- Unmounting uploads what is still being written, for up to 2 minutes.
