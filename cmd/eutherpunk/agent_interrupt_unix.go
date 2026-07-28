//go:build !windows

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

func startDoubleEscapeWatcher(
	ctx context.Context,
	cancel context.CancelFunc,
	output io.Writer,
) (*agentInterruptWatcher, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return nil, nil
	}
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return nil, err
	}

	done := make(chan struct{})
	interrupted := &atomic.Bool{}
	go func() {
		defer close(done)
		defer term.Restore(fd, oldState)

		detector := doubleEscapeDetector{}
		buffer := make([]byte, 16)
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			pollFD := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
			ready, err := unix.Poll(pollFD, 75)
			if err != nil {
				if err == unix.EINTR {
					continue
				}
				return
			}
			if ready == 0 {
				continue
			}
			count, err := unix.Read(fd, buffer)
			if err != nil {
				if err == unix.EINTR || err == unix.EAGAIN {
					continue
				}
				return
			}
			for _, key := range buffer[:count] {
				if key != 0x1b {
					detector.Reset()
					continue
				}
				if detector.Press(time.Now()) {
					interrupted.Store(true)
					_, _ = fmt.Fprint(output, "\r\nAvbryter agenten …")
					cancel()
					return
				}
				_, _ = fmt.Fprint(output, "\r\nTryck Esc igen för att avbryta.\r\n")
			}
		}
	}()

	return &agentInterruptWatcher{done: done, interrupted: interrupted}, nil
}
