//go:build linux

package scour

import (
	"errors"
	"io/fs"

	"golang.org/x/sys/unix"
)

// linuxDir reads a directory with raw getdents64 calls into the worker's
// buffer. Compared to os.File.ReadDir this skips the per-entry DirEntry
// allocation and reads in much larger chunks.
type linuxDir struct {
	fd   int
	path string
}

// openDir opens a directory for reading. portable selects the os.File
// reader instead; tests use that to cover the fallback on Linux.
func openDir(path string, portable bool) (directory, error) {
	if portable {
		return openPortable(path)
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	for err == unix.EINTR {
		fd, err = unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	}
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	return &linuxDir{fd: fd, path: path}, nil
}

func (d *linuxDir) id() (fileID, error) {
	var st unix.Stat_t
	err := unix.Fstat(d.fd, &st)
	for err == unix.EINTR {
		err = unix.Fstat(d.fd, &st)
	}
	if err != nil {
		return fileID{}, &fs.PathError{Op: "fstat", Path: d.path, Err: err}
	}
	return fileID{dev: uint64(st.Dev), ino: st.Ino}, nil
}

func (d *linuxDir) read(buf []byte, fn func(name []byte, typ Type) error) (bool, error) {
	n, err := unix.Getdents(d.fd, buf)
	for err == unix.EINTR {
		n, err = unix.Getdents(d.fd, buf)
	}
	if err != nil {
		return false, &fs.PathError{Op: "getdents", Path: d.path, Err: err}
	}
	if n == 0 {
		return true, nil
	}
	if err := parseDirents(buf[:n], fn); err != nil {
		if errors.Is(err, errBadDirent) {
			err = &fs.PathError{Op: "getdents", Path: d.path, Err: err}
		}
		return false, err
	}
	return false, nil
}

func (d *linuxDir) close() { unix.Close(d.fd) }
