package scour

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bete7512/scour/internal/treegen"
)

var ctx = context.Background()

func nop(Entry) error { return nil }

// fixture builds a small tree with every entry type: nested directories,
// files, symlinks, a fifo, an empty directory, a wide directory and a
// name of maximum length.
func fixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	spec := treegen.Spec{Depth: 3, Dirs: 3, Files: 4, Symlinks: 1, Wide: 1, WideFiles: 300}
	if runtime.GOOS == "windows" {
		spec.Symlinks = 0
	}
	if _, err := treegen.Build(root, spec); err != nil {
		t.Fatal(err)
	}
	if err := mkfifo(filepath.Join(root, "fifo")); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("n", 255)
	if runtime.GOOS == "windows" {
		long = long[:100]
	}
	if err := os.WriteFile(filepath.Join(root, long), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// walkSet runs Walk and returns every delivered path with its type.
func walkSet(t *testing.T, root string, opts ...Option) (map[string]Type, *Stats) {
	t.Helper()
	var mu sync.Mutex
	got := map[string]Type{}
	stats, err := Walk(ctx, root, func(e Entry) error {
		mu.Lock()
		defer mu.Unlock()
		if _, dup := got[e.Path]; dup {
			t.Errorf("%s delivered twice", e.Path)
		}
		got[e.Path] = e.Type
		return nil
	}, opts...)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	return got, stats
}

// walkDirSet is the reference: filepath.WalkDir over the same tree.
func walkDirSet(t *testing.T, root string) map[string]Type {
	t.Helper()
	want := map[string]Type{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		want[path] = typeOf(d.Type())
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir: %v", err)
	}
	return want
}

func checkSame(t *testing.T, got, want map[string]Type) {
	t.Helper()
	if maps.Equal(got, want) {
		return
	}
	for p, typ := range want {
		if g, ok := got[p]; !ok {
			t.Errorf("missing %s (%v)", p, typ)
		} else if g != typ {
			t.Errorf("%s: got %v, want %v", p, g, typ)
		}
	}
	for p := range got {
		if _, ok := want[p]; !ok {
			t.Errorf("unexpected %s", p)
		}
	}
}

// checkStats verifies the counters against what was delivered.
func checkStats(t *testing.T, got map[string]Type, s *Stats) {
	t.Helper()
	var want Stats
	for _, typ := range got {
		switch typ {
		case TypeDir:
			want.Dirs++
		case TypeFile:
			want.Files++
		case TypeSymlink:
			want.Symlinks++
		default:
			want.Other++
		}
	}
	if s.Dirs != want.Dirs || s.Files != want.Files || s.Symlinks != want.Symlinks || s.Other != want.Other {
		t.Errorf("counts: dirs %d files %d symlinks %d other %d, want %d %d %d %d",
			s.Dirs, s.Files, s.Symlinks, s.Other, want.Dirs, want.Files, want.Symlinks, want.Other)
	}
	if s.Errors != 0 {
		t.Errorf("%d errors, first: %v", s.Errors, s.FirstErrors)
	}
	if s.Elapsed <= 0 {
		t.Error("Elapsed not set")
	}
}

func depthOf(root, path string) int {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." {
		return 0
	}
	return strings.Count(rel, sep) + 1
}

func TestWalkMatchesWalkDir(t *testing.T) {
	root := fixture(t)
	want := walkDirSet(t, root)
	cases := map[string][]Option{
		"default":             nil,
		"one worker":          {WithWorkers(1)},
		"many workers":        {WithWorkers(64)},
		"small buffer":        {WithBufferSize(1)}, // raised to the 4 KiB minimum
		"stat full":           {WithStat(StatFull)},
		"rate limited":        {WithRateLimit(1 << 20)},
		"portable":            {withPortableReader()},
		"portable one worker": {withPortableReader(), WithWorkers(1)},
		"portable stat full":  {withPortableReader(), WithStat(StatFull)},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, stats := walkSet(t, root, opts...)
			checkSame(t, got, want)
			checkStats(t, got, stats)
		})
	}
}

func TestOrder(t *testing.T) {
	root := fixture(t)
	var mu sync.Mutex
	seq := map[string]int{}
	_, err := Walk(ctx, root, func(e Entry) error {
		mu.Lock()
		defer mu.Unlock()
		seq[e.Path] = len(seq)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seq[root] != 0 {
		t.Errorf("root delivered at position %d, want 0", seq[root])
	}
	for p, n := range seq {
		if p == root {
			continue
		}
		parent := filepath.Dir(p)
		pn, ok := seq[parent]
		if !ok {
			t.Errorf("%s delivered but its directory %s was not", p, parent)
		} else if pn > n {
			t.Errorf("%s delivered at %d, before its directory at %d", p, n, pn)
		}
	}
}

func TestEntryFields(t *testing.T) {
	root := fixture(t)
	_, err := Walk(ctx, root, func(e Entry) error {
		if want := depthOf(root, e.Path); e.Depth != want {
			return fmt.Errorf("%s: Depth = %d, want %d", e.Path, e.Depth, want)
		}
		if want := filepath.Base(e.Path); e.Name != want {
			return fmt.Errorf("%s: Name = %q, want %q", e.Path, e.Name, want)
		}
		if e.Info != nil {
			return fmt.Errorf("%s: Info set without StatFull", e.Path)
		}
		info, err := e.Stat()
		if err != nil {
			return err
		}
		if typeOf(info.Mode()) != e.Type {
			return fmt.Errorf("%s: Type = %v but Stat says %v", e.Path, e.Type, info.Mode())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestStatFull(t *testing.T) {
	root := fixture(t)
	_, err := Walk(ctx, root, func(e Entry) error {
		if e.Info == nil {
			return fmt.Errorf("%s: Info is nil", e.Path)
		}
		if e.Info.Name() != e.Name {
			return fmt.Errorf("%s: Info.Name() = %q, want %q", e.Path, e.Info.Name(), e.Name)
		}
		if typeOf(e.Info.Mode()) != e.Type {
			return fmt.Errorf("%s: Type = %v, Info.Mode() = %v", e.Path, e.Type, e.Info.Mode())
		}
		info, err := e.Stat()
		if err != nil || info != e.Info {
			return fmt.Errorf("%s: Stat() did not return Info", e.Path)
		}
		return nil
	}, WithStat(StatFull))
	if err != nil {
		t.Fatal(err)
	}
}

func TestMaxDepth(t *testing.T) {
	root := fixture(t)
	all := walkDirSet(t, root)
	for _, n := range []int{0, 1, 2} {
		want := map[string]Type{}
		for p, typ := range all {
			if depthOf(root, p) <= n {
				want[p] = typ
			}
		}
		got, _ := walkSet(t, root, WithMaxDepth(n))
		checkSame(t, got, want)
	}
}

func TestSkipDir(t *testing.T) {
	root := fixture(t)
	skip := filepath.Join(root, "d0")
	check := func(t *testing.T, got map[string]Type) {
		t.Helper()
		if _, ok := got[skip]; !ok {
			t.Error("the skipped directory itself was not delivered")
		}
		for p := range got {
			if strings.HasPrefix(p, skip+sep) {
				t.Errorf("%s delivered from inside the skipped directory", p)
			}
		}
		if len(got) < 100 {
			t.Errorf("only %d entries delivered; the rest of the tree was skipped too", len(got))
		}
	}

	t.Run("from callback", func(t *testing.T) {
		var mu sync.Mutex
		got := map[string]Type{}
		_, err := Walk(ctx, root, func(e Entry) error {
			mu.Lock()
			got[e.Path] = e.Type
			mu.Unlock()
			if e.Path == skip {
				return SkipDir
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		check(t, got)
	})

	t.Run("from option", func(t *testing.T) {
		got, _ := walkSet(t, root, WithSkipDir(func(path string) bool { return path == skip }))
		check(t, got)
	})

	t.Run("on a file", func(t *testing.T) {
		// Like filepath.WalkDir: SkipDir from a non-directory skips the
		// rest of that directory.
		dir := t.TempDir()
		for i := range 100 {
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprint("f", i)), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		var files atomic.Int64
		_, err := Walk(ctx, dir, func(e Entry) error {
			if e.Type == TypeFile {
				files.Add(1)
				return SkipDir
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if files.Load() != 1 {
			t.Errorf("%d files delivered, want 1", files.Load())
		}
	})
}

func TestSkipAll(t *testing.T) {
	root := fixture(t)
	total := len(walkDirSet(t, root))
	var n atomic.Int64
	_, err := Walk(ctx, root, func(e Entry) error {
		n.Add(1)
		if e.Depth == 1 && e.Type == TypeDir {
			return SkipAll
		}
		return nil
	})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if int(n.Load()) >= total {
		t.Errorf("%d of %d entries delivered; SkipAll did not stop the walk", n.Load(), total)
	}
}

func TestCallbackError(t *testing.T) {
	root := fixture(t)
	boom := errors.New("boom")
	stats, err := Walk(ctx, root, func(e Entry) error {
		if e.Type == TypeFile {
			return boom
		}
		return nil
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if stats == nil {
		t.Fatal("Stats is nil")
	}
}

func TestCancel(t *testing.T) {
	root := fixture(t)
	cctx, cancel := context.WithCancel(ctx)
	var n atomic.Int64
	stats, err := Walk(cctx, root, func(Entry) error {
		if n.Add(1) == 10 {
			cancel()
		}
		return nil
	}, WithWorkers(4))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want %v", err, context.Canceled)
	}
	if stats == nil || stats.Dirs == 0 {
		t.Errorf("no partial stats: %+v", stats)
	}

	// Already cancelled before the walk starts.
	if _, err := Walk(cctx, root, nop); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want %v", err, context.Canceled)
	}
}

func TestErrors(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a non-root unix user")
	}
	root := fixture(t)
	locked := filepath.Join(root, "d1")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(locked, 0o755); err != nil {
			t.Error(err)
		}
	})

	t.Run("counted and skipped", func(t *testing.T) {
		got, stats := walkSet(t, root)
		if _, ok := got[locked]; !ok {
			t.Error("the unreadable directory itself was not delivered")
		}
		for p := range got {
			if strings.HasPrefix(p, locked+sep) {
				t.Errorf("%s delivered from inside the unreadable directory", p)
			}
		}
		if stats.Errors != 1 || len(stats.FirstErrors) != 1 {
			t.Fatalf("Errors = %d, FirstErrors = %v; want one", stats.Errors, stats.FirstErrors)
		}
		if e := stats.FirstErrors[0]; e.Path != locked || !errors.Is(e, fs.ErrPermission) {
			t.Errorf("FirstErrors[0] = %v, want permission error on %s", e, locked)
		}
	})

	t.Run("handler aborts", func(t *testing.T) {
		var paths []string
		_, err := Walk(ctx, root, nop, WithErrorHandler(func(path string, err error) error {
			paths = append(paths, path)
			return err
		}), WithWorkers(1))
		if !errors.Is(err, fs.ErrPermission) {
			t.Errorf("err = %v, want permission error from the handler", err)
		}
		if len(paths) != 1 || paths[0] != locked {
			t.Errorf("handler saw %v, want [%s]", paths, locked)
		}
	})

	t.Run("first errors are capped", func(t *testing.T) {
		dir := t.TempDir()
		for i := range 150 {
			d := filepath.Join(dir, fmt.Sprint("d", i))
			if err := os.Mkdir(d, 0); err != nil {
				t.Fatal(err)
			}
		}
		_, stats := walkSet(t, dir)
		if stats.Errors != 150 || len(stats.FirstErrors) != maxFirstErrors {
			t.Errorf("Errors = %d, len(FirstErrors) = %d; want 150 and %d", stats.Errors, len(stats.FirstErrors), maxFirstErrors)
		}
	})
}

func TestFollowSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	root := t.TempDir()
	a := filepath.Join(root, "a")
	for _, d := range []string{a, filepath.Join(a, "sub")} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(a, "f1"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	links := map[string]string{
		filepath.Join(a, "back"):        "..",   // loop to the root
		filepath.Join(root, "link_a"):   "a",    // second way into a
		filepath.Join(root, "link_f"):   "a/f1", // to a file
		filepath.Join(root, "dangling"): "nowhere",
	}
	for link, target := range links {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("not followed by default", func(t *testing.T) {
		got, stats := walkSet(t, root)
		checkSame(t, got, walkDirSet(t, root))
		if stats.Symlinks != 4 {
			t.Errorf("Symlinks = %d, want 4", stats.Symlinks)
		}
	})

	t.Run("followed", func(t *testing.T) {
		got, stats := walkSet(t, root, WithFollowSymlinks(), WithStat(StatFull))
		// Entries take the type of their target; the dangling link stays
		// a symlink. a and link_a are the same directory, so its contents
		// are delivered once, under whichever name was read first.
		want := Stats{Dirs: 5, Files: 2, Symlinks: 1}
		if stats.Dirs != want.Dirs || stats.Files != want.Files || stats.Symlinks != want.Symlinks || stats.Other != 0 || stats.Errors != 0 {
			t.Errorf("stats = %+v, want %+v", *stats, want)
		}
		for path, typ := range map[string]Type{
			filepath.Join(root, "link_a"):   TypeDir,
			filepath.Join(root, "link_f"):   TypeFile,
			filepath.Join(root, "dangling"): TypeSymlink,
			a:                               TypeDir,
		} {
			if got[path] != typ {
				t.Errorf("%s: got %v, want %v", path, got[path], typ)
			}
		}
		_, viaA := got[filepath.Join(a, "f1")]
		_, viaLink := got[filepath.Join(root, "link_a", "f1")]
		if viaA == viaLink {
			t.Errorf("a/f1 delivered via a: %v, via link_a: %v; want exactly one", viaA, viaLink)
		}
	})
}

func TestOneFS(t *testing.T) {
	// /dev usually has other filesystems mounted inside it (devpts,
	// tmpfs on /dev/shm), which is the only mount point setup a test can
	// count on without root.
	if runtime.GOOS != "linux" {
		t.Skip("relies on the layout of /dev on linux")
	}
	devInfo, err := os.Lstat("/dev")
	if err != nil {
		t.Skip(err)
	}
	devID, _ := fileIDOf(devInfo)
	mounts := map[string]bool{}
	ents, err := os.ReadDir("/dev")
	if err != nil {
		t.Skip(err)
	}
	for _, e := range ents {
		info, err := os.Lstat(filepath.Join("/dev", e.Name()))
		if err != nil || !info.IsDir() {
			continue
		}
		if id, _ := fileIDOf(info); id.dev != devID.dev {
			mounts[filepath.Join("/dev", e.Name())] = true
		}
	}
	if len(mounts) == 0 {
		t.Skip("nothing mounted under /dev")
	}

	var mu sync.Mutex
	crossed := map[bool]int{}
	for _, oneFS := range []bool{false, true} {
		opts := []Option{WithMaxDepth(2)}
		if oneFS {
			opts = append(opts, WithOneFS())
		}
		_, err := Walk(ctx, "/dev", func(e Entry) error {
			if e.Depth == 2 && mounts[filepath.Dir(e.Path)] {
				mu.Lock()
				crossed[oneFS]++
				mu.Unlock()
			}
			return nil
		}, opts...)
		if err != nil {
			t.Fatal(err)
		}
	}
	if crossed[false] == 0 {
		t.Skip("no entries inside the mounts under /dev")
	}
	if crossed[true] != 0 {
		t.Errorf("WithOneFS delivered %d entries from other filesystems", crossed[true])
	}
}

func TestRoot(t *testing.T) {
	t.Run("file", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "f")
		if err := os.WriteFile(root, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		got, stats := walkSet(t, root)
		if len(got) != 1 || got[root] != TypeFile || stats.Files != 1 {
			t.Errorf("got %v, stats %+v", got, *stats)
		}
	})

	t.Run("missing", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "missing")
		stats, err := Walk(ctx, root, nop)
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("err = %v, want not-exist", err)
		}
		if stats == nil || stats.Errors != 1 || len(stats.FirstErrors) != 1 || stats.FirstErrors[0].Path != root {
			t.Errorf("stats = %+v, want one error on %s", stats, root)
		}
	})

	t.Run("trailing separator", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "f"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		root := dir + sep
		got, _ := walkSet(t, root)
		want := map[string]Type{root: TypeDir, dir + sep + "f": TypeFile}
		checkSame(t, got, want)
	})

	t.Run("symlink", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symlinks need privileges on windows")
		}
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "f"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		root := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(dir, root); err != nil {
			t.Fatal(err)
		}
		got, _ := walkSet(t, root)
		checkSame(t, got, map[string]Type{root: TypeSymlink})
		got, _ = walkSet(t, root, WithFollowSymlinks())
		checkSame(t, got, map[string]Type{root: TypeDir, filepath.Join(root, "f"): TypeFile})
	})
}

func TestScan(t *testing.T) {
	root := fixture(t)
	want := walkDirSet(t, root)

	s := Scan(ctx, root, WithStat(StatFull))
	got := map[string]Type{}
	for e := range s.Entries() {
		got[e.Path] = e.Type
		if e.Info == nil {
			t.Errorf("%s: Info is nil", e.Path)
		}
	}
	stats, err := s.Wait()
	if err != nil {
		t.Fatal(err)
	}
	checkSame(t, got, want)
	checkStats(t, got, stats)

	// A consumer that stops early cancels; Wait returns and the channel closes.
	cctx, cancel := context.WithCancel(ctx)
	s = Scan(cctx, root, WithWorkers(4))
	<-s.Entries()
	cancel()
	if _, err := s.Wait(); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want %v", err, context.Canceled)
	}
	for range s.Entries() {
	}
}

func TestProgress(t *testing.T) {
	root := fixture(t)
	var calls, seen atomic.Int64
	_, err := Walk(ctx, root, func(Entry) error {
		time.Sleep(time.Millisecond)
		return nil
	}, WithWorkers(2), WithProgress(5*time.Millisecond, func(s Stats) {
		calls.Add(1)
		seen.Store(int64(s.Dirs + s.Files + s.Symlinks + s.Other))
	}))
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() == 0 {
		t.Fatal("progress callback never ran")
	}
	if seen.Load() == 0 {
		t.Error("progress snapshot had no counts")
	}
}

func TestRateLimit(t *testing.T) {
	root := t.TempDir()
	if _, err := treegen.Build(root, treegen.Spec{Depth: 1, Dirs: 40, Files: 1}); err != nil {
		t.Fatal(err)
	}
	// 41 directories, at least an open and two reads each: over 120 ops.
	// At 200/s with a burst of 20 that is at least half a second.
	start := time.Now()
	got, stats := walkSet(t, root, WithRateLimit(200))
	if elapsed := time.Since(start); elapsed < 300*time.Millisecond {
		t.Errorf("rate-limited walk took %v, want at least 300ms", elapsed)
	}
	checkSame(t, got, walkDirSet(t, root))
	checkStats(t, got, stats)
}

func TestUnknownType(t *testing.T) {
	// A filesystem that reports DT_UNKNOWN gets the entry resolved with
	// lstat. The portable reader never produces it, so drive visit directly.
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "f"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	var got []Entry
	w := newWalker(newConfig(nil), func(e Entry) error {
		got = append(got, e)
		return nil
	})
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	w.cancel = cancel
	dir := dirItem{path: root}
	for _, name := range []string{"f", "d", "missing"} {
		if err := w.visit(cctx, dir, []byte(name), typeUnknown); err != nil {
			t.Fatal(err)
		}
	}
	if len(got) != 2 || got[0].Type != TypeFile || got[1].Type != TypeDir {
		t.Errorf("got %+v, want f as file and d as dir", got)
	}
	if w.pending != 1 || len(w.stack) != 1 || w.stack[0].path != filepath.Join(root, "d") {
		t.Errorf("d not queued: pending %d, stack %v", w.pending, w.stack)
	}
	if s := w.snapshot(); s.Errors != 1 || len(s.FirstErrors) != 1 || !errors.Is(s.FirstErrors[0], fs.ErrNotExist) {
		t.Errorf("missing entry: Errors = %d, FirstErrors = %v", s.Errors, s.FirstErrors)
	}
}

func TestJoin(t *testing.T) {
	cases := []struct{ dir, want string }{
		{"", "x"},
		{sep, sep + "x"},
		{"a", "a" + sep + "x"},
		{"a" + sep, "a" + sep + "x"},
	}
	name := []byte("x")
	for _, c := range cases {
		if got := join(c.dir, name); got != c.want {
			t.Errorf("join(%q, x) = %q, want %q", c.dir, got, c.want)
		}
	}
	// Assigning to a global keeps the result from being stack-allocated.
	if n := testing.AllocsPerRun(100, func() { sink = join("dir", name) }); n != 1 {
		t.Errorf("join allocates %v times, want 1", n)
	}
}

var sink string

func TestTypeString(t *testing.T) {
	want := map[Type]string{TypeDir: "dir", TypeFile: "file", TypeSymlink: "symlink", TypeOther: "other", typeUnknown: "unknown"}
	for typ, s := range want {
		if typ.String() != s {
			t.Errorf("%d.String() = %q, want %q", typ, typ.String(), s)
		}
	}
}
