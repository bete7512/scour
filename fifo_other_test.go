//go:build !unix

package scour

import "errors"

func mkfifo(string) error { return errors.ErrUnsupported }
