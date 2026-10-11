//go:build unix

package hooks

import (
	"os/exec"
	"syscall"
)

// tree is the hook's process group: everything it starts joins it unless it leaves on purpose.
type tree struct {
	pgid int
}

// prepare makes the command the leader of a new process group.
func prepare(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func startTree(cmd *exec.Cmd) (*tree, error) { return &tree{pgid: cmd.Process.Pid}, nil }

func (t *tree) kill() error { return syscall.Kill(-t.pgid, syscall.SIGKILL) }

func (t *tree) close() {}
