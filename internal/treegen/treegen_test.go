package treegen

import (
	"io/fs"
	"path/filepath"
	"runtime"
	"testing"
)

func TestBuild(t *testing.T) {
	root := t.TempDir()
	spec := Spec{Depth: 2, Dirs: 3, Files: 2, Symlinks: 1, Wide: 2, WideFiles: 5}
	if runtime.GOOS == "windows" {
		spec.Symlinks = 0
	}
	c, err := Build(root, spec)
	if err != nil {
		t.Fatal(err)
	}

	// 3 + 9 directories from the spec, 2 wide ones; files in all 13
	// spec directories (root included) plus the wide ones.
	want := Counts{Dirs: 14, Files: 13*2 + 2*5, Symlinks: 13 * spec.Symlinks}
	if c != want {
		t.Errorf("Build returned %+v, want %+v", c, want)
	}

	n := 0
	err = filepath.WalkDir(root, func(string, fs.DirEntry, error) error { n++; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if n != c.Total() {
		t.Errorf("tree has %d entries, Counts says %d", n, c.Total())
	}
}
