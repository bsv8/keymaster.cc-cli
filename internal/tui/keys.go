package tui

import (
	"context"
	"fmt"
	"os"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/bsv8/keymaster.cc-cli/internal/download"
	"github.com/bsv8/keymaster.cc-cli/internal/scheduler"
)

// runPriceKeys reads terminal keys while the scheduler runs and feeds manual
// price events. Up/+ raises, down/- lowers, 'a' returns to automatic, Ctrl-C
// cancels the download. The reader is non-blocking and is waited for on exit,
// so it can never steal the filename-selection input afterwards.
// RunPriceKeys reads terminal keys while the scheduler runs. See RunPriceKeysFile.
func RunPriceKeys(ctx context.Context, feed func(scheduler.Event) error, view func() scheduler.View, cancel func()) {
	RunPriceKeysFile(ctx, os.Stdin, feed, view, cancel)
}

// RunPriceKeysFile is the injectable form used by terminal lifecycle tests.
func RunPriceKeysFile(ctx context.Context, input *os.File, feed func(scheduler.Event) error, view func() scheduler.View, cancel func()) {
	fd := int(input.Fd())
	raw, err := term.MakeRaw(fd)
	if err != nil {
		return
	}
	if err := syscall.SetNonblock(fd, true); err != nil {
		_ = term.Restore(fd, raw)
		return
	}
	keys := make(chan byte, 32)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		buffer := make([]byte, 1)
		for {
			n, err := syscall.Read(fd, buffer)
			if n > 0 {
				select {
				case keys <- buffer[0]:
				case <-ctx.Done():
					return
				}
				continue
			}
			if err == syscall.EAGAIN {
				select {
				case <-ctx.Done():
					return
				case <-time.After(50 * time.Millisecond):
				}
				continue
			}
			if err != nil {
				return
			}
		}
	}()
	defer func() {
		<-readerDone
		_ = syscall.SetNonblock(fd, false)
		_ = term.Restore(fd, raw)
	}()

	escape := 0
	bump := func(delta int) {
		current := view()
		if current.Price.Range == nil {
			return
		}
		next, err := download.AdjustPrice(current.Price.Effective, current.Price.Range.Min, current.Price.Range.Max, delta)
		if err != nil || next == current.Price.Effective {
			return
		}
		_ = feed(scheduler.Event{
			EventID: fmt.Sprintf("slider-%d", time.Now().UnixNano()),
			Type:    "slider", Slider: "price", Value: next,
		})
	}
	auto := func() {
		_ = feed(scheduler.Event{
			EventID: fmt.Sprintf("automatic-%d", time.Now().UnixNano()),
			Type:    "automatic", Slider: "price",
		})
	}

	for {
		select {
		case <-ctx.Done():
			return
		case key, ok := <-keys:
			if !ok {
				return
			}
			if escape == 1 {
				if key == '[' {
					escape = 2
					continue
				}
				escape = 0
				continue
			}
			if escape == 2 {
				switch key {
				case 'A':
					bump(1)
				case 'B':
					bump(-1)
				}
				escape = 0
				continue
			}
			switch key {
			case 0x03:
				if cancel != nil {
					cancel()
				}
				return
			case 0x1b:
				escape = 1
			case '+', '=', 'k':
				bump(1)
			case '-', '_', 'j':
				bump(-1)
			case 'a', 'A':
				auto()
			}
		}
	}
}
