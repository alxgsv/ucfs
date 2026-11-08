# AGENTS.md

Guide for coding agents and contributors working on ucfs, which mounts an Uploadcare project as a filesystem
([README.md](README.md)). [PLAN.md](PLAN.md) is the plan and the record of what each phase built;
[RESEARCH.md](RESEARCH.md) holds the API and CDN facts the design rests on; `docs/phaseN.md` are the measured
acceptance reports.

## Layout

- `cmd/ucfs/`: the cobra commands (`mount`, `umount`, `status`, `resolve`, `spool`, `index`, `version`).
- `internal/vfs/`: the filesystem model every front-end shares; all behaviour lives here and is tested without a
  mount. `internal/frontend/nfs` (macOS, loopback NFSv3) and `internal/frontend/fuse` (Linux) are thin adapters.
- `internal/index` (SQLite), `cache` (block cache, prefetch), `cdn`, `api` (REST and Upload API), `spool` (writes),
  `ctl` (control socket and mount registry), `config`, and `fake` (test doubles of REST, Upload API and CDN).
- `third_party/go-nfs/`: go-nfs at a pinned commit with ucfs's patches, listed in its `README.ucfs.md`; wired in by a
  `replace` in `go.mod`. Change it only with a note there.
- `scripts/install.sh`, `.goreleaser.yaml`, `.github/workflows/`: packaging ([docs/release.md](docs/release.md)).
- `contrib/`: launchd and systemd examples. `spikes/`: measurement rigs and scripts from the phases.

## Build, test, lint

Go 1.26 with `CGO_ENABLED=0` (the Makefile exports it); golangci-lint v2 with `.golangci.yml`.

```sh
make build        # bin/ucfs
make test         # go test ./..., against internal/fake: no keys, no network
make lint         # golangci-lint for darwin and for linux; both must report 0 issues
make cross        # builds for linux/amd64, linux/arm64, darwin/amd64
make snapshot     # goreleaser: dist/ with the release archives and formula, nothing published
```

In a git worktree inside another checkout (such as `.claude/worktrees/<name>`), build with
`GOFLAGS=-buildvcs=false`: otherwise Go stamps the binary from the outer checkout and runs `git status` in it.

`make build test lint` must pass before every commit. `make race` runs the tests with the race detector, which needs
cgo; run it on Linux, as CI does, for example in an OrbStack VM with Go, since the FUSE tests only run there. `make bench` prints the benchmark table and `make soak SOAK=24h` runs the soak against the fake.

Tests that mount: the FUSE tests in `internal/frontend/fuse` skip without a usable `/dev/fuse`, and fail instead with
`UCFS_MOUNT_TESTS=1`, as CI sets; the `cmd/ucfs` tests that mount a fake project for real (NFS on macOS, FUSE on
Linux) run only with `UCFS_MOUNT_TESTS=1`. To run tests on a Linux host without Go, cross-compile them:
`GOOS=linux go test -c -o fuse.test ./internal/frontend/fuse/` and copy the binary there.

## Live checks

There is no live test in `go test`: everything there runs against `internal/fake`. Acceptance is measured by hand
against a real project, with the scripts in `spikes/measure/` (they take the binary in `UCFS_BIN` and global flags
in `UCFS_FLAGS`) and the soak's `UCFS_SOAK_LIVE=<project>`. They need:

- credentials in `~/.uploadcare/config.yaml` for a project the maintainer names for the task. Pass `--project` on
  every command: the default project may be another one. Never use a project you were told is limited.
- an own `--cache-dir` under a scratch directory, never the default one;
- `--read-only` unless the check is about writes.

Rules for anything that touches a real project:

- **Secrets.** Never print, log, paste or commit anything from `~/.uploadcare/config.yaml`. The default cache dir is
  named after the public key, so keep its path out of docs and logs you share; use `--cache-dir`.
- **Other files are not yours.** Files you did not upload must not be read in full, tagged, renamed, overwritten or
  deleted; `ls` and `stat` cost nothing, a `cat`, a Finder window or a thumbnail does (see [docs/costs.md](docs/costs.md)).
  A mount without `--allow-foreign` already refuses to change them.
- **Mark test uploads.** Upload with metadata `ucfs:test=1`: mount with `--upload-metadata ucfs:test=1`, or
  `uploadcare file upload … --metadata ucfs:test=1`. Delete every test upload when done, with `rm` through a writable
  mount or the CLI, and check with a search for that metadata that none is left (`go run ./spikes/rest -project <p>
  …`, as in [docs/phase4.md](docs/phase4.md)). Record the project's file list before and diff it after.
- **Leave nothing mounted.** Unmount in the same step that mounted, also on failure (a `trap`), and check
  `ucfs status --json <dir>` for what the step cost before unmounting. Always pass your own mount dir to `status`,
  `umount` and `spool --mount`: without it they act on every registered mount, which may be someone else's. Never
  touch mounts, daemons, sockets or registry records you did not create.
- Budget live work: say up front how many CDN bytes, REST requests and uploads a check may use, and compare with the
  counters after.

Where things are:

- Mount registry: `<user cache dir>/ucfs/mounts/<hash>.json` (`~/Library/Caches/ucfs/mounts/` on macOS,
  `~/.cache/ucfs/mounts/` on Linux), one record per running mount with its directory, socket and pid.
- Control sockets: `<cache dir>/ctl-<hash>.sock`, or `$TMPDIR/ucfs-<uid>/<hash>-<hash>.sock` when that path is too
  long.
- `--daemon` logs: `<cache dir>/mount-<hash>.log`.
- A mount left by a dead daemon: `ucfs umount <dir>` forces it off ([docs/troubleshooting.md](docs/troubleshooting.md)).

## Conventions

- **One phase, one branch, one pull request.** Phase N is developed on `phase-N` and merged to `main` through one
  PR whose description is the phase's acceptance checklist. The phase rewrites its items in `PLAN.md` to what was
  built, with a "Measured in" line, and writes `docs/phaseN.md` with the measurements: the exact commands, the
  numbers, and the end state of the project and hosts.
- **Commits** are signed (`commit.gpgsign=true`, SSH keys through 1Password): every commit needs the maintainer's
  approval in a 1Password prompt, so make few, meaningful commits, say when one is coming, and never disable
  signing. Match the history's style: an imperative subject in sentence case without a prefix, and a body that says
  why. Stage paths explicitly, never `git add .` or `-A`. Do not force-push; once pushed, add commits instead of
  amending.
- **Code.** Comments say why, not what. Errors carry errnos the front-ends pass through (`internal/api/errno.go`);
  new behaviour goes into `internal/vfs` with a test against the fake. Every user-visible change updates the docs
  in `docs/` that describe it.
- **Outward-facing actions** (making the repository public, creating repositories, tags, releases, secrets,
  choosing a licence) are the maintainer's; prepare them, do not run them.
