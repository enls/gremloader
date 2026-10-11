package hooks

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// tree is a job object holding the hook and everything it starts (children inherit the job).
type tree struct {
	job windows.Handle
}

// prepare starts the command suspended, so it is inside the job before it runs any code and
// cannot start a process that escapes it.
func prepare(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED}
}

// startTree puts the suspended command into a new job object, then lets it run.
func startTree(cmd *exec.Cmd) (*tree, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("creating a job object: %w", err)
	}
	pid := uint32(cmd.Process.Pid) //nolint:gosec // G115: a process id fits in a DWORD
	if err := assign(job, pid); err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}
	if err := resume(pid); err != nil {
		_ = windows.TerminateJobObject(job, 1)
		_ = windows.CloseHandle(job)
		return nil, err
	}
	return &tree{job: job}, nil
}

func assign(job windows.Handle, pid uint32) error {
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		return fmt.Errorf("opening the process: %w", err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		return fmt.Errorf("putting it in a job object: %w", err)
	}
	return nil
}

// resume starts the threads of a process created suspended (there is one: the main thread).
func resume(pid uint32) error {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fmt.Errorf("listing threads: %w", err)
	}
	defer func() { _ = windows.CloseHandle(snap) }()
	var te windows.ThreadEntry32
	te.Size = uint32(unsafe.Sizeof(te)) //nolint:gosec // G103, G115: the API's own size field
	resumed := 0
	for err = windows.Thread32First(snap, &te); err == nil; err = windows.Thread32Next(snap, &te) {
		if te.OwnerProcessID != pid {
			continue
		}
		th, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, te.ThreadID)
		if err != nil {
			return fmt.Errorf("opening its thread: %w", err)
		}
		_, err = windows.ResumeThread(th)
		_ = windows.CloseHandle(th)
		if err != nil {
			return fmt.Errorf("resuming it: %w", err)
		}
		resumed++
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return fmt.Errorf("listing threads: %w", err)
	}
	if resumed == 0 {
		return errors.New("resuming it: no thread found")
	}
	return nil
}

func (t *tree) kill() error { return windows.TerminateJobObject(t.job, 1) }

// close lets go of the job without ending what is still in it: a hook may start a program that
// is meant to keep running.
func (t *tree) close() { _ = windows.CloseHandle(t.job) }
