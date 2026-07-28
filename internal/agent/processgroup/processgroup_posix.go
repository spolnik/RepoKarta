//go:build !windows

package processgroup

import (
	"fmt"
	"os/exec"
	"syscall"
)

func configure(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func attach(command *exec.Cmd) (*Group, error) {
	if command.Process == nil {
		return nil, fmt.Errorf("attach process group before process start")
	}
	pid := command.Process.Pid
	return &Group{kill: func() error {
		return syscall.Kill(-pid, syscall.SIGKILL)
	}}, nil
}
