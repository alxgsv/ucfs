# Releasing ucfs

Everything for a release is in the repository: [.goreleaser.yaml](../.goreleaser.yaml),
[.github/workflows/release.yml](../.github/workflows/release.yml) and [scripts/install.sh](../scripts/install.sh),
verified locally in [phase6.md](phase6.md). What is left are decisions only the repository owner can take, and the
steps that follow from them. Nothing below has been done.

## Decision 1: where the repository lives

| | A: `alxgsv/ucfs` (configured) | B: `uploadcare/ucfs` |
|---|---|---|
| Install | `brew install alxgsv/tap/ucfs`; `https://raw.githubusercontent.com/alxgsv/ucfs/main/scripts/install.sh` | `brew install uploadcare/tap/ucfs`; the same URL under `uploadcare` |
| Tap repository | `alxgsv/homebrew-tap`, to create | `uploadcare/homebrew-tap`, to create (neither exists today) |
| Code change | none | the module path in `go.mod` and the imports of 47 Go files, plus the places listed below |
| Release secrets | `HOMEBREW_TAP_TOKEN`, a token of `alxgsv` | a token an org owner issues, or a GitHub App the org installs on the tap; the org decides who holds it |
| Actions minutes | the personal account's, refused today for billing | the org's |
| Reads as | a personal project | an Uploadcare product, next to [uploadcare-cli](https://github.com/uploadcare/uploadcare-cli) |

Moving later is a GitHub transfer (it redirects the old URLs, but not `go install` of the old module path) and the
search-and-replace below; the tap moves with a new repository and the old formula pointing users at the new one.

Every place that names the owner, for that search-and-replace (from `rg --hidden alxgsv`, leaving out the
history in `PLAN.md`, `RESEARCH.md` and the phase reports):

- `go.mod` (`module github.com/alxgsv/ucfs`) and the imports in 47 `.go` files;
- `.goreleaser.yaml`: `release.github.owner`, `brews[0].repository.owner`, `homepage`, the caveat's docs URL, the
  header comment;
- `scripts/install.sh`: `REPO` and the usage comment;
- `README.md`: the `brew install` and install script lines;
- `contrib/systemd/ucfs@.service`: `Documentation=`;
- `docs/mount-options.md`, `docs/costs.md` and `docs/troubleshooting.md`: links to pull requests and issues.

On macOS (BSD `sed`):

```sh
rg -l --hidden -g '!.git' 'alxgsv' -g '!PLAN.md' -g '!RESEARCH.md' -g '!docs/phase*.md' -g '!docs/spikes.md' -g '!docs/release.md' |
  xargs sed -i '' -e 's#github.com/alxgsv/ucfs#github.com/uploadcare/ucfs#g' -e 's#alxgsv/#uploadcare/#g' -e 's#owner: alxgsv#owner: uploadcare#'
make build test lint && goreleaser check
```

The launchd label `com.uploadcare.ucfs` stays either way.

## Decision 2: the licence

The repository has no `LICENSE`, so even once public nobody may use the code. uploadcare-cli is MIT-licensed
("Copyright (c) 2026 Uploadcare, Inc."), which makes MIT with the same holder the natural default for B; for A the
holder is the owner's call. Once chosen:

- add `LICENSE` at the root; the archives already take `LICENSE*`;
- add `license: MIT` (or the chosen SPDX id) to `brews[0]` in `.goreleaser.yaml`, so the formula states it.

## Decision 3: Apple signing and notarization

The binaries are not signed with a Developer ID or notarized. What was measured on this Mac:

- the `darwin_arm64` binary carries the ad-hoc signature the Go linker adds (Apple Silicon runs nothing without one);
  `darwin_amd64` carries none, which Intel Macs accept;
- installed by `install.sh` (curl) or Homebrew, the binary has no `com.apple.quarantine` attribute, only
  `com.apple.provenance`, so Gatekeeper does not check it on first run;
- Gatekeeper assessment is turned off on this Mac (`spctl` answers "override=security disabled"), so this Mac cannot
  show what a fresh Mac does; that needs the first real release on a fresh Mac.

A browser download is quarantined, and macOS then refuses to run the binary until
`xattr -d com.apple.quarantine ucfs`. Shipping unsigned is the usual state for a CLI installed through Homebrew or a
script. Signing and notarizing needs an Apple Developer account (Uploadcare's or the owner's), a Developer ID
Application certificate and an App Store Connect API key as repository secrets; goreleaser's `notarize` section
uses them.

## Decision 4: CI minutes

GitHub Actions refuses to start jobs on the personal account ("recent account payments have failed or your spending
limit needs to be increased"). The release workflow needs it, or the release is run by hand: `goreleaser release
--clean` with `GITHUB_TOKEN` and `HOMEBREW_TAP_TOKEN` set, from a clean checkout of the tag.

## Steps for the first release (option A)

1. Merge the Phase 5 and Phase 6 pull requests.
2. Add the licence (decision 2) and commit it.
3. Scan the history for secrets before it goes public (for example `gitleaks git`); the phase reports name the test
   project and its CDN host, which are not secret.
4. Make the repository public:
   `gh repo edit alxgsv/ucfs --visibility public --accept-visibility-change-consequences`.
5. Create the tap with a first commit on `main`, which goreleaser pushes to:
   `gh repo create alxgsv/homebrew-tap --public --add-readme --description "Homebrew formulae"`.
6. Create a fine-grained personal access token: resource owner `alxgsv`, repository access only
   `alxgsv/homebrew-tap`, permission Contents: read and write. Store it:
   `gh secret set HOMEBREW_TAP_TOKEN --repo alxgsv/ucfs`.
7. Fix the Actions billing (decision 4).
8. Tag and push: `git tag -s v0.1.0 -m "ucfs 0.1.0" && git push origin v0.1.0`. The workflow runs the tests, builds
   the four archives, publishes the GitHub release with `checksums.txt`, and commits `Formula/ucfs.rb` to the tap.
   A tag with a pre-release suffix (`v0.1.0-rc.1`) makes a pre-release and leaves the tap alone.
9. On a fresh Mac: `brew install alxgsv/tap/ucfs`, `ucfs version`, `xattr -l "$(command -v ucfs)"`,
   `mkdir ~/uc && ucfs mount --daemon ~/uc && ls ~/uc/files && ucfs umount ~/uc`; then the same with
   `curl -fsSL https://raw.githubusercontent.com/alxgsv/ucfs/main/scripts/install.sh | sh -s -- --install-dir ~/.local/bin`.
   On Linux, the install script and `fuse3`.

`make snapshot` builds the same `dist/` locally without publishing; `goreleaser check` validates the config
(goreleaser 2.20 exits 2 on it only because the `brews` section is deprecated, see phase6.md).
