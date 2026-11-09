# ucfs

ucfs mounts one [Uploadcare](https://uploadcare.com) project as a local filesystem on macOS and Linux. Listing is
served from a local index and costs no requests; reading a file fetches it from the Uploadcare CDN in 1 MiB blocks
and keeps them on disk; image transformations are paths you can `cp`; files you copy into the mount are uploaded.

```console
$ ucfs mount --daemon ~/Uploadcare
mounted /Users/me/Uploadcare: pid 14594, log /Users/me/Library/Caches/ucfs/<public key>/mount-b790e4f3.log
$ ls ~/Uploadcare
by-date  files  tree
$ cp ~/Uploadcare/files/<uuid>/-/resize/1200x/-/format/webp/hero.webp .
$ mkdir ~/Uploadcare/tree/pets && cp cat.jpg ~/Uploadcare/tree/pets/
$ ucfs resolve ~/Uploadcare/tree/pets/cat.jpg
```

**Status: experimental.** Not for data you cannot afford to lose. The [plan](PLAN.md) describes what is built and the
[phase reports](docs/) what was measured.

## Install

macOS needs nothing else: ucfs serves NFS on 127.0.0.1 and mounts it with the built-in `mount_nfs`, as you, without
`sudo` or a kernel extension. Linux needs FUSE 3 (`fusermount3`, package `fuse3`).

- Install script, once a release is published ([docs/release.md](docs/release.md)):
  `curl -fsSL https://raw.githubusercontent.com/alxgsv/ucfs/main/scripts/install.sh | sh` puts `ucfs` in
  `/usr/local/bin` (with `sudo` if that is not writable); `... | sh -s -- --install-dir ~/.local/bin` does without
  `sudo`. `--version 0.1.0` picks a version, `UNINSTALL=1` removes it.
- From source, with Go 1.26: `make build` writes `bin/ucfs`.
- Homebrew comes after 0.1.0, as `brew install alxgsv/tap/ucfs`.

The binaries are not signed by Apple or notarized. The install script does not mark them as downloaded, so
Gatekeeper does not stop them; an archive downloaded with a browser is quarantined like any download, and macOS
refuses to run it until `xattr -d com.apple.quarantine ucfs`.

## Credentials

ucfs reads the [Uploadcare CLI](https://github.com/uploadcare/uploadcare-cli)'s `~/.uploadcare/config.yaml`, in the
CLI's order: `--public-key`/`--secret-key` or `UPLOADCARE_PUBLIC_KEY`/`UPLOADCARE_SECRET_KEY`, then the project named
by `--project` or `UPLOADCARE_PROJECT`, then the file's `default_project`, then its top-level keys. The secret key is
needed: listing goes through the REST API.

## Quick start

```sh
mkdir -p ~/Uploadcare
ucfs mount --daemon ~/Uploadcare      # or without --daemon, in the foreground until Ctrl-C
ls -l ~/Uploadcare/files | head
ucfs status ~/Uploadcare              # requests, bytes, cache, uploads
ucfs umount ~/Uploadcare
```

The first mount lists the whole project into the index (one request per 1,000 files), then lists new uploads every
30 seconds. To mount at login, see [contrib/launchd](contrib/launchd/README.md) (macOS) and
[contrib/systemd](contrib/systemd/README.md) (Linux).

## What is in the mount

| Path | What it holds | Writes |
|---|---|---|
| `files/<uuid>_<name>` | every stored file | read-only; `rm` with `--allow-foreign` |
| `files/<name>` (new) | a file you write there | uploaded; it then lists as `<uuid>_<name>`, and the written name keeps resolving for an hour (`--alias-ttl`) |
| `files/<uuid>/-/<operation>/<args>/…/<name>` | a [URL API](https://uploadcare.com/docs/transformations/image/) variant, such as `files/<uuid>/-/resize/1200x/-/format/webp/hero.webp`; nothing is listed under `-/`, and only the URL API's operation names resolve there | read-only |
| `tree/<dir>/<name>` | files by their `ucfs:path` metadata; `tree/_inbox/` holds files without one, by original name | `cp` uploads, `mv` changes `ucfs:path`, `rm` deletes, `mkdir` makes a local folder |
| `by-date/YYYY/MM/DD/<uuid>_<name>` | files by upload day (`--by-date-zone`) | read-only |
| `tags/<tag>/<uuid>_<name>` | files by tag; opt-in with `--views` | read-only |
| `groups/<uuid>~<n>/<index>_<name>` | file groups in group order; opt-in | read-only |
| `.ucfs/uploads.log` | one line per upload: time, path, uuid, size, type | read-only |

`--views files,tree,by-date` is the default; `--views files,tree,by-date,tags,groups` mounts all five.

**Writes.** A file written in `tree/` or `files/` is kept in a local spool and uploaded when it is complete: on Linux
when it is closed, which waits for the upload and returns its error; on macOS 300 ms after the NFS client commits it,
or 3 s after its last write, and an error is only logged (`ucfs spool list` shows what failed). The upload stores the
written name as the file's name, `ucfs:path` for `tree/`, and `--upload-metadata` pairs. Writing over an uploaded file
uploads a new one, copies the old one's metadata and tags, and deletes the old one (`--on-overwrite=keep` keeps it),
so the uuid changes. `ucfs resolve <path>` prints the uuid and CDN URL a write got. Files that other clients
uploaded and that have no `ucfs:path` cannot be removed, overwritten or moved unless the mount has `--allow-foreign`,
so a stray `rm -r` cannot empty a project. `--read-only` refuses every write.

**Metadata.** On Linux, metadata, tags and file facts are extended attributes: `getfattr -d tree/x.jpg`, and
`setfattr -n user.uc.meta.camera -v Canon tree/x.jpg` sets a key. macOS NFS has no extended attributes; use
`ucfs resolve --json`.

## Caveats

- The project is the source of truth: files other clients upload appear after the next sync (30 s), and their
  metadata and tag changes after `--meta-ttl` (5 min) or `ucfs index sync --full`.
- Finder and Quick Look read every image they show a thumbnail for, in full. See [docs/finder.md](docs/finder.md)
  and [docs/costs.md](docs/costs.md) for what costs CDN traffic; `--traffic-budget 2GiB` caps a day's.
- No hard links, symbolic links or special files. `chmod`, `chown` and `touch` succeed and change nothing; a
  file's time is its upload time. Renaming a directory updates each file below it, one request each, and is not
  atomic. Empty files are not uploaded. [docs/semantics.md](docs/semantics.md) has the details.
- On macOS the mount is served by NFS on a loopback port. Other users cannot use the mount point, but any local
  process that finds the port can read the project through it ([PLAN.md](PLAN.md) item 45).
- A daemon that crashes leaves its mount behind; `ucfs umount <dir>` forces it off
  ([docs/troubleshooting.md](docs/troubleshooting.md)).

## Documentation

- [Mount options](docs/mount-options.md): every command and flag with its default.
- [Finder and Spotlight](docs/finder.md), [costs](docs/costs.md), [semantics](docs/semantics.md),
  [troubleshooting](docs/troubleshooting.md).
- [Releasing](docs/release.md): the decisions and steps before the first public release.
- [AGENTS.md](AGENTS.md): building, testing and conventions for contributors and coding agents.
- [PLAN.md](PLAN.md), [RESEARCH.md](RESEARCH.md) and the phase reports in [docs/](docs/): design and measurements.

## License

[MIT](LICENSE).
