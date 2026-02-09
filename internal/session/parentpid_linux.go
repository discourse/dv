//go:build linux

package session

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// parseProcStatFields reads /proc/<pid>/stat and returns fields after the comm field.
// Returns nil if the file can't be read or parsed.
func parseProcStatFields(pid int) []string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return nil
	}
	// /proc/PID/stat format: PID (comm) state PPID ...
	// comm may contain spaces and ), so find the last ')'.
	s := string(data)
	i := strings.LastIndexByte(s, ')')
	if i < 0 || i+2 >= len(s) {
		return nil
	}
	return strings.Fields(s[i+2:])
}

// parentPID returns the parent PID of the given process by reading /proc.
// Returns -1 if the information cannot be determined.
func parentPID(pid int) int {
	fields := parseProcStatFields(pid)
	if len(fields) < 2 {
		return -1
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return -1
	}
	return ppid
}

// processStartTime returns the start time of the given process (clock ticks since boot).
// Returns 0 if the information cannot be determined.
func processStartTime(pid int) int64 {
	fields := parseProcStatFields(pid)
	if len(fields) < 20 {
		return 0
	}
	// fields[19] = starttime (field 22 in /proc/PID/stat, 0-indexed from after ')')
	st, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return 0
	}
	return st
}
