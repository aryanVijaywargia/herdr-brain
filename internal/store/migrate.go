package store

import (
	"fmt"
	"io/fs"
	"path"
	"strconv"
	"strings"
)

// A migration is one numbered SQL file: migrations/0001_init.sql has version 1.
type migration struct {
	version int
	name    string
	sql     string
}

// loadMigrations reads the migrations in version order. The versions must be 1, 2, 3 and so
// on with no gap, because the store's user_version is the number of applied migrations.
func loadMigrations(fsys fs.FS) ([]migration, error) {
	names, err := fs.Glob(fsys, "migrations/*.sql")
	if err != nil {
		return nil, fmt.Errorf("list migrations: %w", err)
	}
	ms := make([]migration, 0, len(names))
	for i, name := range names {
		num, _, ok := strings.Cut(path.Base(name), "_")
		v, err := strconv.Atoi(num)
		if !ok || err != nil || v != i+1 {
			return nil, fmt.Errorf("migration %s: want a name that starts with %04d_", name, i+1)
		}
		b, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("read migration: %w", err)
		}
		ms = append(ms, migration{version: v, name: path.Base(name), sql: string(b)})
	}
	return ms, nil
}
