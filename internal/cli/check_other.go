//go:build !linux

package cli

import (
	"context"
	"fmt"
	"io"
)

func runLocalCheck(_ context.Context, _ string, _ []string, _ io.Writer, _ io.Writer) error {
	return fmt.Errorf("trusted-local checks currently support Linux only; process-tree cancellation is not implemented for this OS")
}
