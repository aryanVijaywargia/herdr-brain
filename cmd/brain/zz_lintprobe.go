package main

import (
	"fmt"
	"io"
	"os"
)

// Temporary lint probe. Expect no finding on the Fprintf, and errcheck on the Close.
func lintProbeCmd(stdout io.Writer, name string) {
	fmt.Fprintf(stdout, "x\n")
	f, err := os.Create(name)
	if err != nil {
		return
	}
	f.Close()
}
