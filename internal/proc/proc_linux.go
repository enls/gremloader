package proc

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// list reads /proc/<pid>/exe for every process. Another user's process refuses the read and is
// skipped.
func list() ([]Process, error) {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("listing processes: %w", err)
	}
	var out []Process
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		exe, err := os.Readlink(filepath.Join("/proc", e.Name(), "exe"))
		if err != nil {
			continue
		}
		// A program whose file was replaced or deleted while it runs still runs from this place,
		// and the kernel says so with a suffix.
		exe = strings.TrimSuffix(exe, " (deleted)")
		out = append(out, Process{PID: pid, Path: exe})
	}
	return out, nil
}
