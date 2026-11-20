//go:build !windows && !linux

package main

import (
	"errors"
	"runtime"
)

func processMemory(int) (current, peak uint64, err error) {
	return 0, 0, errors.New("reading another process's memory is not supported on " + runtime.GOOS)
}
