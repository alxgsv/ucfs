# Finder and Spotlight

On macOS ucfs is an NFS mount, and Finder treats it like a network share: it browses it, writes its own files into
it, and reads images to draw thumbnails. This page says what that costs and what ucfs does with what Finder writes.
The findings are from [Spike C](spikes.md) and the [Phase 1](phase1.md) and [Phase 3](phase3.md) reports.

## Seeing the mount

The mount is made with `nobrowse`, so it does not appear in the Finder sidebar or on the desktop; `open ~/Uploadcare`
still opens a Finder window on it. `--browse` mounts it without `nobrowse`.

## Thumbnails read whole files

Finder reads **every image it shows a thumbnail for, in full**. A folder of 7 images opened in icon view had all 7
read; opening `files/` on a mount read the 15 visible files, 23,288,128 bytes, and none of those outside the window. A fresh
16 MB JPEG was read to the last byte on a read-only mount with read-ahead off. The test images had no embedded EXIF
thumbnail; whether a camera JPEG's embedded thumbnail spares the read was not tested.

Quick Look keeps thumbnails across mounts and mount paths, so a later visit reads only images it has not drawn yet,
and an image in the block cache costs no CDN traffic again. No marker file or mount option stops the reads: a
`.ql_disablethumbnails` file at the root did not. To keep a Finder window cheap, open folders with few images, such
as a `tree/` folder, rather than `files/` or `by-date/`; [costs.md](costs.md) has the traffic budget that caps a day's
traffic. Finder in a variant directory (`files/<uuid>/-/…`) was never observed.

## Spotlight

Spotlight does not index the mount: `mdutil -s` reports "Indexing disabled" on it, as on NFS mounts in general. The
root also always holds an empty, read-only `.metadata_never_index`, which asks Spotlight to stay out.

## What Finder writes

On a writable mount Finder writes `.DS_Store` files with its view settings, and `cp` and Finder copies write an
AppleDouble `._<name>` beside every file they create: macOS stores extended attributes there on a filesystem that has
none, and every file copied on macOS 27 has at least `com.apple.provenance`. These names, and `.Spotlight-V100`,
`.fseventsd`, `.Trashes`, `.TemporaryItems` and vim's swap files (`.<name>.swp` and the like), are **shadow files**:

- they are kept in `<cache dir>/shadow/` and never uploaded;
- they list and read like other files, in the root, `files/` and `tree/`, but only on this machine and only with this
  cache dir, so Finder tags and other extended attributes copied with a file do not reach the project;
- creating one in `files/<uuid>/`, `by-date/`, `tags/` or `groups/` fails with "Read-only file system";
- renaming one to an ordinary name in `tree/` or `files/` uploads it.

A Finder copy of a folder of 50 images into `tree/` made 50 uploads, wrote 51 shadow files, uploaded none of them,
and sent no REST request; Finder finished in 0.39 s and all uploads were done after 6.6 s.

`--read-only` (the `rdonly` mount option) stops Finder writing anything: no `.DS_Store`, no `._*`, and no attribute
changes after thumbnails. Reads are unchanged.

## Saving from applications

- Uploads start 300 ms after the NFS client commits a file, or 3 s after its last write (`--commit-idle`). An
  upload that fails is not reported to the application, which has already closed the file: `ucfs spool list` shows
  it and `ucfs spool retry` uploads it again.
- Apps that save through `NSDocument` (TextEdit and others) write into a scratch folder
  `(A Document Being Saved By <app>)` beside the file and rename the result over it. In `tree/` that lands as one
  upload under the final name, and the old version is deleted (`--on-overwrite`). This was measured through
  `FileManager.replaceItemAt`, the call such saves use; TextEdit itself stopped at a dialog during the test.
- vim on macOS saves in place; its swap files stay local.

## New files from elsewhere

The mount lists files other clients upload after its next sync, every 30 seconds (`--sync-interval`). The NFS client
caches a directory's attributes for up to 5 seconds (`acdirmax=5`) and a file's for up to an hour (`acregmax=3600`,
safe because uploaded files never change), so a listing can lag the index by those 5 seconds.
