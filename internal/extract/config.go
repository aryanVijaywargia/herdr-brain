package extract

import (
	"errors"
	"fmt"
	"io/fs"

	"github.com/BurntSushi/toml"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// ReadConfig returns the queue gates (§7.2 "you can change them in the config") and the caps
// (§7.3 "(defaults)") of the Brain config file at path (paths.ConfigFile), for example:
//
//	[gates]
//	correction = 3
//	[caps]
//	proposals_per_day = 10
//
// [gates] takes a minimum for each candidate kind: correction, lesson, failure, procedure,
// command_chain. [caps] takes calls_per_day, proposals_per_day and input_bytes. [NEW] table and
// key names. A key that is not set keeps its default, and a missing file gives all the
// defaults: most people never write it. An unknown key in the two tables, or a value below 1,
// is an error, so a typo does not go unseen.
func ReadConfig(path string) (store.Gates, Caps, error) {
	gates, caps := store.DefaultGates(), DefaultCaps()
	c := struct {
		Gates map[string]int `toml:"gates"`
		Caps  struct {
			Calls      int `toml:"calls_per_day"`
			Proposals  int `toml:"proposals_per_day"`
			InputBytes int `toml:"input_bytes"`
		} `toml:"caps"`
	}{}
	c.Caps.Calls, c.Caps.Proposals, c.Caps.InputBytes = caps.Calls, caps.Proposals, caps.InputBytes
	md, err := toml.DecodeFile(path, &c)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return gates, caps, nil
	case err != nil:
		return nil, Caps{}, fmt.Errorf("read config: %w", err)
	}
	for _, k := range md.Undecoded() {
		if k[0] == "caps" {
			return nil, Caps{}, fmt.Errorf("config %s: unknown key %s", path, k)
		}
	}
	for name, n := range c.Gates {
		g, ok := gates[store.CandidateKind(name)]
		if !ok {
			return nil, Caps{}, fmt.Errorf("config %s: unknown key gates.%s", path, name)
		}
		if n < 1 {
			return nil, Caps{}, fmt.Errorf("config %s: gates.%s = %d, want 1 or more", path, name, n)
		}
		g.Min = n
		gates[store.CandidateKind(name)] = g
	}
	caps = Caps{Calls: c.Caps.Calls, Proposals: c.Caps.Proposals, InputBytes: c.Caps.InputBytes}
	for name, v := range map[string]int{"calls_per_day": caps.Calls, "proposals_per_day": caps.Proposals,
		"input_bytes": caps.InputBytes} {
		if v < 1 {
			return nil, Caps{}, fmt.Errorf("config %s: caps.%s = %d, want 1 or more", path, name, v)
		}
	}
	return gates, caps, nil
}
