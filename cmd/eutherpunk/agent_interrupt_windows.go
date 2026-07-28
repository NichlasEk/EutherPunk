//go:build windows

package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/term"
)

const (
	windowsKeyEvent = 0x0001
	windowsVKEscape = 0x001b
)

var readConsoleInputW = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReadConsoleInputW")

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

	handle := windows.Handle(os.Stdin.Fd())
	done := make(chan struct{})
	interrupted := &atomic.Bool{}
	go func() {
		defer close(done)
		defer term.Restore(fd, oldState)

		detector := doubleEscapeDetector{}
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}

			var events uint32
			if windows.GetNumberOfConsoleInputEvents(handle, &events) != nil {
				return
			}
			for events > 0 {
				var record [20]byte
				var read uint32
				ok, _, _ := readConsoleInputW.Call(
					uintptr(handle),
					uintptr(unsafe.Pointer(&record[0])),
					1,
					uintptr(unsafe.Pointer(&read)),
				)
				if ok == 0 || read == 0 {
					return
				}
				events--
				if binary.LittleEndian.Uint16(record[0:2]) != windowsKeyEvent {
					continue
				}
				keyDown := binary.LittleEndian.Uint32(record[4:8]) != 0
				virtualKey := binary.LittleEndian.Uint16(record[10:12])
				if !keyDown || virtualKey != windowsVKEscape {
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
