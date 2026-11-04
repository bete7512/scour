//go:build linux

package scour

import (
	"bytes"
	"encoding/binary"
	"errors"

	"golang.org/x/sys/unix"
)

// A linux_dirent64 record as getdents64 fills it in:
//
//	offset  size  field
//	0       8     d_ino
//	8       8     d_off
//	16      2     d_reclen  whole record, padding included
//	18      1     d_type
//	19      ...   d_name    NUL-terminated
const (
	direntReclen = 16
	direntType   = 18
	direntName   = 19
)

var errBadDirent = errors.New("malformed dirent record")

// parseDirents walks the records in buf and calls fn for every entry
// other than "." and "..". name points into buf and is only valid during
// the call.
func parseDirents(buf []byte, fn func(name []byte, typ Type) error) error {
	for len(buf) > 0 {
		if len(buf) < direntName {
			return errBadDirent
		}
		reclen := int(binary.NativeEndian.Uint16(buf[direntReclen:]))
		if reclen < direntName || reclen > len(buf) {
			return errBadDirent
		}
		rec := buf[:reclen]
		buf = buf[reclen:]

		// Some filesystems leave records with inode 0 behind for deleted
		// entries. The os package skips them; so does this.
		if binary.NativeEndian.Uint64(rec) == 0 {
			continue
		}
		name := rec[direntName:]
		if i := bytes.IndexByte(name, 0); i >= 0 {
			name = name[:i]
		}
		if len(name) == 0 || string(name) == "." || string(name) == ".." {
			continue
		}
		if err := fn(name, direntTypeOf(rec[direntType])); err != nil {
			return err
		}
	}
	return nil
}

func direntTypeOf(t byte) Type {
	switch t {
	case unix.DT_DIR:
		return TypeDir
	case unix.DT_REG:
		return TypeFile
	case unix.DT_LNK:
		return TypeSymlink
	case unix.DT_UNKNOWN:
		return typeUnknown
	}
	return TypeOther
}
