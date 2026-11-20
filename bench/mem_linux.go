package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// processMemory is the resident set of a process, now and at its peak.
func processMemory(pid int) (current, peak uint64, err error) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = f.Close() }()
	s := bufio.NewScanner(f)
	for s.Scan() {
		name, value, ok := strings.Cut(s.Text(), ":")
		if !ok || (name != "VmRSS" && name != "VmHWM") {
			continue
		}
		kb, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(value), " kB"), 10, 64)
		if err != nil {
			return 0, 0, err
		}
		if name == "VmRSS" {
			current = kb << 10
		} else {
			peak = kb << 10
		}
	}
	return current, peak, s.Err()
}
