//go:build !linux

package session

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// parentPID returns the parent PID of the given process using ps(1).
// Returns -1 if the information cannot be determined.
func parentPID(pid int) int {
	out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return -1
	}
	ppid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return -1
	}
	return ppid
}

// processStartTime returns the start time of the given process as Unix epoch seconds.
// Returns 0 if the information cannot be determined.
func processStartTime(pid int) int64 {
	cmd := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid))
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	// lstart uses ctime(3) format: "Mon Jan  2 15:04:05 2006"
	t, err := time.Parse("Mon Jan _2 15:04:05 2006", strings.TrimSpace(string(out)))
	if err != nil {
		return 0
	}
	return t.Unix()
}
