package main

import "testing"

func TestTerminalCursorPositionAcrossWrappedLines(t *testing.T) {
	tests := []struct {
		columns int
		width   int
		row     int
		column  int
	}{
		{columns: 0, width: 10, row: 0, column: 1},
		{columns: 5, width: 10, row: 0, column: 6},
		{columns: 10, width: 10, row: 0, column: 10},
		{columns: 11, width: 10, row: 1, column: 2},
		{columns: 21, width: 10, row: 2, column: 2},
	}
	for _, test := range tests {
		row, column := terminalCursorPosition(test.columns, test.width)
		if row != test.row || column != test.column {
			t.Fatalf(
				"terminalCursorPosition(%d, %d) = (%d, %d), want (%d, %d)",
				test.columns,
				test.width,
				row,
				column,
				test.row,
				test.column,
			)
		}
	}
}
