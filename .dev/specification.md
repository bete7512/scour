# scour

Parallel directory walker for Go, built for very large trees on parallel and network filesystems (Lustre, GPFS, BeeGFS, NFS). It streams entries as it finds them, does not `stat` unless you ask, and lets you cap how hard it hits the metadata server.

**Status:** design stage. The API below is the target for v0.1 and may change before then.

```
go get github.com/bete7512/scour
```

## Why this exists

`filepath.WalkDir` is single-threaded and sorts every directory it reads. On a local SSD that is fine. On a shared parallel filesystem with a few hundred million files, two things decide wall time: how many metadata requests are in flight at once, and how many of them are `stat` calls. One walker thread cannot keep a metadata server busy, and a `stat` per file doubles the request count for nothing if you only needed names and types.

`scour` reads many directories in parallel, classifies entries from `d_type` without stat, and gives the caller a knob for request rate so a scan on a shared cluster does not slow everyone else down.

The closest existing Go package is `charlievieth/fastwalk`, which is also parallel. `scour` is narrower and aimed at HPC use: stat modes, a metadata rate limiter, counters, no sorting, and a planned resume mode. If you want a fast general walker for a laptop, `fastwalk` is a good choice.

## Quick start

```go
import "github.com/bete7512/scour"

stats, err := scour.Walk(ctx, "/lustre/project", func(e scour.Entry) error {
    if e.Type == scour.TypeFile {
        fmt.Println(e.Path)
    }
    return nil
}, scour.WithWorkers(64))
```

The callback runs on worker goroutines, **concurrently**. Keep it safe for that, or use the channel form, which gives you a single consumer:

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

## API (v0.1 target)

```go
func Walk(ctx context.Context, root string, fn WalkFunc, opts ...Option) (*Stats, error)
func Scan(ctx context.Context, root string, opts ...Option) *Scanner

// Return SkipDir from fn on a directory entry to not descend into it.
// Any other error aborts the whole walk.
type WalkFunc func(e Entry) error

type Entry struct {
    Path  string
    Name  string      // last path element
    Type  Type        // TypeDir, TypeFile, TypeSymlink, TypeOther
    Depth int         // 0 for root
    Info  fs.FileInfo // set only with StatFull
}

// Stat does an lstat on demand. Available in every mode.
func (e Entry) Stat() (fs.FileInfo, error)

type Stats struct {
    Dirs, Files, Symlinks, Other uint64
    Errors                       uint64
    Elapsed                      time.Duration
    FirstErrors                  []PathError // first 100, for reports
}
```

Options:

| Option | Default | What it does |
|---|---|---|
| `WithWorkers(n)` | `4 * GOMAXPROCS` | Number of directory-reading goroutines |
| `WithStat(mode)` | `StatOnDemand` | `StatFull` makes workers lstat every entry in parallel and fill `Info` |
| `WithRateLimit(opsPerSec)` | off | Token bucket over open, getdents and stat calls |
| `WithSkipDir(func(path string) bool)` | none | Prune subtrees before they are queued |
| `WithMaxDepth(n)` | unlimited | Stop descending below depth n |
| `WithFollowSymlinks()` | off | Follow symlinks to directories (with loop detection by dev/inode) |
| `WithOneFS()` | off | Do not cross mount points |
| `WithErrorHandler(func(path string, err error) error)` | count and continue | Return an error to abort |
| `WithProgress(every, func(Stats))` | none | Periodic snapshot of counters |
| `WithBufferSize(bytes)` | 256 KiB | Per-worker getdents buffer |

Entries for directories are emitted before their contents, and the root is emitted first. No ordering is promised beyond that.

## Architecture

```
  root
   │
   ▼
 ┌──────────────────────────────┐
 │ scheduler                    │
 │  - LIFO stack of dir paths   │◀──────────── subdirectories pushed back
 │  - pending counter           │
 │  - rate limiter (optional)   │
 └──────────────┬───────────────┘
                │ pop
    ┌───────────┼───────────┐
    ▼           ▼           ▼
 ┌───────┐   ┌───────┐   ┌───────┐
 │worker │   │worker │   │worker │   N goroutines, one open fd each
 │ open  │   │       │   │       │
 │getdents   │       │   │       │   256 KiB buffer, parsed in place
 │classify   │       │   │       │   d_type → dir / file / link / other
 │ (lstat)   │       │   │       │   only in StatFull or on DT_UNKNOWN
 └───┬───┘   └───┬───┘   └───┬───┘
     │           │           │
     ▼           ▼           ▼
   fn(entry) called on the worker          ← Walk
   or sent on a bounded channel            ← Scan
                │
                ▼
          atomic counters → progress callback, final Stats
```

### Components

**Scheduler.** A mutex-guarded stack of directory paths plus an atomic `pending` counter. `pending` starts at 1 for the root. Every directory discovered does `pending++` before it is pushed; every directory finished does `pending--`. When it reaches zero the walk is over: workers are released, the output channel (if any) is closed.

**Workers.** `N` goroutines running the same loop:

1. Pop a directory. If the stack is empty, wait. If `pending == 0` or `ctx` is done, exit.
2. `open(path, O_RDONLY|O_DIRECTORY|O_CLOEXEC)`.
3. Loop `getdents64` into the worker's buffer until it returns 0.
4. For each record: build the full path, classify from `d_type`, lstat if `StatFull` or if `d_type == DT_UNKNOWN`, then hand the entry to the caller.
5. If the entry is a directory and the caller did not return `SkipDir` and `WithSkipDir` does not reject it, `pending++` and push.
6. Close the fd, `pending--`.

**Directory reader.** On Linux, raw `getdents64` through `golang.org/x/sys/unix` and manual parsing of `linux_dirent64` records (`d_ino`, `d_off`, `d_reclen`, `d_type`, `d_name`). Elsewhere, `os.ReadDir`. Same output either way; only speed differs.

**Stat layer.** `StatOnDemand` does nothing up front; `Entry.Stat()` is an lstat when the caller wants it. `StatFull` makes workers lstat in parallel, which is the right choice when every entry's size or mtime is needed anyway.

**Rate limiter.** A small internal token bucket that every open, getdents and stat call takes a token from. Off by default.

**Stats.** Atomic counters, snapshotted for the progress callback and returned at the end.

**Error handler.** Receives every failure with its path. The default counts it, keeps the first 100 for the report, and moves on.

### Memory

Memory is roughly `frontier + N × buffer`. The frontier is the set of discovered-but-unread directories. With a LIFO stack it stays close to `depth × average fan-out`, not the width of an entire level. The only per-entry allocation is the path string, which the caller owns after `fn` returns.

### Cancellation

`ctx.Done()` is checked on every pop and every `getdents64` iteration. Workers stop, in-flight entries are dropped, and `Walk` returns `ctx.Err()` with the partial `Stats`.

## Design decisions

Each one with the reasoning and what it costs.

**1. The unit of work is a directory, not a file.**
Readdir is what talks to the metadata server; files are just records inside its reply. Scheduling per file would add coordination overhead for nothing. Cost: one huge flat directory (tens of millions of entries) is read by one worker. That is a known pattern on some HPC systems; splitting a single directory's read across workers is possible with `d_off` seeking and is on the list for later if it shows up in practice.

**2. Fixed worker pool, not a goroutine per directory.**
A goroutine per directory is the obvious Go shape, but every queued directory then costs a parked goroutine (several KB each). On a tree with millions of directories that is gigabytes of stack before any work happens. A pool bounds concurrency, file descriptors and memory in one place.

**3. LIFO stack, not FIFO queue.**
Breadth-first discovers an entire level before reading any of it, so the frontier grows to the width of the widest level. Depth-first keeps the frontier small and finishes subtrees early, which also makes resume and per-subtree stats simpler. Cost: traversal order is nondeterministic and not breadth-first. Nothing in the API depends on order.

**4. Shared stack under one mutex, no work stealing yet.**
The critical section is a push or pop of a string. Workers spend almost all their time in syscalls, so with 64 to 256 workers the lock is not where time goes. Per-worker deques with stealing are a later optimization, if profiling shows contention. Start simple, measure, then change.

**5. Raw `getdents64` on Linux instead of `os.ReadDir`.**
`os.ReadDir` already uses `d_type`, so stat avoidance is not the gain. The gain is that `os.ReadDir` sorts every directory by name, allocates a `DirEntry` per entry, and reads with a small (8 KiB) buffer. `scour` skips the sort, parses records in place from a 256 KiB reusable buffer, and does fewer syscalls per directory. On Lustre, larger readdir chunks also mean fewer RPCs. Cost: Linux-specific code and a parser that needs fuzzing. Other platforms get `os.ReadDir` and correct but slower results.

**6. No stat by default.**
A walk that only needs names and types should cost one readdir stream per directory, nothing per file. On Lustre and GPFS, `lstat` per file is the single most expensive thing a client-side scanner can do. Callers that need size or mtime say so with `StatFull` and get it done in parallel by the workers. Always `lstat`, never `stat`, so symlinks are reported as symlinks.

**7. The callback runs on the workers, concurrently.**
A single serial callback caps throughput and needs a channel hop for every entry. Calling `fn` on the worker means a slow callback slows only that worker, which is exactly the backpressure you want, with no extra queue. Cost: callers must write thread-safe callbacks. This is documented in the first paragraph of the quick start, and `Scan` exists for anyone who prefers one consumer.

**8. Open directories by full path, one fd per worker.**
`openat` relative to a parent fd avoids path resolution and is safer against renames mid-walk, but it needs one open fd per depth level per worker. One fd per worker keeps the descriptor budget predictable, which matters when `ulimit -n` is 1024 and the operator did not expect a walker to need more. Path lookups are served from the dentry cache on local filesystems; on network filesystems they add a little per-directory cost, which is small next to the readdir itself.

**9. A rate limiter on metadata operations.**
On a shared cluster the metadata server belongs to everyone. A walker that runs 256 workers flat out can make an interactive login node unusable for other users. The limiter is a token bucket over open, getdents and stat so an operator can say "no more than 20k metadata ops/sec from this job" and have it hold. Off by default because the right number is site-specific.

**10. Skip-and-report on errors.**
A 500M-file walk should not die at file 400M because one directory lost its permissions or was deleted mid-walk. `EACCES`, `ENOENT` races, `ELOOP` and `EIO` go to the error handler and are counted. Callers who want strict behavior return an error from the handler and the walk aborts.

**11. No ordering guarantees.**
Sorting costs CPU and memory per directory and a parallel walk cannot produce global order anyway. Anyone who needs sorted output sorts the result, which is cheaper than sorting inside the walk.

**12. Symlinks not followed; mount points crossed by default.**
This matches `find`. Following symlinks is opt-in and uses dev/inode tracking to stop loops. `WithOneFS` is opt-in because checking it costs one lstat per directory to compare `st_dev` against the root.

**13. Single node first, multi-node later.**
Most of the win on one node comes from parallelism and not stat-ing. Multi-node adds a coordinator, work partitioning and result merging, which is a different kind of problem. The seam is already there: a coordinator can hand each node a list of subtree roots and run `scour` on them. v2 does that without MPI, modeled on mpiFileUtils' `dwalk`.

**14. Dependencies: `golang.org/x/sys` only.**
Syscalls need it. Everything else, including the token bucket, is small enough to write in-tree. Anyone importing a walker should not inherit a dependency tree.

## Notes for HPC filesystems

- Start with 32 to 64 workers per node. Metadata latency on a network filesystem is high, so concurrency well above core count helps, until the metadata server saturates. Watch its load and back off, or set `WithRateLimit` on shared systems.
- Some filesystems return `DT_UNKNOWN` (certain NFS exports, FUSE, some older XFS layouts). `scour` lstats only those entries, but throughput drops there.
- On NFS, `readdirplus` means attributes are often already cached after readdir, so `StatFull` is much cheaper there than on Lustre.
- If the filesystem has a native scan (Lustre changelogs or `lfs find`, GPFS `mmapplypolicy`), it will beat any client-side walker. `scour` is the portable option for when you do not have that, or need the same code on every system.
- Until v2, split top-level subtrees across nodes by hand and run `scour` on each.

## Benchmarks

No numbers yet. The method, once there is code to measure:

- A generator that builds trees of 10M+ files with configurable depth, width, and a few very wide directories, so the layout is reproducible.
- Baselines: `filepath.WalkDir`, `charlievieth/fastwalk`, `karrick/godirwalk`, GNU `find`.
- Metrics: entries/sec, metadata ops/sec, peak RSS, and metadata-server-side load where it can be measured.
- Targets: local ext4/XFS, NFS, and at least one of Lustre or BeeGFS. Local-disk numbers alone do not say much about HPC behavior.

## Testing

- Path and type set equality against `filepath.WalkDir` on generated trees.
- Race detector on every run.
- Chaos test: delete and rename files and directories while a walk is running. Must not panic, must count the errors.
- Permission-denied subtrees, symlink loops with `WithFollowSymlinks`, `WithOneFS` across a bind mount.
- Fuzz the `linux_dirent64` parser.
- The portable `os.ReadDir` path forced on under Linux, so it stays correct.

## Roadmap

- **v0.1** `Walk`, `Scan`, Linux `getdents64` path, stat modes, rate limiter, stats, error handling.
- **v0.2** Checkpoint and resume: a journal of completed subtrees, `WithResume(path)`.
- **v0.3** `cmd/scour` CLI (count, du-style sizes, list) as the demo and the benchmark harness.
- **v1.0** API freeze after outside users have used it.
- **v2** Multi-node: coordinator splits subtrees, nodes run `scour`, results merge.

## Non-goals

- Copying, hashing or indexing files. Build those on top.
- Sorted output.
- Replacing filesystem-native scanners.
- Performance parity on macOS or Windows. They work through the fallback and that is all.

## Prior art

- [mpiFileUtils](https://github.com/hpc/mpifileutils) (`dwalk`, `dcp`): MPI-based, the reference for multi-node walking.
- [pwalk](https://github.com/fizwit/filesystem-reporting-tools): parallel walk in C for HPC reporting.
- [fpart](https://github.com/martymac/fpart): partitions trees for parallel copies.
- [charlievieth/fastwalk](https://github.com/charlievieth/fastwalk), [karrick/godirwalk](https://github.com/karrick/godirwalk): Go walkers.

## License

MIT
