# Troubleshooting

## Where to look

- `ucfs status <dir>` shows the mount's counters, its last sync and the last sync error; `--json` has more
  ([mount-options.md](mount-options.md#ucfs-status-dir---json)).
- Logs: a foreground `ucfs mount` logs to its terminal; `--daemon` to `<cache dir>/mount-<hash>.log`, whose path it
  prints; the launchd example to `~/Library/Logs/ucfs.log`; the systemd unit to `journalctl --user -u <unit>`.
  `-v` adds debug lines.
- `<mount>/.ucfs/uploads.log` lists every upload this cache dir made; `ucfs spool list` what is not uploaded yet.

## A mount is left behind after a crash

When a daemon dies without unmounting (killed or crashed), its mount stays in the mount table with no server behind it. On macOS the NFS mount is `soft`, so programs that touch it get "Operation
timed out" rather than hanging for good; on Linux a FUSE mount whose daemon is gone fails its calls. A new
`ucfs mount` on the same directory then fails: "operation timed out" on macOS, "device or resource busy" on Linux.

Run `ucfs umount <dir>`. When no daemon answers, it forces the mount off: `umount`, `umount -f` and
`diskutil unmount force` on macOS, `fusermount3 -u` and the `umount` variants on Linux. There is no `-f` flag; this
is the only way. Measured: 3.0 s on macOS, at once on Linux, after which the launchd and systemd examples mounted
again by themselves. Files that were queued or uploading are uploaded again by the next mount; a file cut off while
it was being written stays in `ucfs spool list`.

## The registry and control sockets

Each running mount has a control socket, which `status`, `umount`, `resolve` and `spool` talk to:
`<cache dir>/ctl-<hash>.sock`, or, when that path is too long for a socket, `$TMPDIR/ucfs-<hash>-<hash>.sock`
(`$TMPDIR/ucfs-<uid>/<hash>-<hash>.sock` with PR #6). Mounts are recorded in `<user cache dir>/ucfs/mounts/<hash>.json`
(`~/Library/Caches/ucfs/mounts/` on macOS, `~/.cache/ucfs/mounts/` on Linux) with their directory, socket and pid, so
commands find them whatever `--cache-dir` they use. `ucfs status` without a directory removes records whose socket
is gone or refuses connections. A record left by a dead daemon is harmless; the next mount of that directory
replaces it.

## "a ucfs daemon already serves …", "device or resource busy"

The directory is mounted already. `ucfs status <dir>` shows by whom; `mount | grep <dir>` shows a mount without a
daemon, which `ucfs umount <dir>` removes.

## "another writable mount uses <cache dir>"

A writable mount locks its cache dir, so two daemons never upload the same spool. Mount the second one with
`--read-only`, which can share the cache dir, or give it another `--cache-dir`. A daemon that is unmounting keeps the
lock while it finishes uploads, up to 2 minutes.

## Credentials and config

- "no project credentials found": neither the flags, the environment nor `~/.uploadcare/config.yaml` gave both keys.
- "project not found in config": `--project`, `UPLOADCARE_PROJECT` or `default_project` names a project the file does
  not have.
- "config file is readable by group or others" (PR #6): `chmod 600 ~/.uploadcare/config.yaml`, or pass
  `--insecure-config`.

## "Operation denied", usage limits and throttling

The REST API answers `403 Operation denied` when the keys are wrong or the project's API access is suspended, for
example over a usage limit; the CDN may keep serving files meanwhile. Check the project in the
[dashboard](https://app.uploadcare.com). Without an index the first mount fails ("initial index sync"); with one, the
mount keeps serving what the index holds and `ucfs status` shows the sync error. With PR #6 the mount enters degraded
mode once the refusal has lasted a sync interval: reads go on, changes that need the API fail with `EACCES` (`EAGAIN`
when throttled), and the `api` line of `ucfs status` ends with "API denied since <time>: <what the API said> (serving
the index; writes refused)" until the API answers again.

A `429 Too Many Requests` is retried, up to five times for REST requests, honouring `Retry-After`; what still fails
is `EAGAIN`. Over NFS that is `NFS3ERR_JUKEBOX`: the macOS client retries it and gives up after about 28 seconds with
"Operation timed out".

## Uploads

On macOS an upload error does not reach the program that wrote the file. `ucfs spool list` shows failed spools with
their error; `ucfs spool retry --all` tries again; `ucfs spool drop <id>` discards one. Typical errors: "Uploading of
these file types is not allowed" (the project restricts file types; `EPERM`), a file too large for the Upload API
(`EFBIG`),
a name with `\` (`EINVAL` before PR #6). On Linux the same errors come back from `close`.

## Linux

- FUSE needs `fusermount3` (package `fuse3`) and access to `/dev/fuse`. Without `fusermount3`, go-fuse looks for
  `fusermount`, and the mount fails with an error that names it.
- `--frontend nfs` on Linux mounts with `mount -t nfs`, which needs root; use the default `fuse`.
- Only the user who mounted sees the files: the mount has no `allow_other`, so other users and root-run services
  such as backup tools get "Permission denied".

## macOS

- Operations that touch a dead mount time out after a while instead of failing at once (`soft,intr`); see the crash
  section above.
- The mount is not in the Finder sidebar (`nobrowse`); `open <dir>` opens it, `--browse` lists it.
- Finder windows on folders of images cost traffic: [finder.md](finder.md).

## Reporting a problem

Open an issue at <https://github.com/alxgsv/ucfs/issues> with:

- `ucfs version`, the OS and its version, and the front-end;
- the exact command, what you expected and what happened;
- `ucfs status --json <dir>` and the log, ideally with `-v`.

Never include `~/.uploadcare/config.yaml` or the keys. Before PR #6, which redacts the keys from logs and errors,
check what you paste: the default cache dir path contains the project's public key.
