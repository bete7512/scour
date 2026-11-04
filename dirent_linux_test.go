//go:build linux

package scour

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// appendDirent adds a linux_dirent64 record laid out as the kernel does
// it: header, NUL-terminated name, padding to 8 bytes.
func appendDirent(buf []byte, ino uint64, typ byte, name string) []byte {
	reclen := (direntName + len(name) + 1 + 7) &^ 7
	rec := make([]byte, reclen)
	binary.NativeEndian.PutUint64(rec, ino)
	binary.NativeEndian.PutUint16(rec[direntReclen:], uint16(reclen))
	rec[direntType] = typ
	copy(rec[direntName:], name)
	return append(buf, rec...)
}

type dirent struct {
	name string
	typ  Type
}

func collectDirents(buf []byte) ([]dirent, error) {
	var got []dirent
	err := parseDirents(buf, func(name []byte, typ Type) error {
		got = append(got, dirent{string(name), typ})
		return nil
	})
	return got, err
}

func sampleDirents() []byte {
	var buf []byte
	buf = appendDirent(buf, 1, unix.DT_DIR, ".")
	buf = appendDirent(buf, 2, unix.DT_DIR, "..")
	buf = appendDirent(buf, 3, unix.DT_REG, "file")
	buf = appendDirent(buf, 0, unix.DT_REG, "deleted") // inode 0 is skipped
	buf = appendDirent(buf, 4, unix.DT_LNK, "link")
	buf = appendDirent(buf, 5, unix.DT_FIFO, "fifo")
	buf = appendDirent(buf, 6, unix.DT_UNKNOWN, "unknown")
	buf = appendDirent(buf, 7, unix.DT_DIR, strings.Repeat("n", 255))
	return buf
}

func TestParseDirents(t *testing.T) {
	buf := sampleDirents()
	got, err := collectDirents(buf)
	if err != nil {
		t.Fatal(err)
	}
	want := []dirent{
		{"file", TypeFile},
		{"link", TypeSymlink},
		{"fifo", TypeOther},
		{"unknown", typeUnknown},
		{strings.Repeat("n", 255), TypeDir},
	}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}

	boom := errors.New("boom")
	if err := parseDirents(buf, func([]byte, Type) error { return boom }); err != boom {
		t.Errorf("fn's error came back as %v", err)
	}

	zeroReclen := slices.Clone(buf)
	binary.NativeEndian.PutUint16(zeroReclen[direntReclen:], 0)
	hugeReclen := slices.Clone(buf)
	binary.NativeEndian.PutUint16(hugeReclen[direntReclen:], 0xffff)
	for name, bad := range map[string][]byte{
		"truncated header": buf[:10],
		"truncated record": buf[:len(buf)-3],
		"zero reclen":      zeroReclen,
		"huge reclen":      hugeReclen,
	} {
		if _, err := collectDirents(bad); !errors.Is(err, errBadDirent) {
			t.Errorf("%s: err = %v, want %v", name, err, errBadDirent)
		}
	}
}

// TestParseDirentsReal reads a real directory with getdents64 and checks
// the parse against os.ReadDir.
func TestParseDirentsReal(t *testing.T) {
	root := t.TempDir()
	want := map[string]Type{}
	for i := range 50 {
		f := filepath.Join(root, strings.Repeat("f", i+1))
		if err := os.WriteFile(f, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		want[filepath.Base(f)] = TypeFile
		d := filepath.Join(root, strings.Repeat("d", i+1))
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
		want[filepath.Base(d)] = TypeDir
	}
	if err := os.Symlink("f", filepath.Join(root, "l")); err != nil {
		t.Fatal(err)
	}
	want["l"] = TypeSymlink

	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	got := map[string]Type{}
	buf := make([]byte, minBufSize) // small, so the directory takes several reads
	for {
		n, err := unix.Getdents(fd, buf)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
		err = parseDirents(buf[:n], func(name []byte, typ Type) error {
			if typ == typeUnknown {
				t.Skip("filesystem does not report d_type")
			}
			got[string(name)] = typ
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d entries, want %d", len(got), len(want))
	}
	for name, typ := range want {
		if got[name] != typ {
			t.Errorf("%s: got %v, want %v", name, got[name], typ)
		}
	}
}

func FuzzParseDirents(f *testing.F) {
	buf := sampleDirents()
	f.Add(buf)
	f.Add(buf[:30])
	f.Add([]byte{})
	f.Add(bytes.Repeat([]byte{0xff}, 64))
	f.Fuzz(func(t *testing.T, buf []byte) {
		err := parseDirents(buf, func(name []byte, typ Type) error {
			if len(name) == 0 || bytes.IndexByte(name, 0) >= 0 {
				t.Errorf("bad name %q", name)
			}
			if s := string(name); s == "." || s == ".." {
				t.Errorf("dot entry %q leaked", s)
			}
			switch typ {
			case TypeDir, TypeFile, TypeSymlink, TypeOther, typeUnknown:
			default:
				t.Errorf("bad type %d", typ)
			}
			return nil
		})
		if err != nil && !errors.Is(err, errBadDirent) {
			t.Errorf("unexpected error %v", err)
		}
	})
}

func BenchmarkParseDirents(b *testing.B) {
	var buf []byte
	for i := 0; len(buf) < defaultBufSize-300; i++ {
		buf = appendDirent(buf, uint64(i+1), unix.DT_REG, "file-"+strings.Repeat("x", i%40)+"-name")
	}
	entries, _ := collectDirents(buf)
	b.SetBytes(int64(len(buf)))
	b.ReportAllocs()
	for b.Loop() {
		_ = parseDirents(buf, func([]byte, Type) error { return nil })
	}
	b.ReportMetric(float64(len(entries))*float64(b.N)/b.Elapsed().Seconds(), "entries/s")
}
