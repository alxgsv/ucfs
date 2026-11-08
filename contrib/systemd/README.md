# ucfs as a systemd user unit (Linux)

`ucfs@.service` is a template user unit: the instance name is the escaped mount path, and the unit runs
`ucfs mount` in the foreground (`Type=simple`), stops with `ucfs umount`, and restarts after a failure but not after
a clean unmount. Logs go to the journal.

## Install

The unit runs `/usr/local/bin/ucfs`, where `install.sh` puts it; change both paths if yours is elsewhere
(`command -v ucfs`). Homebrew puts this file in `$(brew --prefix)/share/ucfs/contrib/systemd/`.

```sh
mkdir -p ~/Uploadcare ~/.config/systemd/user
sed "s|/usr/local/bin/ucfs|$(command -v ucfs)|" ucfs@.service > ~/.config/systemd/user/ucfs@.service
systemctl --user daemon-reload
unit="ucfs@$(systemd-escape --path ~/Uploadcare).service"
systemctl --user enable --now "$unit"
ucfs status ~/Uploadcare
```

Mount flags go in `UCFS_MOUNT_FLAGS`, set in a drop-in of the instance (`systemctl --user edit "$unit"` writes the
same file interactively):

```sh
mkdir -p ~/.config/systemd/user/"$unit".d
printf '[Service]\nEnvironment="UCFS_MOUNT_FLAGS=--project my-project --read-only"\n' \
  > ~/.config/systemd/user/"$unit".d/flags.conf
systemctl --user daemon-reload
systemctl --user restart "$unit"
systemctl --user show "$unit" -p Environment
```

The unit reads `~/.uploadcare/config.yaml` like the command line does. A user unit runs only while you are logged
in, unless lingering is on (`loginctl enable-linger`, which may need an administrator).

## Use

- `systemctl --user status "$unit"` and `journalctl --user -u "$unit"` show the state and the log.
- `ucfs umount ~/Uploadcare` unmounts and the unit stays inactive; `systemctl --user start "$unit"` mounts again.
- After a crash the FUSE mount stays behind and the restarted unit fails with "device or resource busy": run
  `ucfs umount ~/Uploadcare`, which runs `fusermount3 -u` when no daemon answers, and the unit mounts again at its
  next restart (every 30 s, `RestartSec`).

## Uninstall

```sh
systemctl --user disable --now "$unit"
rm -f ~/.config/systemd/user/ucfs@.service
rm -rf ~/.config/systemd/user/"$unit".d
systemctl --user daemon-reload
```
