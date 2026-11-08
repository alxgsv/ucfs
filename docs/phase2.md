# Phase 2 acceptance: variants as paths

Measured on 2026-11-08 against `uploadcare-go-test`, on the same two hosts as [Phase 1](phase1.md):

- **macOS:** Apple Silicon Mac, macOS 27.0.1, NFS front-end, nothing installed.
- **Linux:** x86_64 host, Ubuntu 26.04.1 with kernel 7.0, FUSE front-end, mounted as a normal user through `fusermount3`.

The project's 33 fixtures are 333 KB at most, so one larger image was uploaded for the measurements: `hero.jpg`, 9,785,662 B,
6000×4000, tagged `ucfs:test=1` and deleted after acceptance. It is synthetic, because the project rejects random
binaries and a pure-noise JPEG shrinks unlike a photo when resized. It is value noise summed over 12 octaves with
amplitude proportional to scale^0.3, saved at JPEG quality 95. That falloff was picked so the image compresses like a
photograph. Its 1200 px WebP takes 0.12 B per pixel. A real 1000×667 stock photo among the fixtures takes 0.125 B per
pixel as a WebP. A first try with smooth gradients and grain gave a 1200 px WebP of 12 KB, which would have overstated
the saving, and was deleted.

Every run uses a fresh mount and an empty `--cache-dir`, which means a new index and an empty block cache. The
scripts are [`spikes/measure/variants.sh`](../spikes/measure/variants.sh) and
[`spikes/measure/variants-concurrent.sh`](../spikes/measure/variants-concurrent.sh). CDN requests come from
`ucfs status`, which now splits them into HEADs and GETs. CDN bytes are response bodies read.

## Acceptance

`<f>` is `files/<uuid of hero.jpg>`, and `<webp>` is `-/resize/1200x/-/format/webp`.

| Criterion | macOS, NFS | Linux, FUSE |
|---|---|---|
| `cp <f>/<webp>/hero.webp .` equals `curl` of the CDN URL | MD5 `299f2b9f…0bd` for both, 153,608 B | same MD5, 153,608 B |
| `stat` of a never-seen variant | 1 HEAD, 0 GET, 0.82 s | 1 HEAD, 0 GET, 0.46 s |
| The same `stat` again | 0 requests, 10 ms | 0 requests, 4 ms |
| `ls -l <f>/<webp>/` | `hero.webp`, 153,608 B, 0 requests (already known) | same |
| `ls -l <f>/-/resize/800x/`, never seen | `hero.jpg`, 83,023 B, 1 HEAD | same |
| Unknown operation: `stat <f>/-/nosuchop/hero.jpg` | No such file or directory, 0 requests | same |
| Bad argument: `stat <f>/-/resize/abc/hero.jpg` | Invalid argument; 1 HEAD and 1 GET (the 738 B error page) | same |
| `cat` of it, `ls` of `-/resize/abc/`, `stat` again | Invalid argument, 0 requests | same |
| Three variants of one file at once | no stall, see below | no stall, see below |
| `stat` of another file during those reads | 9–22 ms | 4–5 ms |
| CDN bytes of `cp` of the 1200 px WebP | 153,608 B, against 9,785,662 B for the original | same |

The daemon log gets the CDN's reason for a 400 at info level, once per hour per variant:

```
level=INFO msg="CDN rejected a variant" uuid=23b17a75-… ops=-/resize/abc/ message="Mandatory argument \"dimensions\" not found while parsing \"resize/abc\""
```

Other checks:

- `ls <f>/` lists `-` and `hero.jpg`; `ls <f>/-/` lists nothing. Neither costs a request.
- A second `cp` of the variant costs 0 B. The blocks stay cached under the variant's ETag.
- A Phase 1 index (schema version 1, in the default cache dir) upgraded in place. A `stat` of a 600 px WebP variant of
  the stock photo then cost 1 HEAD.
- No mount, daemon or socket was left behind on either host after any run.

## Traffic: what the view saves

| What was copied | CDN bytes | Share of the original |
|---|---|---|
| `hero.jpg`, the original | 9,785,662 B | 100 % |
| `-/resize/1200x/-/format/webp/hero.webp` | 153,608 B | 1.6 % (64× less) |
| `-/resize/800x/hero.jpg` (size from `ls -l`) | 83,023 B | 0.85 % |
| Stock photo fixture, original 1000×667 JPEG | 309,707 B | 100 % |
| Its `-/resize/600x/-/format/webp/` | 43,458 B | 14 % |

`cp` of a variant costs exactly the variant's size: `ucfs status` showed 153,608 B for 153,608 B copied, on both
hosts.

Rendering a variant for the first time is what costs time, and the CDN does it once for all clients. The first GET of
the 3000 px PNG of `hero.jpg` took 21.3 s and the second took 0.94 s; for the 2600 px PNG it was 13.3 s, then 1.3 s.
The `stat` that triggers a render waits for it, because the HEAD does. The 1200 px WebP took 0.82 s on macOS, and
0.46 s on Linux, which ran later.

## Three variants of one file at once

These are three PNG variants of `hero.jpg`: 3000 px (9,264,856 B), 2800 px (8,129,630 B) and 2600 px (7,053,079 B),
already rendered on the CDN. Each was read with `cat`, first alone and then all three together from three shells.
`stat` of another file and `ls -l files/` ran 0.3 s after the three readers started.

| Host, round | Solo 3000 / 2800 / 2600 | Together 3000 / 2800 / 2600 | `stat` during | `ls -l` during |
|---|---|---|---|---|
| macOS, round 0 | 1.42 / 1.25 / 1.25 s | 2.30 / 1.79 / 2.14 s | 22 ms | 12 ms |
| macOS, round 1 | 2.56 / 2.33 / 2.05 s | 4.16 / 4.06 / 4.06 s | 13 ms | 10 ms |
| macOS, round 2 | 2.05 / 1.90 / 1.81 s | 4.07 / 3.88 / 3.59 s | 12 ms | 10 ms |
| macOS, round 3 | 1.99 / 1.93 / 1.76 s | 4.00 / 4.02 / 4.00 s | 11 ms | 9 ms |
| Linux, round 1 | 1.97 / 1.95 / 1.72 s | 4.13 / 3.98 / 3.83 s | 5 ms | 5 ms |
| Linux, round 2 | 2.03 / 1.92 / 3.43 s | 4.01 / 4.01 / 4.01 s | 4 ms | 5 ms |
| Linux, round 3 | 2.14 / 1.87 / 1.70 s | 4.33 / 3.96 / 4.26 s | 4 ms | 5 ms |

macOS round 0 came from a first run whose script then hung on its own bug, a `wait` that also waited for the daemon.
The network was faster at the time, which is why the same reads took 1.2–1.4 s alone.

- **Nobody stalls.** The three readers finish together, sooner than reading the three files one after another (about
  5.8 s of solo time). Combined throughput rose from about 4.6 MB/s solo to about 6.1 MB/s.
- **ucfs is not the limit.** `curl` of the same three URLs on the Mac took 1.43–1.94 s alone and 2.83–4.84 s in
  parallel. The Mac and the Linux host show the same combined rate, about 6 MB/s, and probably share one uplink.
- **Metadata never waits.** `stat` and `ls -l` stayed at 9–22 ms on macOS and 4–5 ms on Linux, as in Phase 1.
- **A slow render does not hold up other reads.** On macOS, `cat` of a 2900 px PNG nobody had requested before took
  9.78 s while the CDN rendered it. A read of the rendered 3000 px PNG started 0.5 s later and took 1.97 s, and `stat`
  of another file took 10 ms. On Linux, with a 2700 px PNG, the numbers were 7.35 s, 1.89 s and 5 ms.

## Behaviour this phase defines

- **Layout.** `files/<uuid>/` is still left out of `files/`. It lists the original under its own name and the
  directory `-`. `-/` lists nothing, and so does every `…/-/` after it, so a recursive walk of a file's directory
  costs no request. A name there resolves only if it is an operation in the table, otherwise `ENOENT` with no request.
  In an operation or argument directory:
  - `-` starts the next operation.
  - The default entry is the variant.
  - A name starting with `.` is `ENOENT`, so `.DS_Store` and `._*` probes do not become directories.
  - Any other name is one more argument directory.
- **The default entry.** It is named after the original, with the same `/` and NUL mapping as `files/`. The names `""`,
  `.`, `..` and `-` become `unnamed`. The extension follows the last `format/<x>`: `jpeg` gives `.jpg`, and `auto` or
  `preserve` keep the original's. `json` and `main_colors` give `.json`. So `hero.webp` resolves under `-/format/webp/`,
  and `hero.jpg` there is an argument directory.
- **Cost of a listing.** `readdir` of an operation or argument directory reports the variant's real size. Both NFS
  READDIRPLUS and macOS cache those attributes, so a placeholder would stick. Listing therefore costs the same one HEAD
  as a `stat`. When the CDN rejects the operations, the listing fails with `EINVAL`.
- **Caching.** A HEAD answer is kept in the index `variants` table for 24 h, and concurrent lookups share one HEAD.
  After 24 h the next access sends a new HEAD. If the size and ETag are unchanged, the cached blocks stay. Otherwise the
  old blocks are dropped and the variant's mtime moves, so NFS clients drop their cached pages. Blocks are stored per
  ETag, so content fetched under an old ETag is never served for a new one. FUSE keeps the page cache across opens for
  originals only.
- **Errors.**
  - A 400 is `EINVAL` on lookup, `stat`, `readdir` and read. The reason is fetched with one GET and logged, and the
    400 is remembered for an hour, the `max-age` the CDN gives its error page.
  - A 404 hands the file to the Phase 1 REST refresh and is remembered for 10 s, long enough to cover the several
    walks of one NFS request.
  - Network errors and 5xx answers are `EIO` and are not remembered.
- **NFS file handles.** Handles stay 28 bytes. A variant node's operations path does not fit, so the first handle for
  each node writes `(node id, uuid, ops)` to a `node_ops` table. A restarted daemon then resolves those handles. The
  test does this with a second filesystem on the same index.
- **`ucfs status` and `ucfs umount`** find every running mount, whatever `--cache-dir` it was started with. A
  registry in `<user cache dir>/ucfs/mounts/` maps the canonical mount directory to its socket. The registry is
  checked on macOS (a mount through `/tmp`, unmounted through `/private/tmp`) and on Linux.

## Found while measuring

- **go-nfs could not return `EINVAL`.** It answers every failed LOOKUP with `NOENT`, every failed READ with `IO` or
  `ACCES`, and every failed listing with `NOTDIR`, whatever the filesystem returned. ucfs now carries a copy of the
  pinned commit in [`third_party/go-nfs`](../third_party/go-nfs/README.ucfs.md). It maps the errno inside an error to
  the matching NFS status. A probe build confirmed that macOS reports NFS3ERR_INVAL as "Invalid argument" for both
  `stat` and `cat`.
- **The 400 reason needs a GET.** A HEAD answer has no body, and the reason is the `<p>` text of an HTML page.
- **`-/resize/1200x/` upscales.** It turned the 1000×667 stock photo into a 1200×800 JPEG, so a variant can be larger
  than the original's pixel size.
- **The CDN rejects `-/main_colors/3/` alone with a 400.** Image operations need one of `resize`, `smart_resize`,
  `scale_crop` or `preview` in the chain. `-/preview/-/main_colors/3/` returns JSON.

## Not covered

- **A variant re-rendered within its 24 h window** with different bytes could mix blocks fetched before and after the
  change. A weak ETag cannot be used with `If-Range`. Comparing each 206 response's ETag with the recorded one would
  catch it, and is left for Phase 5.
- **The go-nfs change is not upstream.** It is small and general (errno to NFS status), and a pull request to
  `willscott/go-nfs` would let ucfs drop the copy.
- **Finder in the variants view** was not observed. Its thumbnailer reads the default entry like any image, and each
  operation directory it opens costs one HEAD.
- **Still open from Phase 1:**
  - NodeID collision detection (Phase 5).
  - The NFS front-end on Linux (needs root).
  - FUSE tests under `-race` (no cgo on the Linux host).

  `go test -race` ran on macOS for `vfs`, `cache`, `index`, `frontend/nfs`, `ctl` and `cdn`.
