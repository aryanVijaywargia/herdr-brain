package herdr

import (
	"reflect"
	"strings"
	"testing"
)

func TestCheckVersion(t *testing.T) {
	tests := []struct {
		v            string
		wantUntested bool
		wantErr      []string // the error must contain each part; nil: no error
	}{
		{v: "0.9.1"},
		{v: "0.9.4", wantUntested: true},
		{v: "0.10.0", wantUntested: true},
		{v: "1.0.0", wantUntested: true},
		{v: "0.9.2-preview.1", wantUntested: true},
		{v: "0.9.1+build.7", wantUntested: true},
		{v: "0.8.2", wantErr: []string{"herdr 0.8.2 is too old", "0.9.1 or later", "herdr update", "restart herdr"}},
		{v: "0.9.0", wantErr: []string{"herdr 0.9.0 is too old"}},
		{v: "0.9.1-rc.1", wantErr: []string{"herdr 0.9.1-rc.1 is too old"}},
		{v: "0.9", wantErr: []string{`"0.9" is not MAJOR.MINOR.PATCH`}},
		{v: "v0.9.1", wantErr: []string{"not MAJOR.MINOR.PATCH"}},
		{v: "", wantErr: []string{"not MAJOR.MINOR.PATCH"}},
	}
	for _, tt := range tests {
		t.Run(tt.v, func(t *testing.T) {
			untested, err := checkVersion(tt.v)
			if untested != tt.wantUntested {
				t.Errorf("checkVersion(%q) untested = %v, want %v", tt.v, untested, tt.wantUntested)
			}
			wantErrParts(t, err, tt.wantErr)
		})
	}
}

func wantErrParts(t *testing.T, err error, parts []string) {
	t.Helper()
	if parts == nil {
		if err != nil {
			t.Errorf("err = %v, want nil", err)
		}
		return
	}
	if err == nil {
		t.Fatalf("err = nil, want an error with %q", parts)
	}
	for _, p := range parts {
		if !strings.Contains(err.Error(), p) {
			t.Errorf("err = %q, want it to contain %q", err, p)
		}
	}
}

// TestCheckServer uses a herdr binary that says 0.9.4 in front of a server that runs an
// older version: the server version decides.
func TestCheckServer(t *testing.T) {
	tests := []struct {
		name         string
		snapshot     string
		wantVersion  string
		wantUntested bool
		wantErr      []string
	}{
		{name: "tested server", snapshot: "snapshot-0.9.1.json", wantVersion: "0.9.1"},
		{name: "newer untested server", snapshot: "snapshot-0.9.4.json", wantVersion: "0.9.4", wantUntested: true},
		{name: "old server", snapshot: "snapshot-0.8.2.json", wantErr: []string{"herdr 0.8.2 is too old", "herdr update"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, dir := fakeClient(t, map[string]reply{
				"api snapshot": {Stdout: fixture(t, tt.snapshot)},
				"--version":    {Stdout: "herdr 0.9.4\n"},
			})
			v, untested, err := c.CheckServer(deadline(t))
			if v != tt.wantVersion || untested != tt.wantUntested {
				t.Errorf("CheckServer() = %q, %v, want %q, %v", v, untested, tt.wantVersion, tt.wantUntested)
			}
			wantErrParts(t, err, tt.wantErr)
			wantArgs := []string{"api", "snapshot"}
			if cs := calls(t, dir); len(cs) != 1 || !reflect.DeepEqual(cs[0].Args, wantArgs) {
				t.Errorf("herdr calls = %+v, want only %q", cs, wantArgs)
			}
		})
	}
}
