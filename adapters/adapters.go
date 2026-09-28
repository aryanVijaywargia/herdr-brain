// Package adapters holds the harness profiles (HLD §8.5): one TOML data file for each harness,
// embedded in the binary. The base adapter (internal/adapter) reads them. A harness gets no Go
// code of its own.
package adapters

import "embed"

// Profiles holds every <harness>.toml file of this folder.
//
//go:embed *.toml
var Profiles embed.FS
