package scour

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"github.com/bete7512/scour/internal/treegen"
)

// benchSpec is the generated tree: 1555 directories and about 71k files,
// 40k of them in two wide directories.
var benchSpec = treegen.Spec{Depth: 4, Dirs: 6, Files: 20, Wide: 2, WideFiles: 20000}

// benchRoot returns the tree to walk: SCOUR_BENCH_ROOT if set, otherwise
// a tree generated from benchSpec in a temporary directory.
func benchRoot(b *testing.B) string {
	if root := os.Getenv("SCOUR_BENCH_ROOT"); root != "" {
		return root
	}
	root := b.TempDir()
	if _, err := treegen.Build(root, benchSpec); err != nil {
		b.Fatal(err)
	}
	return root
}

func total(s *Stats) uint64 { return s.Dirs + s.Files + s.Symlinks + s.Other }

// BenchmarkWalk walks the same tree with filepath.WalkDir, find, and scour
// in several configurations. The entries/s metric is what matters; ns/op
// is one whole walk.
func BenchmarkWalk(b *testing.B) {
	root := benchRoot(b)
	stats, err := Walk(ctx, root, nop)
	if err != nil {
		b.Fatal(err)
	}
	entries := total(stats)
	b.Logf("%s: %d entries, %d directories", root, entries, stats.Dirs)

	rate := func(b *testing.B, walked uint64) {
		b.ReportMetric(float64(walked)/b.Elapsed().Seconds(), "entries/s")
	}

	b.Run("filepath.WalkDir", func(b *testing.B) {
		var n uint64
		for b.Loop() {
			err := filepath.WalkDir(root, func(string, fs.DirEntry, error) error { n++; return nil })
			if err != nil {
				b.Fatal(err)
			}
		}
		rate(b, n)
	})

	b.Run("find", func(b *testing.B) {
		find, err := exec.LookPath("find")
		if err != nil || runtime.GOOS == "windows" {
			b.Skip("find not available")
		}
		var n uint64
		for b.Loop() {
			// -false visits every entry without printing anything.
			if err := exec.Command(find, root, "-false").Run(); err != nil {
				b.Fatal(err)
			}
			n += entries
		}
		rate(b, n)
	})

	scour := func(opts ...Option) func(b *testing.B) {
		return func(b *testing.B) {
			b.ReportAllocs()
			var n uint64
			for b.Loop() {
				stats, err := Walk(ctx, root, nop, opts...)
				if err != nil {
					b.Fatal(err)
				}
				n += total(stats)
			}
			rate(b, n)
		}
	}
	for _, w := range []int{1, 4, 16, 64} {
		b.Run("scour/workers="+strconv.Itoa(w), scour(WithWorkers(w)))
	}
	b.Run("scour/portable/workers=16", scour(WithWorkers(16), withPortableReader()))
	b.Run("scour/statfull/workers=16", scour(WithWorkers(16), WithStat(StatFull)))
	b.Run("scour/statfull/workers=64", scour(WithWorkers(64), WithStat(StatFull)))

	b.Run("scour/scan/workers=16", func(b *testing.B) {
		b.ReportAllocs()
		var n uint64
		for b.Loop() {
			s := Scan(ctx, root, WithWorkers(16))
			for range s.Entries() {
				n++
			}
			if _, err := s.Wait(); err != nil {
				b.Fatal(err)
			}
		}
		rate(b, n)
	})
}
