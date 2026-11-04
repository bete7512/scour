//go:build !unix

package scour

import "io/fs"

func fileIDOf(fs.FileInfo) (fileID, bool) {
	return fileID{}, false
}
