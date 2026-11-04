# scour

Parallel directory walker for Go, built for very large trees on parallel and
network filesystems (Lustre, GPFS, BeeGFS, NFS). It streams entries as it finds
them, does not `stat` unless you ask, and lets you cap how hard it hits the
metadata server.

```
go get github.com/bete7512/scour
```

Go 1.24 or later. The only dependency is `golang.org/x/sys`.

## Why

`filepath.WalkDir` is single-threaded and sorts every directory it reads. On a
local SSD that is fine. On a shared parallel filesystem with a few hundred
million files, two things decide wall time: how many metadata requests are in
flight at once, and how many of them are `stat` calls. One walker thread cannot
keep a metadata server busy, and a `stat` per file doubles the request count for
nothing if you only need names and types.

scour reads many directories in parallel, classifies entries from `d_type`
without stat, and gives the caller a knob for request rate so a scan on a shared
cluster does not slow everyone else down.

## Usage

```go
stats, err := scour.Walk(ctx, "/lustre/project", func(e scour.Entry) error {
	if e.Type == scour.TypeFile {
		fmt.Println(e.Path)
	}
	return nil
}, scour.WithWorkers(64))
```

The callback runs on the worker goroutines, concurrently. Keep it safe for that,
or use the channel form, which gives you a single consumer:

```go
s := scour.Scan(ctx, "/lustre/project",
	scour.WithWorkers(64),
	scour.WithStat(scour.StatFull),
)
var total int64
for e := range s.Entries() {
	total += e.Info.Size()
}
stats, err := s.Wait()
```

If you stop reading from `Entries()` before it is closed, cancel the context, or
the workers block.

```go
type Entry struct {
	Path  string      // root joined with the path below it; not cleaned
	Name  string      // last element of Path
	Type  Type        // TypeDir, TypeFile, TypeSymlink, TypeOther
	Depth int         // 0 for the root
	Info  fs.FileInfo // set only with StatFull; Entry.Stat() lstats on demand
}

type Stats struct {
	Dirs, Files, Symlinks, Other uint64
	Errors                       uint64
	Elapsed                      time.Duration
	FirstErrors                  []PathError // at most 100
}
```

Return `scour.SkipDir` from the callback on a directory to stay out of it, or
`scour.SkipAll` to end the walk with no error. These are the `io/fs` values and
mean the same as in `filepath.WalkDir`. Any other error ends the walk and is
returned.

## Options

| Option | Default | What it does |
|---|---|---|
| `WithWorkers(n)` | `4 × GOMAXPROCS` | Directory-reading goroutines |
| `WithStat(mode)` | `StatOnDemand` | `StatFull` lstats every entry on the workers and fills `Info` |
| `WithRateLimit(opsPerSec)` | off | Token bucket over open, getdents and stat calls |
| `WithSkipDir(func(path string) bool)` | none | Prune subtrees before they are queued |
| `WithMaxDepth(n)` | unlimited | Do not descend below depth n; the root is depth 0 |
| `WithFollowSymlinks()` | off | Follow symlinks to directories; loops end by device and inode |
| `WithOneFS()` | off | Do not cross mount points |
| `WithErrorHandler(func(path string, err error) error)` | count and continue | Return an error to end the walk |
| `WithProgress(every, func(Stats))` | none | Periodic snapshot of the counters |
| `WithBufferSize(bytes)` | 256 KiB | Per-worker getdents buffer |

## Behavior

- A directory is delivered before its contents, and the root first. Nothing
  else about the order is promised. There is no sorting.
- `Path` is the root joined with the path below it, without cleaning, the way
  `find` prints it. `Name` is the last element.
- Symlinks are reported as symlinks and not followed. With `WithFollowSymlinks`
  entries are classified by what they point to, as with `find -L`; dangling
  links stay `TypeSymlink`; each directory is read once, identified by device
  and inode, which is also what ends loops.
- Mount points are crossed. With `WithOneFS` the mount point is delivered as a
  directory but not read, as with `find -xdev`.
- Failures below the root (`EACCES`, `ENOENT` races, `EIO`) are counted, the
  first 100 are kept in `Stats.FirstErrors`, and the walk goes on. An error
  handler sees each one and can turn it into an abort. A root that cannot be
  lstat-ed is returned as an error.
- Cancelling the context stops the workers at their next syscall. `Walk`
  returns the context's error with the partial `Stats`.
- On Linux, directories are read with `getdents64` into a per-worker buffer and
  parsed in place. Everywhere else `os.File.ReadDir` is used: same results,
  slower. `WithFollowSymlinks` and `WithOneFS` need device and inode numbers
  and return an error on Windows.

## How it works

A fixed pool of workers shares one LIFO stack of directories. A worker pops a
directory, opens it, reads it in 256 KiB chunks, classifies each record from
`d_type`, calls the callback, and pushes subdirectories back on the stack. A
pending counter ends the walk when it reaches zero. Depth-first order keeps the
frontier near depth × fan-out rather than the width of a level, so memory is
roughly that plus workers × buffer. The only per-entry allocation is the path
string. `lstat` happens only with `StatFull`, for entries whose `d_type` is
unknown, and for symlinks when following them.

The design notes with the reasoning behind each decision, and notes on Lustre,
GPFS and NFS, are in [specification.md](specification.md).

## Benchmarks

```
go test -run='^$' -bench=. .
```

builds a tree of 1,557 directories and 71,100 files (two directories hold
20,000 each) in a temporary directory and walks it with `filepath.WalkDir`,
`find` and scour. Set `SCOUR_BENCH_ROOT` to walk an existing tree instead:

```
SCOUR_BENCH_ROOT=/lustre/project go test -run='^$' -bench=. -benchtime=3x .
```

On a laptop (i7-10870H, 8 cores, ext4 on NVMe, warm page cache, Linux 6.8,
Go 1.26):

| | entries/s | allocs/entry |
|---|---:|---:|
| `filepath.WalkDir` | 0.72M | |
| `find -false` (bfs 4.1.1) | 0.64M | |
| scour, 1 worker | 1.20M | 1.06 |
| scour, 4 workers | 3.50M | 1.06 |
| scour, 16 workers | 3.26M | 1.07 |
| scour, 64 workers | 4.02M | 1.07 |
| scour, 16 workers, `os.File.ReadDir` fallback | 2.86M | 4.3 |
| scour, 16 workers, `StatFull` | 1.05M | 3.1 |
| scour `Scan`, 16 workers | 2.19M | 1.07 |

Past four workers the walk is waiting on the kernel: 73% of CPU time is in
syscalls, and the directory cache is shared. The worker count is for network
filesystems, where every request waits on a server and concurrency is what
hides the latency. There are no Lustre, GPFS or NFS numbers yet; local-disk
numbers say little about HPC behavior.

## Testing

```
go test -race ./...
go test -run='^$' -fuzz=FuzzParseDirents -fuzztime=1m .
```

The tests compare path and type sets against `filepath.WalkDir` across worker
counts, buffer sizes, both readers and both stat modes; check delivery order and
depths; exercise `SkipDir`, `SkipAll`, cancellation, permission-denied subtrees,
the error handler and the 100-error cap; walk symlink loops with
`WithFollowSymlinks` and the mounts under `/dev` with `WithOneFS`; delete and
rename files while walks are running; and check the `linux_dirent64` parser
against the kernel and under fuzzing. CI runs them with the race detector on
Linux and macOS, and without it on Windows.

## Status

v0.1. The API may change before v1.0. Planned: checkpoint and resume (v0.2), a
`scour` CLI for counting, sizes and listing (v0.3), multi-node walks with a
coordinator (v2).

## License

MIT
