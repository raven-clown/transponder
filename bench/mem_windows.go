package main

import (
	"errors"
	"syscall"
	"unsafe"
)

var getProcessMemoryInfo = syscall.NewLazyDLL("psapi.dll").NewProc("GetProcessMemoryInfo")

// processMemoryCounters is PROCESS_MEMORY_COUNTERS from psapi.h.
type processMemoryCounters struct {
	cb                         uint32
	pageFaultCount             uint32
	peakWorkingSetSize         uintptr
	workingSetSize             uintptr
	quotaPeakPagedPoolUsage    uintptr
	quotaPagedPoolUsage        uintptr
	quotaPeakNonPagedPoolUsage uintptr
	quotaNonPagedPoolUsage     uintptr
	pagefileUsage              uintptr
	peakPagefileUsage          uintptr
}

const processQueryLimitedInformation = 0x1000

// processMemory is the working set of a process, now and at its peak.
func processMemory(pid int) (current, peak uint64, err error) {
	if pid < 0 {
		return 0, 0, errors.New("no process has a negative id")
	}
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid)) //nolint:gosec // checked above
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = syscall.CloseHandle(h) }()
	var c processMemoryCounters
	c.cb = uint32(unsafe.Sizeof(c))
	if ok, _, err := getProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&c)), uintptr(c.cb)); ok == 0 { //nolint:gosec // the documented way to pass the struct
		return 0, 0, err
	}
	return uint64(c.workingSetSize), uint64(c.peakWorkingSetSize), nil
}
