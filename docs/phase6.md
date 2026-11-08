# Phase 6 acceptance: packaging and docs

Measured on 2026-11-08 on branch `phase-6`, cut from `main` at d94413c. The Mac: macOS 27 on arm64, Go 1.26.1,
Homebrew 7.0.12, goreleaser 2.14.3 (Homebrew) and 2.20.0 (`go install …@latest` into a scratch directory). The Linux
host: Ubuntu 26.04.1 on x86_64, `/bin/sh` is dash, systemd 259, no Go. Live checks used `alex-gusev-test`, always with
`--project alex-gusev-test`, `--read-only` and a cache dir of their own (one exception, see the systemd unit), and
ran while Phase 5's soak mounts were up on the Mac, without touching them; they read one 4,859-byte image and
listed names.
`$S` is the session's scratch directory.

| Item | Check | Result |
|---|---|---|
| 39 | `docs/release.md`: owner, licence, Gatekeeper, steps, places to rename | decided after the measurements: `alxgsv`, MIT, no Homebrew in 0.1.0; nothing outward-facing done |
| 40 | `goreleaser check` | 2.14.3: exit 0, deprecation note for `brews`; 2.20.0: exit 2 for the same deprecation only |
| 40 | `make snapshot` | 2.97 s; four archives of 5.32–5.92 MB, `checksums.txt`, formula |
| 40 | `install.sh` from a local server | macOS 0.41 s, Linux 0.065 s; argument tests pass on both shells |
| 40 | generated formula through a local tap | installs in 5.3 s, `brew test` passes, removed cleanly |
| 41 | `ucfs mount --daemon` live | returns in 1.03 s with the mount up; `ls -lR` 0 CDN bytes; `umount` 0.10 s |
| 41 | launchd agent live | mounts in 1 s; stays down after `ucfs umount`; restarts after SIGKILL once the stale mount is cleared |
| 41 | systemd user unit live | same, after two fixes the first run found |
| 42 | docs | flags from `--help` of `main` and of `origin/phase-5`; claims checked against the code |
| 43 | `AGENTS.md`, `CLAUDE.md` | written; `CLAUDE.md` is `@AGENTS.md` |
| all | `make build test lint` | pass; 0 lint issues for darwin and linux |

## 40: release build

`goreleaser check` with the Homebrew build (2.14.3) prints `brews is being phased out in favor of homebrew_casks` and
exits 0. The current release (2.20.0) prints `DEPRECATED: brews should not be used anymore` and `configuration is
valid, but uses deprecated properties`, and exits 2. Its brew pipe still runs (`deprecate.Notice` only), so a release
with the workflow's `~> v2` publishes the formula. The section stays because a cask has no `test` block and is
quarantined on install.

`make snapshot` (`goreleaser release --snapshot --clean --skip=publish`) on the committed tree b3a3674, warm module
cache:

| Artifact | Bytes |
|---|---|
| `ucfs_0.0.0-SNAPSHOT-b3a3674_darwin_amd64.tar.gz` | 5,667,686 |
| `ucfs_0.0.0-SNAPSHOT-b3a3674_darwin_arm64.tar.gz` | 5,321,227 |
| `ucfs_0.0.0-SNAPSHOT-b3a3674_linux_amd64.tar.gz` | 5,920,278 |
| `ucfs_0.0.0-SNAPSHOT-b3a3674_linux_arm64.tar.gz` | 5,447,477 |
| `checksums.txt` | 454 |
| binaries (darwin amd64, darwin arm64, linux amd64, linux arm64) | 13,265,568; 12,497,234; 13,783,202; 12,976,290 |

real 2.97 s, user 4.62 s. Each archive holds `ucfs`, `README.md` and the five `contrib/` files. The darwin/arm64
binary from `dist/` prints
`ucfs 0.0.0-SNAPSHOT-b3a3674 (commit b3a36745…, built 2026-11-08T14:06:12Z, go1.26.1, darwin/arm64)`.
`codesign -dv`: darwin/arm64 `flags=0x20002(adhoc,linker-signed)`, darwin/amd64 `code object is not signed at all`.
The archive step fails when an explicitly listed file is missing (`README.md` before it existed) and accepts a
`LICENSE*` glob that matches nothing, so a licence added later is packed without a config change.

**install.sh.** `go test ./scripts` runs the script against an `httptest` server that serves `checksums.txt` and
archives with a stand-in binary: install from a base URL with no version, `--version=v1.2.3` and `--version 1.2.3`
with the flag beating `INSTALL_DIR`, `VERSION` and `INSTALL_DIR` from the environment, `UNINSTALL=1` and
`--uninstall`, an unknown flag, a flag without a value, a missing version, a missing `checksums.txt`, and a tampered
archive; the last five exit non-zero and install nothing, and no request leaves the base URL. It passes on macOS
(bash as `sh`) and, cross-compiled, on the Linux host (dash).

Against `dist/` served by `python3 -m http.server --bind 127.0.0.1`:

```console
$ sh scripts/install.sh --base-url http://127.0.0.1:18766 --install-dir $S/inst/bin
Installing ucfs version: 0.0.0-SNAPSHOT-d94413c (darwin/arm64)
Downloaded ucfs_0.0.0-SNAPSHOT-d94413c_darwin_arm64.tar.gz
Checksum verified
Installed ucfs 0.0.0-SNAPSHOT-d94413c (commit d94413c…, built 2026-11-08T13:40:38Z, go1.26.1, darwin/arm64) to $S/inst/bin
Warning: $S/inst/bin is not in PATH
```

real 0.41 s; the server saw `GET /checksums.txt` and `GET` of the one archive. `xattr -l` of the installed binary
lists only `com.apple.provenance`, no `com.apple.quarantine`; `spctl --assess --type execute` answers
`accepted, override=security disabled`: Gatekeeper assessment is off on this Mac. On the Linux host, with the
linux/amd64 archive and `checksums.txt` copied to `~/ucfs-test/phase6/dist` and served there on 127.0.0.1, the same
command installed `ucfs … linux/amd64` in 0.065 s.

**Homebrew.** `brew install ./ucfs.rb` is not how Homebrew 7 takes a formula, so the generated one went into a
throwaway tap, its URLs pointed at the local server:

```sh
export HOMEBREW_NO_AUTO_UPDATE=1 HOMEBREW_NO_INSTALL_CLEANUP=1 HOMEBREW_NO_ANALYTICS=1
brew tap-new --no-git ucfsp6/local
sed 's|https://github.com/alxgsv/ucfs/releases/download/v0.0.0/|http://127.0.0.1:18766/|' \
  dist/homebrew/Formula/ucfs.rb > "$(brew --repository ucfsp6/local)/Formula/ucfs.rb"
brew install ucfsp6/local/ucfs     # 5.3 s: 9 files, 12.5 MB; prints the caveats
brew test ucfsp6/local/ucfs        # 13.8 s, passes: runs `ucfs version`
brew uninstall ucfsp6/local/ucfs && brew untap ucfsp6/local
```

`ucfs` was then `/opt/homebrew/bin/ucfs`, ad-hoc signed, with no quarantine attribute, and `share/ucfs/contrib`
held the examples. Installing by the full name added the formula to `~/.homebrew/trust.json` (Homebrew 6 and later
load third-party taps only when trusted), and uninstalling removed it again.

**Workflow.** `.github/workflows/release.yml` parses (Ruby's YAML: checkout, setup-go, `go test ./...`,
goreleaser-action v7). actionlint was not run: installing it was refused in this session. Actions cannot start jobs
on this account, so the workflow itself has not run.

**What the fresh-Mac test still needs:** a Mac with Gatekeeper on, and the release (public repository, tag
`v0.1.0`) or the snapshot served to it: the install script, a browser download, and a first `ucfs mount`. Steps in
[release.md](release.md#testing-on-a-fresh-mac).

**After the decisions** (owner `alxgsv`, MIT, no Homebrew in 0.1.0): with `LICENSE` added, `make snapshot` packs it
into every archive (darwin/arm64 now 5,322,736 bytes), the formula states `license "MIT"`, and `skip_upload: true`
keeps it in `dist/`: goreleaser 2.20's brew pipe returns before it reads the token when that is set.

## 41: persistent mounts

**Unit tests** (`cmd/ucfs/daemon_test.go`, macOS and Linux) start the test binary itself as the daemon:
`spawnDaemon` returns its pid once a control socket answers; the daemon is its own process group leader, has
`/dev/null` as stdin, and appends stdout and stderr to a `0600` log after what an earlier run left; a daemon that
exits first fails the call with its exit status and only this run's log lines; one still starting after the wait
keeps running; cancelling the wait stops it.

**`--daemon` live** (`$S/live-daemon.sh`):

```console
$ ucfs mount --daemon --read-only --project alex-gusev-test --cache-dir $S/live/cache $S/live/mnt
mounted $S/live/mnt: pid 14594, log $S/live/cache/mount-b790e4f3.log
real 1.03
$ ps -o pid=,ppid=,pgid=,stat=,tty= -p 14594
14594     1 14594 Ss   ??
```

`getsid(14594)` is 14594. The mount was `nfs, nodev, nosuid, read-only, nobrowse`. `ls -lR` of the mount took
0.06 s and cost 0 CDN requests; `files/` had 24 entries. Reading one 4,859-byte PNG cost 1 GET and 4,859 bytes;
`api.rest` stayed 1 (the listing at mount). `ucfs umount` returned in 0.10 s, the pid was gone, its registry record
removed, and the log held two lines (`mounted …`, `unmount requested`).

**launchd** (`$S/live-launchd.sh`): the plist made with the README's `sed` plus this test's label
`com.uploadcare.ucfs.p6test`, binary, flags and paths, in `~/Library/LaunchAgents`:

| Step | Seen |
|---|---|
| `plutil -lint`, `launchctl bootstrap gui/501 …` | OK; mounted within 1 s |
| `ucfs umount`, then 40 s | not remounted; `state = not running`, `last exit code = 0` |
| `launchctl kickstart` | mounted again at once |
| `kill -9` of the daemon, then 45 s | restarted; the new daemon exits 1 with `stat …/mnt-launchd: operation timed out` on the stale mount; `spawn scheduled` |
| `ucfs umount` of the stale mount | 3.03 s; the agent mounted again by itself 13 s later |
| `launchctl bootout`, `rm` of the plist | unmounted; `launchctl print` fails; only Phase 5's registry record left |

A second, short run with the final plist mounted (`rest=1`, 0 CDN bytes) and left nothing. It also showed
`exit timeout = 60` for an `ExitTimeOut` of 150: a probe agent running `sleep` kept 45 but showed 60 for 150, so
launchd caps it at 60 s for an agent, and the example now asks for 60.

**systemd** (`~/ucfs-test/phase6/live-systemd.sh` on the Linux host): the unit made with the README's `sed` under
the name `ucfs-p6test@.service`, instance `home-alxgsv-ucfs\x2dtest-phase6-mnt` (the path has a `-`).

The first run found two faults:

- Flags were in `%h/.config/ucfs/%i.env`, which systemd showed under the right name but never read: the journal
  said `Referenced but unset environment variable evaluates to an empty string: UCFS_MOUNT_FLAGS`. A probe unit read
  such a file for an instance without escapes and not for one with `\x2d`. So that mount ran **without** the test's
  flags: writable, on the default cache dir, on the host's default project, which is `alex-gusev-test`. It listed
  the project and `files/`, wrote nothing, uploaded nothing, and the cache dir it created (empty `spool/`,
  `blocks/` and `shadow/`, no `uploads.log`) was removed. Flags now go in a drop-in, and the test checks the
  resolved `Environment` and the journal's `read_only=true` before it goes on.
- After `ucfs umount` the unit restarted: systemd runs `ExecStop` also after the main process exited by itself, the
  `ucfs umount` there found nothing to unmount and exited 1 (`fusermount3: entry … not found in /etc/mtab`), and
  that failure triggered `Restart=on-failure`. `ExecStop` now has the `-` prefix.

The second run, with both fixes:

| Step | Seen |
|---|---|
| drop-in, `daemon-reload` | `Environment="UCFS_MOUNT_FLAGS=--read-only --project alex-gusev-test --cache-dir …"` |
| `systemctl --user enable --now` | mounted within 2 s: `fuse.ucfs ro,nosuid,nodev,noatime,user_id=1000,group_id=1000,max_read=1048576` |
| `ucfs umount`, then 40 s | not remounted: `inactive (dead)`, `NRestarts=0`, `ExecMainStatus=0` |
| `systemctl --user start` | mounted again |
| `kill -9`, then 45 s | `activating (auto-restart)`, `NRestarts=1`, `ExecMainStatus=1`; the journal: `ucfs: mount …: device or resource busy` |
| `ucfs umount` of the stale mount | at once; the unit mounted again by itself 17 s later (`NRestarts=2`) |
| `disable --now`, remove unit and drop-in, `daemon-reload` | unmounted, no unit file, no process, no registry record |

## 42, 43: docs and agent guide

The flag tables in [mount-options.md](mount-options.md) come from `--help` of every command, built from this branch
and from `git archive origin/phase-5` in a scratch directory: Phase 5 adds `--insecure-config`, `--traffic-budget`,
`--max-inflight`, `ucfs index refresh` and the status fields marked "PR #6", and this branch adds `--daemon`. The
claims in each page were checked against the code, and the measured ones against the phase reports;
[semantics.md](semantics.md) marks with † the errors seen on a real mount and says where the macOS NFS client's
translation is not measured. Things found while checking: there is no `ucfs umount -f` (plain `umount` forces a
mount no daemon answers for); `ucfs umount` returns once the daemon closed its socket, while a writable daemon goes on
uploading for up to 2 minutes; truncating a stored file to its own size works on FUSE but is `EROFS` over NFS. All
relative links in the new pages resolve. `docs/phase5.md` is not on any branch yet; the pages point at PR #6 for
what lands with it.

## Live budget and end state

- CDN: 4,859 bytes (one PNG), 1 GET; no HEAD.
- REST: about eleven listings, one per mount start (eleven mounts, each up for less than a minute, so before the
  loop's first sync; a start that failed on a stale mount sent none), against a budget of 300. Uploads: none.
- The Mac has no mount, daemon, launchd job or plist of this phase; its registry holds only Phase 5's record. The
  Homebrew tap is gone and its trust entry removed; the local HTTP servers are stopped.
- The Linux host has no mount, daemon, unit, drop-in or registry record of this phase, and no `~/.config/ucfs`.
  `~/ucfs-test/phase6/` holds the snapshot binary, the scripts and test binaries, and the copied archive, no keys.
