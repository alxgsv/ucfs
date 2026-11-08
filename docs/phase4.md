# Phase 4 acceptance: metadata, tags, groups, dates

Measured on 2026-11-08 against `alex-gusev-test` (Uploadcare name `alex_gusev_test`, autostore on, the tags feature on),
on the same two hosts as [Phase 1](phase1.md) to [Phase 3](phase3.md):

- **macOS:** Apple Silicon Mac, macOS 27.0.1, NFS front-end, nothing installed.
- **Linux:** x86_64 host, Ubuntu 26.04.1 with kernel 7.0, FUSE front-end, mounted as a normal user through
  `fusermount3`.

`uploadcare-go-test`, the project of the earlier phases, answers every REST request with 403 since its usage limit was
hit, so this phase moved to `alex-gusev-test`. That project holds 18 files of the user's: 6 PNGs (4.9 KB to 7.9 MB,
three named `SCR-20260611-ojir.png`) and 12 copies of a 244 MB WebM, 2.9 GB in all, none with metadata or tags. Their
uuids, names, sizes, metadata and tags were recorded before the first request of this phase and compared at the end
(*End state*); none of their attributes was read or set (only `ls -l` listed their names, from the index), and the
videos were never read.

Test files were uploaded for the measurements: photo-like JPEGs from [`spikes/photogen`](../spikes/photogen/main.go)
(800×600, 19.7–22.4 KB; one of 2000×1500, 134,861 B; one of 6700×4470, 12,204,021 B for a multipart upload), each with
metadata `ucfs:test=1` (`--upload-metadata ucfs:test=1` on the mounts, `--metadata ucfs:test=1` with the CLI), about
12.7 MB in all; all were deleted at the end, and so were the groups made from them. Request counts come from
`ucfs status --json`, which now counts info refreshes, xattr calls, md5 HEADs and group reads; the script is
[`spikes/measure/meta.sh`](../spikes/measure/meta.sh) (`cost` runs a command and prints what it cost the mount,
`watch` polls one attribute). [`spikes/rest`](../spikes/rest/main.go) sends one raw REST request with the project's
credentials, for what the `uploadcare` CLI has no command for: tags, search and raw file info (the CLI's
`file info --json` has no `tags` field).

The Linux host has no `getfattr`/`setfattr` (package `attr`) and no sudo, so `apt download attr` and `dpkg -x`
unpacked attr 2.5.2 into a directory of the test user's; nothing was installed. Its `ls` is uutils coreutils 0.10.0,
`cp` GNU coreutils 9.7, `rsync` 3.4.1. The Linux mount ran with every view, `--meta-ttl 60s` and `--sweep-interval 1m`,
so the TTL and the group sync could be watched in minutes; the Mac's with the default views and `--meta-ttl 30s`.

## Acceptance

| Criterion | Linux, FUSE | macOS, NFS |
|---|---|---|
| `setfattr -n user.uc.meta.camera -v Canon tree/p4lin/p4-1.jpg` reaches the API | yes: 1 REST request, 0.21–0.29 s; `uploadcare metadata list` shows `camera Canon` | no xattrs over NFSv3 |
| `getfattr -d` shows the read-only attributes with correct values | yes: uuid, cdn_url, mime, md5 (= `md5sum`), 800×600 (= the JPEG header), stored | — |
| `setfattr -x user.uc.meta.camera` removes the key | yes: 1 `DELETE`; gone from the API; again: "No such attribute" | — |
| `tags/p4-approved/` lists exactly the search result for `tags.all=[p4-approved]` | yes: the same 4 uuids once the search index settled | — |
| `groups/<id>/` lists a new group in index order | yes: `0_p4-3.jpg` … `3_p4-2.jpg`, as created; 1 REST request | — |
| `by-date/` shows today's uploads under today, the 18 fixtures under theirs | yes, the same as macOS | yes: 2026/06/11 (15), 2026/08/11 (3), 2026/11/08 (test files) |
| Cost of listing all of `by-date/` (`ls -lR`) | 0 CDN requests, 0 B, 0 REST, 12 ms | 0 CDN requests, 0 B, 0 REST |
| `getfattr -d` with the entry past the TTL (refetched) versus within it (cached) | past the TTL: 1 REST, 0.207–0.208 s; cached: 0 requests, 7–8 ms | — |
| A metadata change by the CLI shows within the TTL | yes, `getfattr`: 45.4 s after the change, 60 s after the last refresh (TTL 60 s), one refresh | yes, `ucfs resolve`: 20.4 s after the change, TTL 30 s, one refresh |
| `ucfs resolve --json` prints metadata and tags of a written file | yes | yes |

## What the API does (probed on test uploads)

Each limit was tried once against a test upload with `spikes/rest` before the design was fixed:

| Request | Answer |
|---|---|
| `PUT /files/{uuid}/tags/` `["Probe-A"," x ","probe-a"]` | 200, stored `["probe-a","x"]`: lowercased, trimmed, deduplicated |
| A tag with `,`, a non-Latin letter, 101 characters, an empty tag, 51 tags | 400 each: "Tag contains invalid characters…", "Tag too long: 101 chars (max 100).", "This field may not be blank.", "Too many tags: 51 (max 50)." |
| File info on this project | carries `"tags": []` (the previous project's had no `tags` field at all) |
| `PUT /files/{uuid}/metadata/{key}/` with a 64-character key | 200; 65 characters: 400 "Length of key … is exceeded. The limit is 64." |
| A key with a space or `+` | 404, an HTML "Page not found": such keys do not route |
| A value of 512 characters, also 512 `é` (1,024 bytes) | 200; 513 characters: 400 "Value length of key … is exceeded. The limit is 512." |
| An empty value | 400 "Value of key … can not be empty." |
| A 51st key | 400 "Number of metadata keys per file has been exceeded. The limit is 50."; replacing a key at 50 works |
| `DELETE /files/{uuid}/metadata/{key}/` of a missing key | 204, the same as for an existing one |
| `POST /files/search/` `{"tags":{"all":["probe-a"]}}` | the tagged file within seconds of tagging, `total` and file info with `tags` |
| `GET /groups/{id}/` | group info with `files`, each a full file info with metadata and tags |
| `uploadcare group create A B A` | a group of 2: the CLI drops the repeated uuid |
| CDN `HEAD` of a base upload | `ETag: "<content MD5>"` (66a06adc… for a 22,444 B file) |
| CDN `HEAD` of a multipart upload (12,204,021 B, 3 parts) | `ETag: "af921799bcddb0519d6f49b3a8b3cced-3"`, not the content MD5 (6893f860…) |

So the errnos are: a key over 64 characters `ENAMETOOLONG`; a key outside `[\w\-\.\:]+`, an empty value or one that is
not UTF-8 `EINVAL`; a value over 512 characters `E2BIG` (characters, as the API counts, not bytes); a 51st key or tag
`ENOSPC`, mapped from the API's message because the index may not know keys other clients added; a tag over 100
characters `ENAMETOOLONG`, other bad tags `EINVAL`.

## macOS

A mount with `--meta-ttl 30s` and the default views (`files`, `tree`, `by-date`) indexed 20 files with one REST request.

**`ucfs resolve --json`** of `tree/p4mac/p4-1.jpg`, written with `cp` (uploaded in 1.25 s):

```json
{
  "path": "tree/p4mac/p4-1.jpg",
  "state": "uploaded",
  "uuid": "1613d2a6-d3e5-4c20-9d00-470b64c095c3",
  "cdn_url": "https://44bsymnfbi.ucarecd.net/1613d2a6-d3e5-4c20-9d00-470b64c095c3/",
  "name": "p4-1.jpg",
  "size": 22250,
  "mime": "image/jpeg",
  "metadata": {
    "ucfs:path": "/p4mac/p4-1.jpg",
    "ucfs:test": "1"
  },
  "tags": []
}
```

**Changes by other clients.** `uploadcare metadata set <uuid> lens 50mm` and a tags `PUT` of `["p4-mac"]` through
`spikes/rest`, then `ucfs resolve --json` every 3 s:

| Since the change | `metadata.lens` | `tags` | REST requests of the mount |
|---|---|---|---|
| 1.6 s to 17.3 s (6 reads) | absent | `[]` | 3 |
| 20.4 s | `50mm` | `["p4-mac"]` | 5 |

The index entry was written by the upload about 10 s before the change; once it was 30 s old, the next resolve read the
file's info again (one request; the other was the 30 s sync listing). Reads within the TTL cost nothing.

**`by-date/` over NFS.** `ls -lR by-date` listed `2026/06/11` (the 12 videos and the three `SCR-…` PNGs, uploaded on
2026-06-11 between 13:32 and 22:43 local time), `2026/08/11` (the three other PNGs) and `2026/11/08` (the test
uploads), in the Mac's zone (UTC+6), with 0 CDN requests, 0 bytes and 0 REST requests. The directory tree comes from
the index: each year, month and day is one probe of the `uploaded_at` index.

**Multipart and md5.** `cp` of the 12.2 MB image returned in 0.021 s; the upload went in 3 parts in 12.9 s. Its CDN ETag
is `"af921799bcddb0519d6f49b3a8b3cced-3"`, S3's hash of the parts' hashes, while a base upload's ETag is its MD5 (see
*Found while measuring*).

The mount made 8 REST requests in 2 min 34 s (syncs, the refresh, deletes), 3 Upload API requests and 3 part PUTs, and
no CDN request. Its two uploads were removed with `rm -r tree/p4mac` (2 REST requests), and both show
`datetime_removed`.

## Linux

A mount with every view indexed 20 files with one REST request. `cp` of the five test images into `tree/p4lin/`
returned after the upload, as FUSE close waits for it: 1.24 s for the first, 0.59–0.61 s for the next three, 2.14 s
for the 134,861 B one.

**`getfattr -d` right after the upload** (0 requests, 7 ms):

```
# file: mnt/tree/p4lin/p4-1.jpg
user.uc.cdn_url="https://44bsymnfbi.ucarecd.net/24c3b05b-c0ba-4c08-8fc8-c701aa10df6b/"
user.uc.md5="35a2d8658f48cab669dc50fd80997553"
user.uc.meta.ucfs:path="/p4lin/p4-1.jpg"
user.uc.meta.ucfs:test="1"
user.uc.mime="image/jpeg"
user.uc.stored="true"
user.uc.uuid="24c3b05b-c0ba-4c08-8fc8-c701aa10df6b"
```

The md5 is the upload's own (`md5sum p4-1.jpg` prints the same); width and height came with the next sync, 30 s later,
as an upload answers with the uuid only. `getfattr -d` lists twice and reads every attribute twice (a size probe,
then the value), so a dump of 10 attributes is 2 lists and 20 reads, all from the index.

**`setfattr -n user.uc.meta.camera -v Canon`**: one metadata `PUT`, 0.293, 0.210 and 0.214 s in three runs. The API then
had it:

```
$ uploadcare file info 24c3b05b-c0ba-4c08-8fc8-c701aa10df6b --json uuid,filename,metadata
{"filename":"p4-1.jpg","metadata":{"camera":"Canon","ucfs:path":"/p4lin/p4-1.jpg","ucfs:test":"1"},"uuid":"24c3b05b-c0ba-4c08-8fc8-c701aa10df6b"}
```

The dashboard reads the same API; it was not checked in this run, and the file was deleted with the cleanup. After
the sync, `getfattr -d` added `user.uc.width="800"` and `user.uc.height="600"`, which the JPEG's SOF header
(600, 800) and the API's `content_info` agree with, and `user.uc.meta.camera="Canon"`. `setfattr -x
user.uc.meta.camera` was one `DELETE` (1.05 s); `uploadcare metadata list` then showed only `ucfs:path` and
`ucfs:test`, and a second `setfattr -x` answered "No such attribute".

**Entries past the TTL and cached ones.** `getfattr -d` of a file whose index entry was older than the TTL, which
refetches its info, then twice more within the TTL, from the index:

| File | Past the TTL (refetched) | Within the TTL (cached), twice |
|---|---|---|
| `tree/p4lin/p4-2.jpg` | 1 REST, 0.208 s | 0 requests, 0.008 s, 0.007 s |
| `tree/p4lin/p4-3.jpg` | 1 REST, 0.207 s | 0 requests, 0.008 s, 0.008 s |
| `tree/p4lin/p4-4.jpg` | 1 REST, 0.207 s | 0 requests, 0.008 s, 0.008 s |
| `tree/_inbox/probe1.jpg` (CLI upload, 6 keys, 2 tags) | 1 REST, 0.208 s | 0 requests, 0.008 s, 0.008 s |
| `tree/_inbox/p4-5.jpg` (CLI upload, md5 never read) | 1 CDN `HEAD` (0 B), 0.677 s | 0 requests, 0.007 s, 0.008 s |

The refresh is one `GET /files/{uuid}/` however many attributes are read; a file the mount did not upload pays one
`HEAD` for its md5 the first time, which matched `md5sum`.

**`ls -l` lists attributes.** uutils `ls -l` calls `listxattr` twice per file (strace: 10 calls for 5 files) and
marks every file that has attributes with `+` after its mode, as it would an ACL. With all five entries older than the
TTL, `ls -l` made 0 requests in 8 ms: `listxattr` answers from the index and never refreshes. Refreshing there would
have cost one request per file listed.

**A change by the CLI within the TTL.** `uploadcare metadata set <p4-2> lens 50mm` at 16:59:00; the entry had been
refreshed at 16:58:46. `meta.sh watch` read `user.uc.meta.lens` every 3 s:

| Since the change | `getfattr -n user.uc.meta.lens` |
|---|---|
| 0.0–42.2 s (15 reads) | "No such attribute" |
| 45.4 s | `50mm` |

It showed when the entry turned 60 s old, with one refresh; the reads before it cost nothing.

**Copies that carry attributes.**

| Command | Result |
|---|---|
| `cp -a tree/p4lin/p4-1.jpg tree/p4lin/p4-1-copy.jpg` (source has `lens=35mm`) | exit 0, silent; the upload carried `lens=35mm`, its own uuid, md5 and `ucfs:path` |
| `cp -a` of a local file with `user.uc.meta.source=local` and `user.comment=hello` | exit 0; the upload carried `source=local`; `user.comment` is not supported (`ENOTSUP`), which `cp -a` ignores |
| `cp --preserve=xattr` inside the mount | before the fix: "Operation not permitted" for the 8 read-only attributes, exit 1; after: exit 0, `lens` carried |
| `rsync -aX tree/p4lin/p4-2.jpg tree/p4rs/` | before the fix: 8 × `lsetxattr … Operation not permitted`, exit 23; after: exit 0, `lens`, `ucfs:test` and the tags carried, 5 REST requests (the source's refresh, two metadata `PUT`s, the tags `PUT`, the rename's `PUT`) |
| `rsync -aX` of a local file into `tree/p4rs/` | after the fix: exit 0, 3 REST requests: `source` set, `ucfs:test` deleted, the rename |

rsync makes a copy's attributes equal the source's: it writes `.<name>.<random>`, closes it (which uploads it under
that name, so the temporary name becomes `original_filename`, as for any rename after an upload, Phase 3), sets and
removes attributes, then renames. A local source without `ucfs:test` therefore loses the key that
`--upload-metadata` put on the upload. See *Found while measuring* for the two fixes.

**Tags.** `setfattr -n user.uc.tags` on four files and `-v "P4-Approved, p4-cat"` on one: one `PUT` each, 0.227–0.234 s;
the API normalised the last to `p4-approved,p4-cat`, and `getfattr` read it back so. `ls tags/` showed `p4-approved`,
`p4-cat`, `probe-a` and `x` (the probe's tags), and `ls tags/p4-approved/` four files with 0 requests in 8 ms. The
search API, asked for `{"tags":{"all":["p4-approved"]}}` right after tagging, returned 1 file, then two different pairs:
its index is asynchronous. From the next minute on it returned `total=4` with the same four uuids as the listing, five
times in a row over 24 s.

A tag set by another client (`PUT /files/{p4-3}/tags/` through `spikes/rest`) on a file already listed did not reach
`tags/p4-cat/` with the 30 s sync, which only lists new uploads; `ucfs index sync --full`, run next to the mount on the
same cache dir, listed the project in 2 requests (1.03 s), and `tags/p4-cat/` then showed the file.

**Groups.** `uploadcare group create p4-3 p4-1 p4-big p4-2` made `d510905e-…~4`. `groups/` did not list it yet;
`ls -l groups/d510905e-…~4/`, opening it by id, cost one group info request (0.236 s) and showed

```
-r--r--r--+ 1 alxgsv alxgsv  21651 Nov  8 16:55 0_p4-3.jpg
-r--r--r--+ 1 alxgsv alxgsv  22250 Nov  8 16:55 1_p4-1.jpg
-r--r--r--+ 1 alxgsv alxgsv 134861 Nov  8 16:55 2_p4-big.jpg
-r--r--r--+ 1 alxgsv alxgsv  21752 Nov  8 16:55 3_p4-2.jpg
```

in the order the group was created with; a second listing cost nothing. A second group, `9d0a3b6d-…~2`, was left alone:
the group sync of the next sweep listed it in `groups/` 21 s after its creation, without reading its members. After
both groups were deleted with the CLI, opening the second answered `ENOENT` and dropped it; the first, whose members
had been read, stayed listed (see *Not covered*).

**`by-date/`.** `ls -lR by-date` gave the same tree as on macOS (the host's zone is also UTC+6): the fixtures under
`2026/06/11` and `2026/08/11`, the test uploads under `2026/11/08`, with 0 CDN requests, 0 bytes and 0 REST requests
in 12 ms (and 72 `listxattr` calls from `ls`).

**Errnos**, through `setfattr`, `rm`, `touch` and Python's `os.setxattr` on `tree/p4lin/p4-4.jpg`:

| Call | Result |
|---|---|
| `user.uc.meta.<64 chars>`, a value of 512 `é` | set |
| `user.uc.meta.<65 chars>` | File name too long |
| `user.uc.meta.a b`, an empty value, tag `café` | Invalid argument |
| a 513-character value | Argument list too long |
| `user.uc.meta.ucfs:path`, `user.uc.uuid`, `setfattr -x user.uc.meta.ucfs:path` | Operation not permitted |
| `user.other` | Operation not supported |
| 51 tags | No space left on device (checked before sending) |
| a 51st key on `probe2.jpg`, which has 50 | No space left on device, from the API's 400 (1 request); the file kept 50 keys |
| `XATTR_CREATE` of an existing key, `XATTR_REPLACE` of a missing one | `EEXIST`, `ENODATA` |
| `touch` in `by-date/…`, `rm` in `tags/…`, `mkdir` in `groups/`, `mv` out of `by-date/`, append to a group member | Read-only file system |

`ucfs resolve --json tree/p4lin/p4-4.jpg` printed its metadata and `"tags": ["p4-approved", "p4-cat"]`. The first
mount's status after 8 min 49 s: 59 REST requests, 8 Upload API requests, 3 CDN `HEAD`s and 0 CDN bytes, 10 info
refreshes, 272 `listxattr`, 1,164 `getxattr`, 47 `setxattr` and 7 `removexattr` calls, 3 md5 HEADs, 1 group read.

## Behaviour this phase defines

- **Attributes.** Files in every view carry `user.uc.uuid`, `user.uc.cdn_url` (with the operations path for a
  variant), `user.uc.mime`, `user.uc.md5`, `user.uc.width` and `user.uc.height` (images), `user.uc.stored`
  (`true`/`false`), `user.uc.tags` (comma-separated, only when there are tags) and `user.uc.meta.<key>`. An attribute
  exists only when it has a value. Variants carry uuid, cdn_url and mime; directories and local-only files none.
- **Writes.** `setfattr` of `user.uc.meta.<key>` is one metadata `PUT`, `setfattr -x` one `DELETE`; `user.uc.tags`
  replaces the tag list with one `PUT /files/{uuid}/tags/` (`-x` clears it). Every successful write updates the index at
  once, so `getfattr` and `tags/` show it without a refresh. `user.uc.meta.ucfs:path` and the read-only attributes are
  `EPERM`; names outside `user.uc.*` are `ENOTSUP` to set and `ENODATA` to read. Files other clients uploaded take
  metadata too; a `--read-only` mount is `EROFS`.
- **Pending files.** A file still being written answers `user.uc.uuid` with an empty value and queues metadata and tags
  set on it into its upload form, kept with the spool across a restart; while its upload is in flight, `setfattr` is
  `EAGAIN`. Read-only attributes set or removed on such a file, or on one the mount uploaded within the last minute,
  are ignored, so `cp --preserve=xattr` and `rsync -X` from the mount succeed; on other files they are `EPERM`.
- **md5.** The CDN's ETag, one `HEAD` on first read and kept. Uploads record their MD5 from the spool, because a
  multipart upload's ETag is not one; other files list md5 only when it is known or the file is under 10 MiB.
- **Refresh.** `getxattr` and `ucfs resolve` read a file's info again when the index copy is older than `--meta-ttl`
  (5 min); concurrent readers share one request. `listxattr` answers from the index alone.
- **Views.** `--views` picks the mounted views, `files,tree,by-date` by default. `by-date/YYYY/MM/DD/<uuid>_<name>` uses
  `--by-date-zone` (default the local zone). `tags/` lists every tag a present file in the index carries.
  `groups/<uuid>~<count>/<index>_<name>` numbers members from 0 with zero padding, keeping gaps for removed members; the
  group list syncs with every sweep (10 min) when the view is mounted, members are read on the group's first listing,
  and a new group opens by id before the sync lists it. All three views are read-only.

## Found while measuring

- **A multipart upload's ETag is not its MD5.** The CDN answered `"af92…-3"` for a 12.2 MB file uploaded in 3 parts.
  `user.uc.md5` would have been missing for every large file. Uploads now record the MD5 of the spool, and other files
  list `md5` only when it is known or below 10 MiB, the Upload API's multipart minimum; an explicit read still asks the
  CDN and answers `ENODATA` when the ETag is not an MD5.
- **`cp --preserve=xattr` failed on the read-only attributes.** It copies `user.uc.uuid` and the rest onto the new file
  and exited 1 with eight "Operation not permitted" lines (`cp -a` keeps quiet about them). A file not uploaded yet now
  ignores read-only attributes, since its upload decides them.
- **`rsync -X` failed the same way after that fix**, exit 23: rsync closes its temporary file, which uploads it, before
  setting attributes. Files the mount uploaded within the last minute, the grace in which a write continues an upload
  (Phase 3), now ignore read-only attribute changes too.
- **Ubuntu's `ls -l` lists every file's attributes** (uutils coreutils), which is why `listxattr` never refreshes.

- **Tags set elsewhere on files already listed** reach `tags/` only after a refresh (an xattr read or `ucfs resolve`
  past the TTL) or `ucfs index sync --full`, as listings never show later changes; tags set through the mount show at
  once.
- **A deleted group whose members were read stays in `groups/`**, empty once its members are gone: groups never
  change, so its info is not read again, and the API has no listing of deleted groups. One never opened is dropped
  when opening it answers 404.
- **`rsync -X` into the mount mirrors attributes**, so it deletes `--upload-metadata` keys the source lacks, and a
  copy keeps rsync's temporary name as `original_filename`.
- **xattrs on macOS**: NFSv3 has none; `ucfs resolve` prints metadata and tags, and the CLI writes them.
- **Attributes other than `user.uc.*`** (`user.comment`) are not stored: `ENOTSUP`, which `cp -a` ignores and
  `rsync -X` reports.
- **FUSE under `-race`**, the Linux NFS front-end, and the Finder (the views were only listed with `ls`; a Finder window
  would read every visible image, Phase 1).

## End state

Cleanup went through the mount and the CLI: `rm -r tree/p4lin tree/p4rs` on Linux soft-deleted 13 files (13 REST
requests, 3.0 s), `rm -r tree/p4mac` on macOS 2, `uploadcare file delete` the 3 CLI uploads (the two probes, one with
50 metadata keys, and `p4-5.jpg`), and `uploadcare group delete` both groups. All 18 test uploads show
`datetime_removed`.

The project then listed exactly the 18 fixtures. Their uuids, names, sizes, MIME types, upload times and metadata
(`{}`) diff equal to the listing taken before the first request of this phase, and the raw listing shows `"tags": []`
for all 18:

```
99aa0dee-c92f-4828-819d-071b590cd64b  model-starter.png       b5501213-2aae-4d77-af9c-ba4f2349aaf7  ecommerce-starter.png
d38a135b-62a9-4e18-9369-ccd56833c3f9  declutter-sample.png    af250df2-119d-4ca0-8ea2-1a166f08cee9  SCR-20260611-ojir.png
d66e81b8-0d79-44fc-b500-b425cbefda39  SCR-20260611-ojir.png   cda72bcc-866e-4ac5-bdea-91d5a99ac105  SCR-20260611-ojir.png
1a03b42e-05cc-4803-a46d-cc4522c6aa14  rick.webm               c78bf20e-2a42-4b15-ad0a-6a83d03d0088  rick.webm
937d7848-9bb1-4b99-a629-3df59e62351a  rick.webm               f878de51-21dd-4545-8f57-279661312473  rick.webm
5e92fdb5-1e5e-478d-bc7b-f7f122962bd8  rick.webm               7959e40a-59f6-4076-9ec5-41d79e8a8297  rick.webm
dc384cdb-c25c-4bf1-8df0-eef0a45714ce  rick.webm               aab0feb3-3ad3-4e7e-bb37-8e4a7e09ca3c  rick.webm
608bbe90-7a70-43c6-b4a8-cc789d7a9f71  rick.webm               91c2d123-118d-4e09-bc5c-09b6b81495c7  rick.webm
daede4c7-01e2-4d85-ace8-bcd573ecfe1c  Rick Astley - … .webm   e7c0a2d2-7e53-46d2-a0c7-46c23db6a518  Rick Astley - … .webm
```

A search for `metadata[ucfs:test]=1` finds 0 files and `GET /groups/` lists 0 groups. The phase read 0 CDN bytes on
either host (CDN traffic was 3 `HEAD`s for md5 on Linux and two `curl -I` checks of ETags); uploads came to about
12.7 MB. No ucfs mount, process, control socket, registry record or spool is left on either host, and the test cache
dirs were removed.
