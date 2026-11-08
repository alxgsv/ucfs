# go-nfs, patched for ucfs

A copy of [github.com/willscott/go-nfs](https://github.com/willscott/go-nfs) at commit `c092ae79c64e`
(`v0.0.5-0.20260920152406-c092ae79c64e`), the commit ucfs pinned since Phase 0. Licensed under Apache-2.0, see
[LICENSE](LICENSE). The root `go.mod` of ucfs points the module path here with a `replace` directive.

Only the library itself is copied: the root package, `file/` and `helpers/` (which `spikes/nfsstatic` uses), without
`helpers/memfs`. Tests and `example/` are left out, and
`go.mod` is tidied to what the copied code imports.

## Changes

Stock go-nfs answers every failed LOOKUP with `NFS3ERR_NOENT`, every failed READ with `NFS3ERR_IO` or
`NFS3ERR_ACCES`, and every failed directory listing with `NFS3ERR_NOTDIR`, whatever the filesystem returned. ucfs
has to return `EINVAL` for a variant the CDN rejects with 400, so `errors.go` adds `statusFromError`, which maps a
`syscall.Errno` carried by the error (for example inside an `fs.PathError`) to the matching NFS status and otherwise
keeps the procedure's old status. `EXDEV` maps to `NFS3ERR_XDEV`, `ENOTSUP` and `EOPNOTSUPP` (one value on Linux,
two on darwin) to `NFS3ERR_NOTSUPP`, and `EAGAIN`, which NFSv3 lacks, to `NFS3ERR_JUKEBOX`. `asStatusError` keeps a
status a procedure already chose and maps any other error. The mapping is applied in:

- `nfs_onlookup.go`: both `Lstat` calls (the directory and the entry)
- `nfs_ongetattr.go`: `Lstat`
- `nfs_onread.go`: `Open`, `Stat` and `ReadAt`
- `nfs_onreaddir.go`: `ReadDir`, which READDIR and READDIRPLUS share
- `nfs_oncreate.go`: the parent `Stat`, `Create`, `Close`, and the attributes applied after
- `nfs_onwrite.go`: `Stat`, `OpenFile`, `Write` and `Close`
- `nfs_onsetattr.go`: `Lstat` and the attributes applied, which returned raw errors before and so became RPC
  system errors instead of NFS statuses
- `nfs_onremove.go` (also RMDIR): `Remove`, so `EPERM` reaches the client as `NFS3ERR_PERM` and not `NFS3ERR_ACCES`
- `nfs_onrename.go`: `Rename`
- `nfs_onmkdir.go`: `MkdirAll` and the attributes applied
- `nfs_oncommit.go`: `Commit` (below)

Two behaviours changed for ucfs writes:

- **COMMIT reaches the filesystem.** `nfs_oncommit.go` adds the `Committer` interface. When the billy filesystem
  implements it, WRITE replies `UNSTABLE` to a client that asked for it (stock go-nfs always replied `FILE_SYNC`, so
  clients never sent COMMIT), and COMMIT calls `Commit(path)`. A WRITE the client asks to be stable (`FILE_SYNC`
  or `DATA_SYNC`) calls `Commit` too, since it is a write and a commit at once. A trace of macOS 27 showed `cp`
  sending one COMMIT right after its last WRITE for a 15 MB file, and a single `FILE_SYNC` WRITE and no COMMIT for
  a file that fits in one 1 MiB WRITE; either is the closest NFSv3 has to a close (docs/phase3.md).
- **Exclusive CREATE is served as guarded.** Stock go-nfs refused `open(O_CREAT|O_EXCL)` with `NFS3ERR_NOTSUPP`,
  which broke vim's swap files. The verifier is not stored, so a retransmitted exclusive CREATE fails with
  `NFS3ERR_EXIST`; on ucfs's loopback TCP connection that does not happen.

Each changed file carries a notice at the top. The changes are small enough to send upstream; until then this copy
is the "fork and fix" fallback from PLAN §6.
