# go-nfs, patched for ucfs

A copy of [github.com/willscott/go-nfs](https://github.com/willscott/go-nfs) at commit `c092ae79c64e`
(`v0.0.5-0.20260920152406-c092ae79c64e`), the commit ucfs pinned since Phase 0. Licensed under Apache-2.0, see
[LICENSE](LICENSE). The root `go.mod` of ucfs points the module path here with a `replace` directive.

Only the library itself is copied: the root package, `file/` and `helpers/` (which `spikes/nfsstatic` uses), without
`helpers/memfs`. Tests and `example/` are left out, and
`go.mod` is tidied to what the copied code imports.

## Change

Stock go-nfs answers every failed LOOKUP with `NFS3ERR_NOENT`, every failed READ with `NFS3ERR_IO` or
`NFS3ERR_ACCES`, and every failed directory listing with `NFS3ERR_NOTDIR`, whatever the filesystem returned. ucfs
has to return `EINVAL` for a variant the CDN rejects with 400, so `errors.go` adds `statusFromError`, which maps a
`syscall.Errno` carried by the error (for example inside an `fs.PathError`) to the matching NFS status and otherwise
keeps the procedure's old status. It is applied in:

- `nfs_onlookup.go`: both `Lstat` calls (the directory and the entry)
- `nfs_ongetattr.go`: `Lstat`
- `nfs_onread.go`: `Open`, `Stat` and `ReadAt`
- `nfs_onreaddir.go`: `ReadDir`, which READDIR and READDIRPLUS share

Each changed file carries a notice at the top. The change is small enough to send upstream; until then this copy
is the "fork and fix" fallback from PLAN §6.
