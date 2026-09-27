package main

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// Temporary probe: each function breaks one rule on purpose. Reverted in the next commit.

func probeErrcheck() {
	os.Remove("x")
}

func probeErrorlint(err error) bool {
	return err == os.ErrNotExist
}

func probeNilerr() error {
	if _, err := os.Stat("x"); err != nil {
		return nil
	}
	return errors.New("x")
}

func probeForce(v any) string {
	return v.(string)
}

func probeErrorString() error {
	return errors.New("Bad error.")
}

func probeElse(a, b bool) int {
	if a {
		return 1
	} else {
		if b {
			return 2
		}
	}
	return 3
}

func probeCloser(c io.Closer) {
	c.Close()
}

// probeAllowed must not be flagged by errcheck: terminal writes and a read-only file Close.
func probeAllowed(w io.Writer, f *os.File) {
	defer f.Close()
	fmt.Fprintf(w, "ok\n")
}

var _ = []any{probeErrcheck, probeErrorlint, probeNilerr, probeForce, probeErrorString, probeElse, probeCloser, probeAllowed}
