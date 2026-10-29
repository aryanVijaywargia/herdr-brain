// Package adapters holds the harness profiles: one TOML data file for each harness, embedded
// in the binary. The base adapter (internal/adapter) reads them. A harness gets no Go
// code of its own.
package adapters

import "embed"

// Profiles holds every <harness>.toml file of this folder.
//
//go:embed *.toml
var Profiles embed.FS

// Launch holds launch/<cli>.toml, the launch data of `brain <cli>` (E24-S2). It is apart from
// the profiles: the profile hash covers the whole profile file, so a launch data edit there
// would drop a saved conformance pass from verified to configured.
//
//go:embed launch/*.toml
var Launch embed.FS
