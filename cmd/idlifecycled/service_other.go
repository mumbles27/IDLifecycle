//go:build !windows

package main

import (
	"fmt"
	"io"
)

func runService(_ []string, stderr io.Writer) int {
	fmt.Fprintln(stderr, "service mode is only available on Windows; use 'run'")
	return 2
}
