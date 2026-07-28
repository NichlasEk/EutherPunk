package main

import (
	"testing"
	"time"
)

func TestDoubleEscapeDetector(t *testing.T) {
	start := time.Unix(100, 0)
	detector := doubleEscapeDetector{}
	if detector.Press(start) {
		t.Fatal("first Escape must not interrupt")
	}
	if !detector.Press(start.Add(time.Second)) {
		t.Fatal("second Escape inside the window must interrupt")
	}
}

func TestDoubleEscapeDetectorExpires(t *testing.T) {
	start := time.Unix(100, 0)
	detector := doubleEscapeDetector{}
	if detector.Press(start) {
		t.Fatal("first Escape must not interrupt")
	}
	if detector.Press(start.Add(doubleEscapeWindow + time.Millisecond)) {
		t.Fatal("late second Escape must start a new pair")
	}
}
