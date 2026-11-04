//go:build !linux

package scour

func openDir(path string, _ bool) (directory, error) {
	return openPortable(path)
}
