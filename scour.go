package scour

import (
	"context"
	"io/fs"
	"os"
	"time"
)

// Type is the kind of an entry. It comes from the directory listing when
// the filesystem reports it there and from lstat otherwise.
type Type uint8

const (
	TypeDir Type = iota
	TypeFile
	TypeSymlink
	TypeOther // device, fifo, socket, or anything else
)

// typeUnknown marks a d_type the filesystem did not fill in. Such an
// entry is resolved with lstat before it reaches the caller.
const typeUnknown Type = 255

func (t Type) String() string {
	switch t {
	case TypeDir:
		return "dir"
	case TypeFile:
		return "file"
	case TypeSymlink:
		return "symlink"
	case TypeOther:
		return "other"
	}
	return "unknown"
}

// An Entry is one filesystem object found by a walk.
type Entry struct {
	Path  string // root joined with the path below it; not cleaned
	Name  string // last element of Path
	Type  Type
	Depth int         // 0 for the root
	Info  fs.FileInfo // set only with StatFull
}

// Stat returns Info when it is set and does an lstat otherwise.
func (e Entry) Stat() (fs.FileInfo, error) {
	if e.Info != nil {
		return e.Info, nil
	}
	return os.Lstat(e.Path)
}

// A WalkFunc is called for every entry. It runs on the worker goroutines,
// concurrently with itself.
//
// Returning SkipDir for a directory keeps the walk out of it. Returning
// SkipDir for any other entry skips the rest of that entry's directory.
// Returning SkipAll ends the walk with a nil error. Any other error ends
// the walk and is returned by Walk.
type WalkFunc func(e Entry) error

// SkipDir and SkipAll are the io/fs values, so a WalkFunc can share code
// with filepath.WalkDir.
var (
	SkipDir = fs.SkipDir
	SkipAll = fs.SkipAll
)

// A PathError is one failure recorded during a walk.
type PathError struct {
	Path string
	Err  error
}

func (e PathError) Error() string { return e.Path + ": " + e.Err.Error() }
func (e PathError) Unwrap() error { return e.Err }

// Stats are the counters of a walk. The type counts are of entries
// delivered to the caller.
type Stats struct {
	Dirs, Files, Symlinks, Other uint64
	Errors                       uint64
	Elapsed                      time.Duration
	FirstErrors                  []PathError // at most 100, for reports
}

// Walk walks the tree rooted at root and calls fn for every entry,
// including root itself. See WalkFunc for how fn's result is used.
//
// Failures below the root are counted in Stats, handed to the error
// handler if there is one, and do not stop the walk. Walk returns an
// error if root cannot be lstat-ed, if fn or the error handler returns
// one, or if ctx is cancelled. Stats is never nil.
func Walk(ctx context.Context, root string, fn WalkFunc, opts ...Option) (*Stats, error) {
	return newWalker(newConfig(opts), fn).walk(ctx, root)
}

// A Scanner is a walk in progress whose entries arrive on a channel.
type Scanner struct {
	entries chan Entry
	done    chan struct{}
	stats   *Stats
	err     error
}

const scanBuffer = 1024

// Scan walks like Walk but sends the entries on a channel instead of
// calling a function, so one goroutine can consume them in order of
// arrival. If the consumer stops reading before the channel is closed,
// it must cancel ctx; otherwise the workers block.
func Scan(ctx context.Context, root string, opts ...Option) *Scanner {
	s := &Scanner{
		entries: make(chan Entry, scanBuffer),
		done:    make(chan struct{}),
	}
	go func() {
		defer close(s.done)
		defer close(s.entries)
		s.stats, s.err = Walk(ctx, root, func(e Entry) error {
			select {
			case s.entries <- e:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}, opts...)
	}()
	return s
}

// Entries returns the channel entries arrive on. It is closed when the
// walk ends.
func (s *Scanner) Entries() <-chan Entry { return s.entries }

// Wait blocks until the walk ends and returns its result, as Walk would.
func (s *Scanner) Wait() (*Stats, error) {
	<-s.done
	return s.stats, s.err
}
