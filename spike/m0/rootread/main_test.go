package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReadRoot(t *testing.T) {
	tests := []struct {
		name    string
		file    string
		want    string
		wantErr bool
	}{
		{name: "valid", file: `{"version":3,"workspaces":[{"id":"w1","identity_cwd":"/tmp/A"}]}`, want: "/tmp/A"},
		{name: "extra fields ignored", file: `{"version":3,"x":1,"workspaces":[{"id":"w1","identity_cwd":"/tmp/A","y":2}]}`, want: "/tmp/A"},
		{name: "unknown version", file: `{"version":4,"workspaces":[{"id":"w1","identity_cwd":"/tmp/A"}]}`, wantErr: true},
		{name: "no version", file: `{"workspaces":[{"id":"w1","identity_cwd":"/tmp/A"}]}`, wantErr: true},
		{name: "later entry missing field", file: `{"version":3,"workspaces":[{"id":"w1","identity_cwd":"/tmp/A"},{"id":"w2"}]}`, wantErr: true},
		{name: "earlier entry missing field", file: `{"version":3,"workspaces":[{"id":"w2"},{"id":"w1","identity_cwd":"/tmp/A"}]}`, wantErr: true},
		{name: "duplicate target id", file: `{"version":3,"workspaces":[{"id":"w1","identity_cwd":"/tmp/A"},{"id":"w1","identity_cwd":"/tmp/B"}]}`, wantErr: true},
		{name: "wrong type", file: `{"version":3,"workspaces":[{"id":"w1","identity_cwd":7}]}`, wantErr: true},
		{name: "relative root", file: `{"version":3,"workspaces":[{"id":"w1","identity_cwd":"tmp/A"}]}`, wantErr: true},
		{name: "workspace not in file", file: `{"version":3,"workspaces":[{"id":"w2","identity_cwd":"/tmp/B"}]}`, wantErr: true},
		{name: "truncated", file: `{"version":3,"workspaces":[{"id":"w1"`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "session.json")
			if err := os.WriteFile(path, []byte(tt.file), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := readRoot(path, "w1")
			if tt.wantErr {
				if !errors.Is(err, errWithheld) {
					t.Fatalf("readRoot() error = %v, want errWithheld", err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("readRoot() = %q, %v, want %q", got, err, tt.want)
			}
		})
	}
}

func TestReadRootMissingFile(t *testing.T) {
	_, err := readRoot(filepath.Join(t.TempDir(), "none.json"), "w1")
	if !errors.Is(err, errWithheld) {
		t.Fatalf("readRoot() error = %v, want errWithheld", err)
	}
}
