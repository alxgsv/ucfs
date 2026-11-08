# Releasing ucfs

Everything for a release is in the repository: [.goreleaser.yaml](../.goreleaser.yaml),
[.github/workflows/release.yml](../.github/workflows/release.yml) and [scripts/install.sh](../scripts/install.sh),
verified locally in [phase6.md](phase6.md). The steps below are the repository owner's; none has been run.

## Decisions

Taken on 2026-11-08:

- **Owner:** `alxgsv`. The repository stays `github.com/alxgsv/ucfs`, which is also the Go module path.
- **Licence:** MIT, in [LICENSE](../LICENSE); the archives carry it and the formula states it.
- **No Homebrew in 0.1.0.** goreleaser still builds the formula into `dist/`, but `skip_upload: true` keeps it from
  being pushed anywhere, so no tap or token is needed yet. [Homebrew later](#homebrew-later) has the steps.
- **No Apple signing or notarization.** The darwin/arm64 binary carries the ad-hoc signature the Go linker adds
  (Apple Silicon runs nothing without one); darwin/amd64 carries none, which Intel Macs accept. The install script
  downloads with curl, which sets no quarantine attribute, so Gatekeeper does not check the binary; a browser
  download is quarantined and refused until `xattr -d com.apple.quarantine ucfs`. Measured on a Mac with
  Gatekeeper assessment off; [Testing on a fresh Mac](#testing-on-a-fresh-mac) checks it with Gatekeeper on.
  Signing later needs an Apple Developer account, a Developer ID Application certificate and an App Store Connect
  API key as repository secrets, which goreleaser's `notarize` section uses.

## Releasing 0.1.0

1. Merge the Phase 5 and Phase 6 pull requests.
2. Scan the history for secrets before it goes public (for example `gitleaks git`); the phase reports name the test
   project and its CDN host, which are not secret.
3. Make the repository public:
   `gh repo edit alxgsv/ucfs --visibility public --accept-visibility-change-consequences`.
   The install script fetches from `raw.githubusercontent.com` and the release downloads, both of which need it.
4. Fix the GitHub Actions billing: the account refuses to start jobs ("recent account payments have failed or your
   spending limit needs to be increased"). Without it, release from a clean checkout of the tag instead:
   `GITHUB_TOKEN=$(gh auth token) goreleaser release --clean`.
5. Tag and push: `git tag -s v0.1.0 -m "ucfs 0.1.0" && git push origin v0.1.0`. The workflow runs the tests, builds
   the four archives and publishes the GitHub release with `checksums.txt`. A tag with a pre-release suffix
   (`v0.1.0-rc.1`) makes a pre-release, which is a way to try the whole path first.
6. Test on a fresh Mac, below.

## Testing on a fresh Mac

The point is a Mac whose Gatekeeper is on and that has never run ucfs: `spctl --status` must say
`assessments enabled`. Gatekeeper is a system-wide setting, so a new user account on a Mac where it is off does not
help. Any of these works:

- another Mac, or a new user account on a Mac that has Gatekeeper on;
- a macOS virtual machine on this Mac, for example with [tart](https://tart.run)
  (`brew install cirruslabs/cli/tart`, then `tart clone` of a `macos-…-vanilla` image and `tart run`) or UTM;
- this Mac for the length of the test: `sudo spctl --global-enable`, test, then `sudo spctl --global-disable`,
  which only brings back the "Anywhere" choice under System Settings, Privacy & Security, "Allow applications from";
  choose it again there.

Before the release exists, test the snapshot: `make snapshot` here, then serve it where the test Mac can reach it,
`python3 -m http.server 8000 --directory dist`, and use `http://<this Mac>:8000` in place of the GitHub URLs
below (`--base-url` for the script, Safari for the browser download). A tart VM reaches this Mac at its gateway
address, usually `192.168.64.1`.

On the test Mac, note the macOS version, the architecture (`uname -m`) and `spctl --status`, then:

1. **Install script.**
   ```sh
   curl -fsSL https://raw.githubusercontent.com/alxgsv/ucfs/main/scripts/install.sh | sh -s -- --install-dir ~/.local/bin
   xattr -l ~/.local/bin/ucfs
   ~/.local/bin/ucfs version
   spctl --assess --type execute -vv ~/.local/bin/ucfs
   ```
   Expected: no `com.apple.quarantine` in the `xattr` list, `ucfs version` prints without a dialog. The `spctl`
   answer is for the record: an unnotarized binary is likely "rejected", which does not matter while it is not
   quarantined.
2. **Browser download.** Download `ucfs_0.1.0_darwin_<arch>.tar.gz` from the release page with Safari (if Safari
   unpacks it, skip the `tar` and use what it left), then:
   ```sh
   cd ~/Downloads && mkdir ucfs-dl && tar -xzf ucfs_0.1.0_darwin_*.tar.gz -C ucfs-dl && cd ucfs-dl
   xattr -l ucfs
   ./ucfs version
   ```
   Expected: `com.apple.quarantine` present, and macOS refuses to run `ucfs` with a dialog that it cannot verify it
   (take a screenshot). Then `xattr -d com.apple.quarantine ucfs && ./ucfs version` runs. System Settings, Privacy
   & Security, "Open Anyway" is the other way out.
3. **First mount.** Credentials for a test project in `~/.uploadcare/config.yaml` (mode `0600`), then:
   ```sh
   mkdir ~/uc
   ucfs mount --daemon --read-only --project <test project> ~/uc
   ls ~/uc/files | head
   ucfs status ~/uc
   ucfs umount ~/uc
   ```
   Expected: the mount comes up without `sudo` and without any macOS prompt; note any prompt that does appear (for
   example about network volumes) with a screenshot.
4. **Clean up.** `curl -fsSL https://raw.githubusercontent.com/alxgsv/ucfs/main/scripts/install.sh | UNINSTALL=1 sh -s -- --install-dir ~/.local/bin`,
   `rmdir ~/uc`, `rm -r ~/Downloads/ucfs-dl`, and the config file if it was only for the test. Turn Gatekeeper
   back off if you turned it on for this.

The results belong in `docs/phase6.md` (or the release notes): the outputs above, the dialogs, and what differed
from the expectations.

## Homebrew later

1. Create the tap with a first commit on `main`, which goreleaser pushes to:
   `gh repo create alxgsv/homebrew-tap --public --add-readme --description "Homebrew formulae"`.
2. Create a fine-grained personal access token: resource owner `alxgsv`, repository access only
   `alxgsv/homebrew-tap`, permission Contents: read and write. Store it:
   `gh secret set HOMEBREW_TAP_TOKEN --repo alxgsv/ucfs`.
3. Set `skip_upload: auto` in `.goreleaser.yaml` (pre-releases still skip the tap) and release the next version.
   The workflow then commits `Formula/ucfs.rb` to the tap.
4. Test `brew install alxgsv/tap/ucfs` on a fresh Mac. Installing by the full name also trusts the formula, which
   Homebrew 6 and later require for a third-party tap.

goreleaser deprecates the `brews` section in favour of casks; `goreleaser check` warns on 2.14 and exits 2 on 2.20,
though releases still publish the formula. It stays a formula because a cask has no `test` block and is quarantined
on install.

## Moving to the uploadcare organization later

A GitHub transfer redirects the old URLs, but not `go install` of the old module path, and a tap would move to
`uploadcare/homebrew-tap`. Every place that names the owner (from `rg --hidden alxgsv`, leaving out the history in
`PLAN.md`, `RESEARCH.md` and the phase reports):

- `go.mod` (`module github.com/alxgsv/ucfs`) and the imports in 47 `.go` files;
- `.goreleaser.yaml`: `release.github.owner`, `brews[0].repository.owner`, `homepage`, the caveat's docs URL, the
  header comment;
- `scripts/install.sh`: `REPO` and the usage comment;
- `README.md`: the install script and Homebrew lines;
- `contrib/systemd/ucfs@.service`: `Documentation=`;
- `docs/mount-options.md`, `docs/costs.md`, `docs/troubleshooting.md` and this file: links and commands.

On macOS (BSD `sed`):

```sh
rg -l --hidden -g '!.git' 'alxgsv' -g '!PLAN.md' -g '!RESEARCH.md' -g '!docs/phase*.md' -g '!docs/spikes.md' |
  xargs sed -i '' -e 's#github.com/alxgsv/ucfs#github.com/uploadcare/ucfs#g' -e 's#alxgsv/#uploadcare/#g' -e 's#owner: alxgsv#owner: uploadcare#'
make build test lint && goreleaser check
```

The launchd label `com.uploadcare.ucfs` stays either way.
