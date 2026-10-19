package cli

import (
	"bytes"
	"context"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/adapters"
	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/harness"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/install"
	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
)

// testProfiles is the Claude and Codex profiles of the embedded set. The status and doctor
// tests run with it, so a new embedded profile changes none of their outputs.
var testProfiles = func() fstest.MapFS {
	fsys := fstest.MapFS{}
	for _, k := range []string{"claude", "codex"} {
		data, err := fs.ReadFile(adapters.Profiles, k+".toml")
		if err != nil {
			panic(err)
		}
		fsys[k+".toml"] = &fstest.MapFile{Data: data}
	}
	return fsys
}()

// fakeProfiles is testProfiles and an extra fake profile, which has no Go code (E16-S10). The
// fake profile lives in test data only, never in the embedded set.
func fakeProfiles(t *testing.T) fstest.MapFS {
	t.Helper()
	fsys := maps.Clone(testProfiles)
	fsys["fake.toml"] = &fstest.MapFile{Data: readFile(t, "../adapter/testdata/fake/fake.toml")}
	return fsys
}

// installFake adds the Brain hook entry of the fake profile of fsys that runs bin.
func installFake(t *testing.T, in *install.Installer, fsys fs.FS) {
	t.Helper()
	p, err := adapter.LoadFS(fsys, "fake")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(in.Home, ".fake"), 0o700); err != nil {
		t.Fatal(err)
	}
	if st, err := in.Install(p); err != nil || st != install.Added {
		t.Fatalf("Install(fake) = %q, %v", st, err)
	}
}

// AC 1: status reports a fake extra profile with its hook entry as configured, beside Claude
// and Codex.
func TestStatusFakeProfile(t *testing.T) {
	f := newDoctorFixture(t)
	f.cli(t, "fake", "fake 0.0.1")
	fsys := fakeProfiles(t)
	bin, err := paths.BinaryPath()
	if err != nil {
		t.Fatal(err)
	}
	installFake(t, f.installer(bin), fsys)

	var out bytes.Buffer
	e := f.env(f.sock, "w14:p1", "w14", &out)
	e.Data = f.data
	e.Adapters = harness.Files{Install: filepath.Join(f.data, "install.json"),
		Conformance: filepath.Join(f.data, "conformance.json"), Retest: filepath.Join(f.data, retestDir), Installed: filepath.Join(f.data, installedDir)}
	e.StatusHerdr = func(herdr.Target) (StatusHerdr, error) { return f.herdr, nil }
	e.Now = func() time.Time { return testNow.Add(3 * time.Hour) }
	e.Profiles = fsys
	if err := Status(context.Background(), e, nil); err != nil {
		t.Fatalf("Status() = %v\n%s", err, out.String())
	}
	for _, want := range []string{"  fake 0.0.1: configured, unverified\n", "  fake: none\n", "  fake: no session observed\n"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("brain status:\n%s\nwant %q", out.String(), want)
		}
	}
}

// Doctor checks the fake profile as any other: a CLI on PATH with no Brain hook is a gap with
// its fix.
func TestDoctorFakeProfile(t *testing.T) {
	f := newDoctorFixture(t)
	f.cli(t, "fake", "fake 0.0.1")
	var out bytes.Buffer
	e := f.env(f.sock, "w14:p1", "w14", &out)
	e.Environ = append(e.Environ, "CODEX_HOME="+f.codexHome)
	e.Data = f.data
	e.Adapters = harness.Files{Install: filepath.Join(f.data, "install.json"),
		Conformance: filepath.Join(f.data, "conformance.json"), Retest: filepath.Join(f.data, retestDir), Installed: filepath.Join(f.data, installedDir)}
	e.StatusHerdr = func(herdr.Target) (StatusHerdr, error) { return f.herdr, nil }
	e.Now = func() time.Time { return testNow.Add(3 * time.Hour) }
	e.Profiles = fakeProfiles(t)
	if err := Doctor(context.Background(), e, nil); err == nil {
		t.Fatalf("Doctor() = nil, want a failed check\n%s", out.String())
	}
	want := "fake: no Brain hook in " + filepath.Join(f.home, ".fake", "hooks.json") + ", so fake sessions get no Brain items; fix: brain setup"
	if !strings.Contains(out.String(), want) {
		t.Errorf("brain doctor:\n%s\nwant %q", out.String(), want)
	}
}

// AC 3: uninstall of the fake profile keeps a hand-edited entry and reports it, and removes
// the unchanged entries of Claude and Codex.
func TestUninstallFakeProfileKeepsEditedEntry(t *testing.T) {
	home := userHome(t)
	installBrain(t, home)
	fsys := fakeProfiles(t)
	record, err := paths.InstallFile()
	if err != nil {
		t.Fatal(err)
	}
	installFake(t, &install.Installer{Home: home, Record: record, Binary: "/opt/brain/bin/brain"}, fsys)
	hooks := filepath.Join(home, ".fake", "hooks.json")
	edited := strings.Replace(string(readFile(t, hooks)), `"timeout":5`, `"timeout":9`, 1)
	putFile(t, hooks, edited)

	file, err := paths.BindingsFile()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	e := Env{Bindings: file, Stdout: &out, Stderr: &bytes.Buffer{}, Profiles: fsys}
	if err := Uninstall(context.Background(), e, []string{"--apply"}); err != nil {
		t.Fatalf("Uninstall(--apply) = %v\n%s", err, out.String())
	}
	if got := string(readFile(t, hooks)); got != edited {
		t.Errorf("edited hook file changed:\n%s", got)
	}
	for _, want := range []string{"keep the Brain hook entry for fake in " + hooks, "keep " + record,
		"removed the Brain hook entry for codex in " + filepath.Join(home, ".codex", "hooks.json")} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	rec, err := install.Read(record)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rec.Entries["fake"]; !ok || len(rec.Entries) != 1 {
		t.Errorf("record entries after uninstall = %v, want only fake", rec.Entries)
	}
}
