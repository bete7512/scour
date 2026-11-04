package scour

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

const maxFirstErrors = 100

// errStopDir tells a directory reader to stop handing out entries. The
// walker returns it from visit once it has decided what happens next:
// skip the rest of this directory, or end the walk.
var errStopDir = errors.New("scour: stop reading directory")

var errNoFileID = errors.New("scour: device and inode numbers are not available on this platform")

type dirItem struct {
	path  string
	depth int
}

type walker struct {
	cfg     config
	fn      WalkFunc
	limiter *limiter
	start   time.Time

	// Scheduler. stack holds directories waiting to be read; pending is
	// the number discovered but not yet finished. The walk is over when
	// pending drops to zero. done is also set to end it early.
	mu      sync.Mutex
	cond    sync.Cond
	stack   []dirItem
	pending int
	done    bool

	// Early termination. stopped is what workers check between syscalls;
	// err is the first error that ended the walk.
	stopped  atomic.Bool
	stopOnce sync.Once
	err      error
	cancel   context.CancelFunc

	// Only with WithFollowSymlinks or WithOneFS.
	rootDev uint64
	seenMu  sync.Mutex
	seen    map[fileID]struct{}

	dirs, files, symlinks, other, failures atomic.Uint64
	errMu                                  sync.Mutex
	firstErrors                            []PathError
}

func newWalker(cfg config, fn WalkFunc) *walker {
	w := &walker{cfg: cfg, fn: fn}
	w.cond.L = &w.mu
	if cfg.rate > 0 {
		w.limiter = newLimiter(cfg.rate)
	}
	if cfg.follow {
		w.seen = make(map[fileID]struct{})
	}
	return w
}

func (w *walker) walk(parent context.Context, root string) (*Stats, error) {
	w.start = time.Now()
	ctx, cancel := context.WithCancel(parent)
	w.cancel = cancel
	defer cancel()

	info, err := w.stat(root, TypeSymlink)
	if err != nil {
		w.report(root, err)
		w.stop(err)
		return w.result(parent)
	}
	if w.cfg.follow || w.cfg.oneFS {
		id, ok := fileIDOf(info)
		if !ok {
			w.stop(errNoFileID)
			return w.result(parent)
		}
		w.rootDev = id.dev
	}

	e := Entry{Path: root, Name: filepath.Base(root), Type: typeOf(info.Mode())}
	if w.cfg.stat == StatFull {
		e.Info = info
	}
	if err := w.emit(e); err != nil || w.pending == 0 {
		// Nothing to read: the root is not a directory, or fn said to
		// skip or stop, or the depth limit is zero. No workers exist
		// yet, so pending needs no lock.
		return w.result(parent)
	}

	// Wake idle workers when the context is cancelled, and run the
	// progress ticker.
	var helpers sync.WaitGroup
	helpers.Add(1)
	go func() {
		defer helpers.Done()
		<-ctx.Done()
		w.shutdown()
	}()
	if w.cfg.progress != nil {
		helpers.Add(1)
		go func() {
			defer helpers.Done()
			w.progressLoop(ctx)
		}()
	}

	var workers sync.WaitGroup
	for range w.cfg.workers {
		workers.Add(1)
		go func() {
			defer workers.Done()
			w.run(ctx)
		}()
	}
	workers.Wait()
	cancel()
	helpers.Wait()
	return w.result(parent)
}

func (w *walker) result(parent context.Context) (*Stats, error) {
	s := w.snapshot()
	if w.err != nil {
		return &s, w.err
	}
	return &s, parent.Err()
}

func (w *walker) progressLoop(ctx context.Context) {
	t := time.NewTicker(w.cfg.every)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			w.cfg.progress(w.snapshot())
		case <-ctx.Done():
			return
		}
	}
}

// run is the worker loop: pop a directory, read it, mark it finished.
func (w *walker) run(ctx context.Context) {
	buf := make([]byte, w.cfg.bufSize)
	for {
		dir, ok := w.pop()
		if !ok {
			return
		}
		w.readDir(ctx, dir, buf)
		w.finish()
	}
}

// pop takes the most recently pushed directory, waiting for one if the
// stack is empty. It returns false when the walk is over.
func (w *walker) pop() (dirItem, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for {
		if w.done {
			return dirItem{}, false
		}
		if n := len(w.stack); n > 0 {
			d := w.stack[n-1]
			w.stack = w.stack[:n-1]
			return d, true
		}
		w.cond.Wait()
	}
}

func (w *walker) push(d dirItem) {
	w.mu.Lock()
	w.pending++
	w.stack = append(w.stack, d)
	w.mu.Unlock()
	w.cond.Signal()
}

// finish marks one directory as read. The last one ends the walk.
func (w *walker) finish() {
	w.mu.Lock()
	w.pending--
	if w.pending == 0 {
		w.done = true
		w.cond.Broadcast()
	}
	w.mu.Unlock()
}

// shutdown ends the walk early. Idle workers wake up and exit; busy ones
// see stopped at their next syscall.
func (w *walker) shutdown() {
	w.stopped.Store(true)
	w.mu.Lock()
	w.done = true
	w.mu.Unlock()
	w.cond.Broadcast()
}

// stop records the error that ends the walk and starts the shutdown. Only
// the first call counts.
func (w *walker) stop(err error) {
	w.stopOnce.Do(func() {
		w.err = err
		w.cancel()
	})
}

// wait runs before every syscall. It takes a rate-limit token and reports
// whether the walk is still going.
func (w *walker) wait(ctx context.Context) bool {
	if w.limiter != nil && w.limiter.wait(ctx) != nil {
		return false
	}
	return !w.stopped.Load()
}

// readDir reads one directory and hands its entries to visit.
func (w *walker) readDir(ctx context.Context, dir dirItem, buf []byte) {
	if !w.wait(ctx) {
		return
	}
	d, err := openDir(dir.path, w.cfg.portable)
	if err != nil {
		w.report(dir.path, err)
		return
	}
	defer d.close()

	if w.cfg.follow || w.cfg.oneFS {
		id, err := d.id()
		if err != nil {
			w.report(dir.path, err)
			return
		}
		if w.cfg.oneFS && id.dev != w.rootDev {
			return
		}
		if w.cfg.follow && !w.firstVisit(id) {
			return
		}
	}

	visit := func(name []byte, typ Type) error { return w.visit(ctx, dir, name, typ) }
	for {
		if !w.wait(ctx) {
			return
		}
		eof, err := d.read(buf, visit)
		if err != nil {
			if !errors.Is(err, errStopDir) {
				w.report(dir.path, err)
			}
			return
		}
		if eof {
			return
		}
	}
}

func (w *walker) firstVisit(id fileID) bool {
	w.seenMu.Lock()
	defer w.seenMu.Unlock()
	if _, seen := w.seen[id]; seen {
		return false
	}
	w.seen[id] = struct{}{}
	return true
}

// visit builds the entry for one directory record and delivers it. The
// record is stat-ed only when its type is unknown, StatFull is on, or it
// is a symlink to follow.
func (w *walker) visit(ctx context.Context, dir dirItem, name []byte, typ Type) error {
	if w.stopped.Load() {
		return errStopDir
	}
	path := join(dir.path, name)
	e := Entry{
		Path:  path,
		Name:  path[len(path)-len(name):],
		Type:  typ,
		Depth: dir.depth + 1,
	}
	if typ == typeUnknown || w.cfg.stat == StatFull || (typ == TypeSymlink && w.cfg.follow) {
		if !w.wait(ctx) {
			return errStopDir
		}
		info, err := w.stat(path, typ)
		if err != nil {
			// Usually the entry was removed after it was listed.
			if !w.report(path, err) {
				return errStopDir
			}
			return nil
		}
		e.Type = typeOf(info.Mode())
		if w.cfg.stat == StatFull {
			e.Info = info
		}
	}
	return w.emit(e)
}

// stat classifies path. With WithFollowSymlinks a symlink is stat-ed
// through, falling back to lstat when it dangles.
func (w *walker) stat(path string, typ Type) (fs.FileInfo, error) {
	if typ == TypeSymlink && w.cfg.follow {
		info, err := os.Stat(path)
		if err == nil || !errors.Is(err, fs.ErrNotExist) {
			return info, err
		}
	}
	return os.Lstat(path)
}

// emit counts an entry, delivers it, and queues it when it is a directory
// the walk should descend into.
func (w *walker) emit(e Entry) error {
	w.count(e.Type)
	if err := w.fn(e); err != nil {
		switch {
		case errors.Is(err, SkipDir):
			if e.Type == TypeDir {
				return nil
			}
			return errStopDir
		case errors.Is(err, SkipAll):
			w.stop(nil)
		default:
			w.stop(err)
		}
		return errStopDir
	}
	if e.Type == TypeDir && w.descend(e) {
		w.push(dirItem{path: e.Path, depth: e.Depth})
	}
	return nil
}

func (w *walker) descend(e Entry) bool {
	if w.cfg.maxDepth >= 0 && e.Depth >= w.cfg.maxDepth {
		return false
	}
	return w.cfg.skipDir == nil || !w.cfg.skipDir(e.Path)
}

func (w *walker) count(t Type) {
	switch t {
	case TypeDir:
		w.dirs.Add(1)
	case TypeFile:
		w.files.Add(1)
	case TypeSymlink:
		w.symlinks.Add(1)
	default:
		w.other.Add(1)
	}
}

// report counts a failure and gives it to the error handler. It returns
// false when the handler ended the walk.
func (w *walker) report(path string, err error) bool {
	w.failures.Add(1)
	w.errMu.Lock()
	if len(w.firstErrors) < maxFirstErrors {
		w.firstErrors = append(w.firstErrors, PathError{Path: path, Err: err})
	}
	w.errMu.Unlock()
	if w.cfg.onError != nil {
		if herr := w.cfg.onError(path, err); herr != nil {
			w.stop(herr)
			return false
		}
	}
	return true
}

func (w *walker) snapshot() Stats {
	s := Stats{
		Dirs:     w.dirs.Load(),
		Files:    w.files.Load(),
		Symlinks: w.symlinks.Load(),
		Other:    w.other.Load(),
		Errors:   w.failures.Load(),
		Elapsed:  time.Since(w.start),
	}
	w.errMu.Lock()
	s.FirstErrors = append([]PathError(nil), w.firstErrors...)
	w.errMu.Unlock()
	return s
}

func typeOf(m fs.FileMode) Type {
	switch {
	case m.IsDir():
		return TypeDir
	case m&fs.ModeSymlink != 0:
		return TypeSymlink
	case m.IsRegular():
		return TypeFile
	}
	return TypeOther
}

const sep = string(os.PathSeparator)

// join is filepath.Join without the cleaning: dir, a separator unless dir
// already ends in one, then name. The compiler reuses name's bytes for the
// concatenation, so the result is the only allocation.
func join(dir string, name []byte) string {
	if dir == "" || os.IsPathSeparator(dir[len(dir)-1]) {
		return dir + string(name)
	}
	return dir + sep + string(name)
}
