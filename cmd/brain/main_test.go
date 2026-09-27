package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	cases := []struct {
		args      []string
		code      int
		stdoutHas string
	}{
		{[]string{"version"}, 0, "brain dev"},
		{[]string{"status"}, 0, "not set up"},
		{[]string{"help"}, 0, "usage:"},
		{nil, 2, ""},
		{[]string{"nope"}, 2, ""},
	}
	for _, c := range cases {
		var out, errOut bytes.Buffer
		if got := run(c.args, &out, &errOut); got != c.code {
			t.Errorf("run(%v) = %d, want %d", c.args, got, c.code)
		}
		if !strings.Contains(out.String(), c.stdoutHas) {
			t.Errorf("run(%v) stdout = %q, want it to contain %q", c.args, out.String(), c.stdoutHas)
		}
	}
}
