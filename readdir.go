package scour

import (
	"io"
	"io/fs"
	"os"
)

// A fileID names a file by device and inode.
type fileID struct {
	dev, ino uint64
}

// A directory is an open directory being read. Linux implements it with
// getdents64; everything else, and tests, with portableDir.
type directory interface {
	id() (fileID, error)
	// read takes the next batch of entries and calls fn for each one. It
	// returns eof once the directory is exhausted. An error from fn is
	// returned as is.
	read(buf []byte, fn func(name []byte, typ Type) error) (eof bool, err error)
	close()
}

// portableDir reads with os.File.ReadDir, which does not sort. Batches
// only bound memory.
type portableDir struct {
	f *os.File
}

const portableBatch = 1024

func openPortable(path string) (directory, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return &portableDir{f: f}, nil
}

func (d *portableDir) id() (fileID, error) {
	info, err := d.f.Stat()
	if err != nil {
		return fileID{}, err
	}
	id, ok := fileIDOf(info)
	if !ok {
		return fileID{}, &fs.PathError{Op: "fstat", Path: d.f.Name(), Err: errNoFileID}
	}
	return id, nil
}

func (d *portableDir) read(_ []byte, fn func(name []byte, typ Type) error) (bool, error) {
	ents, err := d.f.ReadDir(portableBatch)
	for _, ent := range ents {
		if err := fn([]byte(ent.Name()), typeOf(ent.Type())); err != nil {
			return false, err
		}
	}
	if err == io.EOF {
		return true, nil
	}
	return false, err
}

func (d *portableDir) close() { d.f.Close() }
