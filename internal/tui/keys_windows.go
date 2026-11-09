//go:build windows

package tui

import (
	"context"
	"os"

	"github.com/bsv8/keymaster.cc-download-scheduler/scheduler"
)

// RunPriceKeys is a no-op on Windows: the interactive price-key loop
// requires POSIX terminal semantics.
func RunPriceKeys(ctx context.Context, feed func(scheduler.Event) error, view func() scheduler.View, cancel func()) {
}

// RunPriceKeysFile is a no-op on Windows.
func RunPriceKeysFile(ctx context.Context, input *os.File, feed func(scheduler.Event) error, view func() scheduler.View, cancel func()) {
}
