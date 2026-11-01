//go:build linux

package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// This is explicit, trusted developer execution, not an isolation boundary.
func runLocalCheck(ctx context.Context, directory string, args []string, stdout io.Writer, stderr io.Writer) error {
	command := exec.CommandContext(ctx, args[0], args[1:]...)
	command.Dir = directory
	command.Stdout, command.Stderr = stdout, stderr
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	command.WaitDelay = time.Second
	return command.Run()
}
