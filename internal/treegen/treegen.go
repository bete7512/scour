// Package treegen builds directory trees of a known shape for tests and
// benchmarks.
package treegen

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

// A Spec describes a tree. Every directory down to Depth holds Dirs
// subdirectories, Files empty files and Symlinks symlinks to the first of
// those files; directories at Depth hold no subdirectories. Wide extra
// directories with WideFiles files each hang off the root, standing in for
// the very wide directories HPC trees tend to have.
//
// Names are d0, d1, ... for directories, f0, f1, ... for files, l0, l1, ...
// for symlinks and w0, w1, ... for the wide directories.
type Spec struct {
	Depth     int
	Dirs      int
	Files     int
	Symlinks  int
	Wide      int
	WideFiles int
}

// Counts is what Build created, not counting the root itself.
type Counts struct {
	Dirs, Files, Symlinks int
}

// Total is the number of entries a walk delivers, root included.
func (c Counts) Total() int { return 1 + c.Dirs + c.Files + c.Symlinks }

func (c *Counts) add(o Counts) {
	c.Dirs += o.Dirs
	c.Files += o.Files
	c.Symlinks += o.Symlinks
}

// Build creates the tree described by s under root, which must exist and
// be empty. The top-level subtrees are built in parallel.
func Build(root string, s Spec) (Counts, error) {
	var (
		mu    sync.Mutex
		total Counts
		first error
		wg    sync.WaitGroup
	)
	done := func(c Counts, err error) {
		mu.Lock()
		defer mu.Unlock()
		total.add(c)
		if err != nil && first == nil {
			first = err
		}
	}

	done(populate(root, s.Files, s.Symlinks))
	for i := range s.Dirs {
		dir := filepath.Join(root, "d"+strconv.Itoa(i))
		wg.Add(1)
		go func() {
			defer wg.Done()
			done(build(dir, 1, s))
		}()
	}
	for i := range s.Wide {
		dir := filepath.Join(root, "w"+strconv.Itoa(i))
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := os.Mkdir(dir, 0o755); err != nil {
				done(Counts{}, err)
				return
			}
			c, err := populate(dir, s.WideFiles, 0)
			c.Dirs = 1
			done(c, err)
		}()
	}
	wg.Wait()
	return total, first
}

// build creates dir and fills it, recursing while depth is below s.Depth.
func build(dir string, depth int, s Spec) (Counts, error) {
	if err := os.Mkdir(dir, 0o755); err != nil {
		return Counts{}, err
	}
	c, err := populate(dir, s.Files, s.Symlinks)
	c.Dirs = 1
	if err != nil || depth >= s.Depth {
		return c, err
	}
	for i := range s.Dirs {
		sub, err := build(filepath.Join(dir, "d"+strconv.Itoa(i)), depth+1, s)
		c.add(sub)
		if err != nil {
			return c, err
		}
	}
	return c, nil
}

// populate creates files empty files and links symlinks to the first one
// in dir.
func populate(dir string, files, links int) (Counts, error) {
	var c Counts
	for i := range files {
		f, err := os.OpenFile(filepath.Join(dir, "f"+strconv.Itoa(i)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			return c, err
		}
		if err := f.Close(); err != nil {
			return c, err
		}
		c.Files++
	}
	for i := range links {
		if err := os.Symlink("f0", filepath.Join(dir, "l"+strconv.Itoa(i))); err != nil {
			return c, err
		}
		c.Symlinks++
	}
	return c, nil
}
