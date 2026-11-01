//go:build linux

package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestLocalCheckGroupHelper(t *testing.T) {
	if os.Args[len(os.Args)-1] != "--spawn-local-child" {
		return
	}
	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		os.Exit(2)
	}
	if err := os.WriteFile("child.pid", []byte(fmt.Sprint(child.Process.Pid)), 0o600); err != nil {
		_ = child.Process.Kill()
		_ = child.Wait()
		os.Exit(3)
	}
	_ = child.Wait()
}

func TestLocalCheckCancellationStopsOrdinaryChildren(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		var stdout, stderr bytes.Buffer
		finished <- runLocalCheck(ctx, dir, []string{executable, "-test.run=^TestLocalCheckGroupHelper$", "--", "--spawn-local-child"}, &stdout, &stderr)
	}()
	deadline := time.After(5 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	var pid string
ready:
	for {
		select {
		case <-deadline:
			t.Fatal("helper did not start")
		case <-ticker.C:
			data, err := os.ReadFile(filepath.Join(dir, "child.pid"))
			if err == nil && len(data) > 0 {
				pid = string(data)
				break ready
			}
		}
	}
	cancel()
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("cancelled command was reported successful")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("command did not stop on cancellation")
	}
	// An unreaped zombie is not executing; no running child may outlive cancellation.
	for attempt := 0; attempt < 100; attempt++ {
		data, err := os.ReadFile("/proc/" + pid + "/stat")
		if os.IsNotExist(err) || errors.Is(err, syscall.ESRCH) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		_, status, ok := strings.Cut(string(data), ") ")
		if ok && strings.HasPrefix(status, "Z ") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("ordinary child kept running after cancellation")
}
