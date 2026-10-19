package extract

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// AC #282: the gates and caps of the config file. A missing file or key keeps the defaults;
// an unknown key or a value below 1 is an error.
func TestReadConfig(t *testing.T) {
	gates := func(kind store.CandidateKind, n int) store.Gates {
		g := store.DefaultGates()
		x := g[kind]
		x.Min = n
		g[kind] = x
		return g
	}
	tests := []struct {
		name, file string
		gates      store.Gates
		caps       Caps
		err        string
	}{
		{name: "no file", gates: store.DefaultGates(), caps: DefaultCaps()},
		{name: "other keys only", file: "live_suggestions = \"off\"\n", gates: store.DefaultGates(), caps: DefaultCaps()},
		{name: "one gate", file: "[gates]\nfailure = 4\n", gates: gates(store.CandidateFailure, 4), caps: DefaultCaps()},
		{name: "all caps", file: "[caps]\ncalls_per_day = 20\nproposals_per_day = 8\ninput_bytes = 4096\n",
			gates: store.DefaultGates(), caps: Caps{Calls: 20, Proposals: 8, InputBytes: 4096}},
		{name: "unknown gate", file: "[gates]\ncorections = 3\n", err: "unknown key gates.corections"},
		{name: "unknown cap", file: "[caps]\ncalls = 3\n", err: "unknown key caps.calls"},
		{name: "zero gate", file: "[gates]\nlesson = 0\n", err: "gates.lesson = 0, want 1 or more"},
		{name: "zero cap", file: "[caps]\nproposals_per_day = 0\n", err: "caps.proposals_per_day = 0, want 1 or more"},
		{name: "bad toml", file: "[caps\n", err: "read config"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if tt.file != "" {
				if err := os.WriteFile(path, []byte(tt.file), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			g, c, err := ReadConfig(path)
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Errorf("ReadConfig() error = %v, want %q", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c != tt.caps {
				t.Errorf("ReadConfig() caps = %+v, want %+v", c, tt.caps)
			}
			for k, want := range tt.gates {
				if g[k] != want {
					t.Errorf("ReadConfig() gate %s = %+v, want %+v", k, g[k], want)
				}
			}
		})
	}
}

// AC #282: a gate from the config changes what the pass proposes. One correction is below the
// default gate of 2 and makes no proposal; with gates.correction = 1 it does.
func TestConfigGate(t *testing.T) {
	for _, tt := range []struct {
		name, file string
		want       int
	}{
		{"default gate", "", 0},
		{"gate of 1", "[gates]\ncorrection = 1\n", 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(tt.file), 0o600); err != nil {
				t.Fatal(err)
			}
			gates, caps, err := ReadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			f.session("a", true, line{"ws-A", pnpm, t0})
			p := f.pass()
			p.Gates, p.Caps = gates, caps
			if err := p.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			if n := f.count(`SELECT COUNT(*) FROM proposals`); n != tt.want {
				t.Errorf("proposals = %d, want %d", n, tt.want)
			}
		})
	}
}
