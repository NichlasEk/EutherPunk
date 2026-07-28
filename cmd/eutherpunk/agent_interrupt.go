package main

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"time"
)

const doubleEscapeWindow = 1500 * time.Millisecond

var errAgentInterrupted = errors.New("agenten avbröts")

type doubleEscapeDetector struct {
	first time.Time
}

func (detector *doubleEscapeDetector) Press(now time.Time) bool {
	if !detector.first.IsZero() && now.Sub(detector.first) <= doubleEscapeWindow {
		detector.first = time.Time{}
		return true
	}
	detector.first = now
	return false
}

func (detector *doubleEscapeDetector) Reset() {
	detector.first = time.Time{}
}

type agentInterruptWatcher struct {
	done        <-chan struct{}
	interrupted *atomic.Bool
}

func (watcher *agentInterruptWatcher) Wait() {
	if watcher != nil {
		<-watcher.done
	}
}

func (watcher *agentInterruptWatcher) Interrupted() bool {
	return watcher != nil && watcher.interrupted.Load()
}

func runInterruptibleAgentCall(
	output io.Writer,
	call func(context.Context) (string, error),
) (string, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	watcher, watchErr := startDoubleEscapeWatcher(ctx, cancel, output)
	if watchErr != nil {
		watcher = nil
	}
	answer, err := call(ctx)
	cancel()
	watcher.Wait()
	if watcher.Interrupted() {
		return answer, errAgentInterrupted
	}
	return answer, err
}
