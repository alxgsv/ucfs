# Commands and options

Every command and flag of `ucfs`, with its default, as `ucfs <command> --help` prints them. Flags marked
**PR #6** land with [Phase 5](https://github.com/alxgsv/ucfs/pull/6) and are not in `main` yet.

## Credentials, project and cache directory

Global flags work with every command.

| Flag | Environment | Default | Meaning |
|---|---|---|---|
| `--config` | | `~/.uploadcare/config.yaml` | the [Uploadcare CLI](https://github.com/uploadcare/uploadcare-cli)'s config file |
| `--project` | `UPLOADCARE_PROJECT` | `default_project` of the file | a project name from the file, matched without regard to case |
| `--public-key` | `UPLOADCARE_PUBLIC_KEY` | | the project's public key |
| `--secret-key` | `UPLOADCARE_SECRET_KEY` | | the project's secret key |
| `--cdn-base` | `UPLOADCARE_CDN_BASE` | derived from the public key | CDN base URL, such as a custom CDN domain |
| `--cache-dir` | | `<user cache dir>/ucfs/<public key>` | index, block cache, spool, logs |
| `-v`, `--verbose` | | off | debug logging |
| `--insecure-config` (**PR #6**) | | off | read a config file that group or others can read; without it such a file is refused, with the `chmod 600` that fixes it |

Keys come from, in this order: the key flags or variables (both keys must then be given this way), the project
named by `--project` or `UPLOADCARE_PROJECT`, the file's `default_project`, and the file's top-level keys. The CDN
base comes from the flag, the variable, the project's `cdn_base`, then the file's top-level `cdn_base`. The file:

```yaml
default_project: my-project
projects:
  my-project:
    public_key: ...
    secret_key: ...
    cdn_base: https://cdn.example.com   # optional
```

The user cache dir is `~/Library/Caches` on macOS and `$XDG_CACHE_HOME`, or `~/.cache`, on Linux. A project's cache
dir holds `index.db` (the SQLite index), `blocks/` (cached file content), `spool/` and `shadow/` (writes not uploaded
yet and local-only files), `uploads.log` (the upload journal), `write.lock`, and per mount a control socket
`ctl-<hash>.sock` (under `$TMPDIR` when the path would be too long for a socket) and a `--daemon` log
`mount-<hash>.log`. Running mounts are also recorded in `<user cache dir>/ucfs/mounts/`, whatever their cache dir.

Sizes such as `2GiB` or `500MB` take binary (`KiB`, `MiB`, `GiB`) and decimal (`KB`, `MB`, `GB`) units. Durations are Go
durations: `30s`, `10m`, `1h`.

## `ucfs mount <dir>`

Mounts the project at an existing directory that is not already a mount point, and serves it until
`ucfs umount <dir>`, an external `umount`, Ctrl-C or SIGTERM. The first mount of a cache dir lists the whole project
before it serves.

| Flag | Default | Meaning |
|---|---|---|
| `--daemon` | off | start in the background and return once the mount answers; output goes to `<cache dir>/mount-<hash>.log` |
| `--frontend` | `nfs` on macOS, `fuse` on Linux | `nfs` or `fuse`; `fuse` is Linux only, and `nfs` on Linux needs root to mount |
| `--read-only` | off | refuse every write; on macOS this also stops Finder writing `.DS_Store` files |
| `--views` | `files,tree,by-date` | views in the root, from `files,tree,by-date,tags,groups`; an unknown name is an error |
| `--cache-size` | `2GiB` | on-disk block cache limit; least recently used blocks go first |
| `--sync-interval` | `30s` | how often to list new uploads |
| `--sweep-interval` | `10m` | how often to look for removed files, and for new groups when `groups` is mounted |
| `--meta-ttl` | `5m` | an xattr read or `ucfs resolve` fetches a file's info again when the index copy is older |
| `--by-date-zone` | `Local` | time zone of `by-date/`: `Local`, `UTC` or a zone name such as `Europe/Berlin` |
| `--readahead` | `0` | macOS NFS client read-ahead in 1 MiB blocks; ucfs prefetches by itself |
| `--browse` | off | show the mount in the Finder sidebar (macOS) |
| `--on-overwrite` | `replace` | `replace`: writing over a file uploads a new one, copies metadata and tags, and deletes the old one; `keep`: leave the old one too |
| `--upload-metadata` | none | `key=value` metadata added to every upload; repeatable; `ucfs:path` is the mount's own |
| `--allow-foreign` | off | let `rm`, overwrites and `mv` change files other clients uploaded (`files/<uuid>_<name>`, `tree/_inbox/`) |
| `--multipart-threshold` | `10MiB` | upload larger files in 5 MiB parts; below 10 MiB, the Upload API's minimum, it acts as 10 MiB, and files over 100 MiB always go in parts |
| `--commit-idle` | `3s` | NFS: upload a written file this long after its last write when no COMMIT came |
| `--upload-concurrency` | `8` | uploads running at once |
| `--alias-ttl` | `1h` | how long a name written in `files/` keeps resolving |
| `--traffic-budget` (**PR #6**) | none | CDN bytes per day, such as `500MB` or `2GiB` (a `/day` suffix is accepted); reads that need the CDN past it fail with `EDQUOT` until local midnight, see [costs.md](costs.md#traffic-budget) |
| `--max-inflight` (**PR #6**) | `32` | CDN requests in flight at once; reads go before prefetches, which get at most three quarters of the slots |

A writable mount locks its cache dir: a second writable mount of the same cache dir fails, and read-only mounts can
share it.

What the front-ends mount with:

- macOS: `mount_nfs -o port=P,mountport=P,vers=3,tcp,locallocks,noresvport,rsize=1048576,wsize=1048576,readahead=0,nfc,soft,intr,acregmax=3600,acdirmax=5,nobrowse 127.0.0.1:/ <dir>`,
  with `rdonly` for `--read-only`, without `nobrowse` for `--browse`, and `--readahead` as `readahead`. The kernel adds
  `nodev,nosuid` to a mount made by a user. The server listens on 127.0.0.1 only.
- Linux FUSE: `fsname=ucfs`, `noatime`, `max_read=1048576`, `ro` for `--read-only`, through `fusermount3`, without
  `allow_other`, so only you see the files; `findmnt` shows `nosuid,nodev,noatime,user_id=…,group_id=…`.

## `ucfs umount <dir>`

Asks the mount's daemon to unmount and waits up to 30 seconds for it to close its control socket. When no daemon
answers, it forces the mount off instead: on macOS `umount`, then `umount -f`, then `diskutil unmount force`; on Linux
`fusermount3 -u`, then `umount`, `umount -f` and `umount -l`. There is no `-f` flag; this is what cleans up after a
crash. A writable mount's daemon goes on uploading what was written for up to 2 minutes after the unmount.

## `ucfs status [dir] [--json]`

Prints the counters of the mount at `dir`, or of every running mount: those in the registry and those using the cache
dir. `--json` prints an array of objects:

| Field | Meaning |
|---|---|
| `mount_dir`, `frontend`, `project`, `pid`, `started`, `views` | the mount |
| `index.files`, `index.bytes` | present files in the index and their size |
| `index.last_sync`, `index.sync_error` | the last successful listing, and the last error if the latest failed |
| `cdn.requests`, `cdn.heads`, `cdn.gets`, `cdn.bytes` | CDN requests and the body bytes received |
| `cache.served`, `cache.hit` | bytes served to readers, and the part served from disk |
| `cache.fetched_demand`, `cache.fetched_prefetch` | bytes fetched for a reader waiting, and ahead of readers |
| `cache.used`, `cache.max` | block cache size and limit |
| `api.rest`, `api.upload`, `api.parts`, `api.throttled` | REST requests, Upload API requests, multipart part uploads, and 429 answers |
| `meta.ttl` | `--meta-ttl` in nanoseconds |
| `meta.refreshes`, `meta.refresh_errors` | file info fetched again for an xattr read or `ucfs resolve` |
| `meta.xattr_lists`, `xattr_gets`, `xattr_sets`, `xattr_removes` | extended attribute calls (Linux) |
| `meta.md5_heads`, `meta.group_fetches` | CDN HEADs for `user.uc.md5`, and group info reads |
| `writes.enabled`, `writes.overwrite` | whether the mount is writable, and `--on-overwrite` |
| `writes.writing`, `queued`, `uploading`, `failed` | spooled files by state |
| `writes.uploads`, `upload_failures`, `upload_bytes` | finished and failed uploads, and bytes uploaded |
| `writes.shadow_creates`, `shadow_writes`, `shadow_bytes` | local-only files such as `.DS_Store` |

**PR #6** adds:

| Field | Meaning |
|---|---|
| `index.sync_failures`, `index.next_sync` | failed listings in a row, and when the next try is |
| `index.db_bytes` | size of `index.db` with its write-ahead log |
| `cdn.max_inflight`, `inflight`, `queued`, `queued_prefetch` | `--max-inflight`, requests in flight, and reads and prefetches waiting for a slot |
| `cdn.waits`, `waited_ms`, `prefetch_dropped` | reads that waited and for how long in all, and prefetches dropped with 1,024 already waiting |
| `cdn.etag_changed` | ranges that came with another ETag than their file's, which fail the read |
| `budget.limit`, `used`, `remaining`, `resets_at` | the traffic budget, present with `--traffic-budget` |
| `budget.refused`, `budget.prefetch` | reads refused with `EDQUOT`, and whether prefetch still runs |
| `api.state`, `since`, `detail` | `ok`, `denied` or `throttled` as the latest REST answers showed, since when, and what the API said |
| `api.degraded` | the state has lasted a sync interval: the mount serves the index and refuses writes |
| `process.goroutines`, `fds`, `heap_bytes` | the daemon's own use of the machine |

## `ucfs resolve <path> [--json] [--wait <duration>]`

Prints what a path in a running mount stands for: the uuid, CDN URL, metadata and tags of a file or variant, or that
a write is still pending, with its spool id and error. File info older than the mount's `--meta-ttl` is fetched again
first. `--wait` blocks up to that long for a pending upload. On macOS this is how a script learns the uuid a `cp` got.

## `ucfs spool list|retry|drop`

Files written through a mount wait in the spool until uploaded, and a failed upload stays there. These commands talk
to the running mount, or act on the cache dir when none runs (`retry` then uploads in the foreground).

| Command | Flags |
|---|---|
| `ucfs spool list` | `--json`; `--mount <dir>` when several mounts run |
| `ucfs spool retry [id...]` | `--all`; `--mount <dir>` |
| `ucfs spool drop [id...]` | `--all`; `--mount <dir>`; discards the written content |

## `ucfs index`

| Command | Meaning |
|---|---|
| `ucfs index sync [--full]` | list files uploaded since the last sync, or all with `--full`, and mark removed ones; `--full` also brings in metadata and tag changes other clients made |
| `ucfs index refresh <uuid>...` (**PR #6**) | fetch each file's info, metadata and tags included, into the index; a file the API no longer knows is marked removed |

A running mount shares the index with these commands and sees their result at once.

## `ucfs version`

Prints the version, the commit and build date, the Go version and the platform.
