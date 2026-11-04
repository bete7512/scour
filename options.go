package scour

import (
	"runtime"
	"time"
)

// StatMode says when a walk calls lstat.
type StatMode uint8

const (
	// StatOnDemand never stats up front. Entry.Info is nil; call
	// Entry.Stat when you need it.
	StatOnDemand StatMode = iota
	// StatFull makes the workers lstat every entry and fill Entry.Info.
	StatFull
)

// An Option configures Walk and Scan.
type Option func(*config)

type config struct {
	workers  int
	stat     StatMode
	rate     int // metadata ops per second; 0 is unlimited
	skipDir  func(path string) bool
	maxDepth int // negative is unlimited
	follow   bool
	oneFS    bool
	onError  func(path string, err error) error
	progress func(Stats)
	every    time.Duration
	bufSize  int
	portable bool // use the os.File reader on Linux too; for tests
}

const (
	defaultBufSize = 256 << 10
	minBufSize     = 4 << 10
)

func newConfig(opts []Option) config {
	cfg := config{
		workers:  4 * runtime.GOMAXPROCS(0),
		maxDepth: -1,
		bufSize:  defaultBufSize,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	return cfg
}

// WithWorkers sets the number of directory-reading goroutines. The
// default is 4 × GOMAXPROCS. Values below 1 keep the default.
func WithWorkers(n int) Option {
	return func(c *config) {
		if n > 0 {
			c.workers = n
		}
	}
}

// WithStat sets when entries are stat-ed. The default is StatOnDemand.
func WithStat(m StatMode) Option {
	return func(c *config) { c.stat = m }
}

// WithRateLimit caps the metadata operations (open, getdents, stat) the
// walk issues per second, across all workers. Off by default.
func WithRateLimit(opsPerSec int) Option {
	return func(c *config) { c.rate = max(opsPerSec, 0) }
}

// WithSkipDir prunes directories before they are queued. fn gets the
// directory's path; returning true leaves its contents unread. The
// directory entry itself is still delivered.
func WithSkipDir(fn func(path string) bool) Option {
	return func(c *config) { c.skipDir = fn }
}

// WithMaxDepth keeps the walk from descending below depth n. The root is
// depth 0, so WithMaxDepth(1) delivers the root and its direct children.
// A negative n means no limit.
func WithMaxDepth(n int) Option {
	return func(c *config) { c.maxDepth = n }
}

// WithFollowSymlinks follows symlinks to directories. Entries are then
// classified by what they point to, as with find -L; a dangling symlink
// stays a TypeSymlink. Each directory is read once, identified by device
// and inode, which is also what ends loops. This costs a stat per symlink
// and an fstat per directory.
func WithFollowSymlinks() Option {
	return func(c *config) { c.follow = true }
}

// WithOneFS keeps the walk on the root's filesystem, as with find -xdev.
// A mount point is delivered as a directory but its contents are not
// read. This costs an fstat per directory.
func WithOneFS() Option {
	return func(c *config) { c.oneFS = true }
}

// WithErrorHandler is called with every failure after it has been
// counted. Returning a non-nil error ends the walk, and Walk returns that
// error. Without a handler, failures are counted and the walk goes on.
func WithErrorHandler(fn func(path string, err error) error) Option {
	return func(c *config) { c.onError = fn }
}

// WithProgress calls fn with a snapshot of the counters every interval
// until the walk ends. fn runs on its own goroutine.
func WithProgress(every time.Duration, fn func(Stats)) Option {
	return func(c *config) {
		if every > 0 && fn != nil {
			c.every, c.progress = every, fn
		}
	}
}

// WithBufferSize sets the per-worker buffer that directories are read
// into. The default is 256 KiB, the minimum 4 KiB. A larger buffer means
// fewer getdents calls per directory.
func WithBufferSize(bytes int) Option {
	return func(c *config) { c.bufSize = max(bytes, minBufSize) }
}

// withPortableReader forces the os.File reader so the fallback can be
// tested on Linux. Elsewhere it is the only reader.
func withPortableReader() Option {
	return func(c *config) { c.portable = true }
}
