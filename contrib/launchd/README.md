# ucfs as a launchd agent (macOS)

`com.uploadcare.ucfs.example.plist` runs `ucfs mount` in the foreground as a user agent: it mounts at login,
restarts after a failure, and stays down after `ucfs umount`. Logs go to `~/Library/Logs/ucfs.log`.

## Install

```sh
mkdir -p ~/Uploadcare ~/Library/Logs ~/Library/LaunchAgents
sed -e "s|/Users/YOU|$HOME|g" -e "s|/opt/homebrew/bin/ucfs|$(command -v ucfs)|" \
  com.uploadcare.ucfs.example.plist > ~/Library/LaunchAgents/com.uploadcare.ucfs.plist
plutil -lint ~/Library/LaunchAgents/com.uploadcare.ucfs.plist
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.uploadcare.ucfs.plist
ucfs status ~/Uploadcare
```

Homebrew puts this file in `$(brew --prefix)/share/ucfs/contrib/launchd/`. Add flags such as `--project`,
`--read-only` or `--views` as more `<string>` elements after `mount`, before the mount directory. The agent reads
`~/.uploadcare/config.yaml` like the command line does; launchd passes no shell environment, so
`UPLOADCARE_*` variables set in a shell profile do not reach it (use an `EnvironmentVariables` dictionary).

For a second mount, copy the plist with another `Label`, file name, mount directory and log path.

## Use

- `ucfs umount ~/Uploadcare` unmounts and the agent stays stopped until the next login or
  `launchctl kickstart gui/$(id -u)/com.uploadcare.ucfs`.
- `launchctl print gui/$(id -u)/com.uploadcare.ucfs` shows its state, pid and last exit status.
- After a crash the old mount stays behind and the restarted agent fails with "operation timed out" on it: run
  `ucfs umount ~/Uploadcare`, which forces a mount no daemon answers for off, and the agent mounts again at its
  next try (every 30 s, `ThrottleInterval`).

## Uninstall

```sh
launchctl bootout gui/$(id -u)/com.uploadcare.ucfs
rm ~/Library/LaunchAgents/com.uploadcare.ucfs.plist
```

`bootout` stops the daemon with SIGTERM, which unmounts.
