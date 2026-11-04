package scour

import (
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/bete7512/scour/internal/treegen"
)

// TestChaos deletes and renames files and directories while walks are
// running. The walks must finish, return no error, and only count what
// went missing.
func TestChaos(t *testing.T) {
	variants := map[string][]Option{
		"plain":              nil,
		"stat full":          {WithStat(StatFull)},
		"portable stat full": {withPortableReader(), WithStat(StatFull)},
	}
	for name, opts := range variants {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			spec := treegen.Spec{Depth: 3, Dirs: 4, Files: 10, Symlinks: 1}
			if runtime.GOOS == "windows" {
				spec.Symlinks = 0
			}
			if _, err := treegen.Build(root, spec); err != nil {
				t.Fatal(err)
			}
			var paths []string
			err := filepath.WalkDir(root, func(p string, _ fs.DirEntry, err error) error {
				if err == nil && p != root {
					paths = append(paths, p)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			rand.Shuffle(len(paths), func(i, j int) { paths[i], paths[j] = paths[j], paths[i] })

			done := make(chan struct{})
			go func() {
				defer close(done)
				for i, p := range paths {
					// Failures are expected: an earlier step may have
					// taken the parent away already.
					if i%3 == 0 {
						os.RemoveAll(p)
					} else {
						_ = os.Rename(p, p+".moved")
					}
				}
			}()

			opts := append(opts, WithWorkers(16))
			for walks, last := 0, false; !last; walks++ {
				select {
				case <-done:
					last = true
				default:
				}
				if _, err := Walk(ctx, root, nop, opts...); err != nil {
					t.Fatalf("walk %d: %v", walks, err)
				}
			}
		})
	}
}
