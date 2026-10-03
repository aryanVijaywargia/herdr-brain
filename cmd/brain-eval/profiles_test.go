package main

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/BurntSushi/toml"

	"github.com/aryanVijaywargia/herdr-brain/adapters"
	"github.com/aryanVijaywargia/herdr-brain/internal/adapter"
	"github.com/aryanVijaywargia/herdr-brain/internal/harness"
	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/install"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// fakeProfiles is the embedded profiles and an extra fake profile, which has no Go code
// (E16-S10). The fake profile lives in test data only, never in the embedded set.
func fakeProfiles(t *testing.T) fstest.MapFS {
	t.Helper()
	data, err := os.ReadFile("../../internal/adapter/testdata/fake/fake.toml")
	if err != nil {
		t.Fatal(err)
	}
	fsys := fstest.MapFS{"fake.toml": {Data: data}}
	for _, k := range adapter.Kinds(nil) {
		data, err := fs.ReadFile(adapters.Profiles, k+".toml")
		if err != nil {
			t.Fatal(err)
		}
		fsys[k+".toml"] = &fstest.MapFile{Data: data}
	}
	return fsys
}

// fakeEnv is an env whose runner has the fake profile, with its Brain hook installed as
// `brain setup` does it.
func fakeEnv(t *testing.T) (*env, *adapter.Profile) {
	t.Helper()
	e := newEnv(t)
	e.r.Profiles = fakeProfiles(t)
	p, err := adapter.LoadFS(e.r.Profiles, "fake")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(e.home, ".fake"), 0o700); err != nil {
		t.Fatal(err)
	}
	in := &install.Installer{Home: e.home, Record: e.r.Install, Binary: "/opt/brain/bin/brain"}
	if _, err := in.Install(p); err != nil {
		t.Fatal(err)
	}
	return e, p
}

// AC 2: the runner accepts a fake extra profile. A delivery run of it writes its conformance
// record, and only a pass marks it verified by the E8-S4 check.
func TestFakeProfileConformance(t *testing.T) {
	e, p := fakeEnv(t)
	e.fake.answer = knows
	bin := t.TempDir()
	cliOnPath(t, bin, "fake", "fake 2.1.283") // the runner's Version gives 2.1.283
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	files := harness.Files{Install: e.r.Install, Conformance: e.r.Conformance, Retest: t.TempDir()}
	if s := checkState(t, e, files, p); s == harness.Verified {
		t.Fatalf("state before a run %s, want not verified", s)
	}
	for _, tt := range []struct {
		res  store.Result
		want string
	}{{store.ResultWithheld, resultFail}, {store.ResultEmitted, harness.Pass}} {
		e.hook(t, tt.res, "")
		if _, err := e.r.Run(context.Background(), deliveryCase("fake")); err != nil {
			t.Fatal(err)
		}
		if got := readConformance(t, e)["fake"].Result; got != tt.want {
			t.Errorf("record after a %s receipt %q, want %q", tt.res, got, tt.want)
		}
		if s := checkState(t, e, files, p); (s == harness.Verified) != (tt.want == harness.Pass) {
			t.Errorf("state after a %s run %s, want verified only after a pass", tt.want, s)
		}
	}
	rs := readResults(t, e)
	if len(rs) == 0 || rs[0].Harness != "fake" || rs[0].ProfileSHA256 != p.SHA256 {
		t.Errorf("results %+v, want the fake probe with its profile hash", rs)
	}
}

// AC 2: the reset guard counts the processes of the CLI of each profile, the fake one too.
func TestFakeProfileWriterBlocksReset(t *testing.T) {
	for _, extra := range []bool{true, false} {
		e, _ := fakeEnv(t)
		if !extra {
			e.r.Profiles = nil // the embedded set has no fake profile
		}
		left := e.fake.add("w9:p1", t.TempDir())
		left.fg = []herdr.Process{{Name: "fake", PID: 9}}
		_, err := e.r.Run(context.Background(), []Case{{ID: "c", Setup: []string{"true"}}})
		if refused := err != nil && strings.Contains(err.Error(), "runs fake"); refused != extra {
			t.Errorf("with the fake profile %t: Run() = %v, want a refused reset only with it", extra, err)
		}
	}
}

// A probe file can ask the CLI of any profile, and a probe of a CLI with no profile is an
// error that names the kinds.
func TestProbeCheckKinds(t *testing.T) {
	var pf probeFile
	if _, err := toml.Decode(strings.Replace(goodProbe, `["claude", "codex"]`, `["claude", "fake"]`, 1), &pf); err != nil {
		t.Fatal(err)
	}
	if err := pf.Probe[0].check(adapter.Kinds(fakeProfiles(t))); err != nil {
		t.Errorf("check(with fake) = %v, want nil", err)
	}
	if err := pf.Probe[0].check(adapter.Kinds(nil)); err == nil || !strings.Contains(err.Error(), `harness "fake": want `+oneOf(adapter.Kinds(nil))) {
		t.Errorf("check(embedded) = %v, want the fake harness refused", err)
	}
	// The embedded kinds sort before "fake".
	if want := strings.Join(adapter.Kinds(nil), ", ") + " or fake"; oneOf(adapter.Kinds(fakeProfiles(t))) != want {
		t.Errorf("oneOf() = %q, want %q", oneOf(adapter.Kinds(fakeProfiles(t))), want)
	}
}

// An exclusion hook probe gives `brain hook` start input of the shape of its profile, the fake
// one too: input of another shape would set the adapter to retest, not test the exclusion.
func TestFakeProfileHookInput(t *testing.T) {
	e, fake := fakeEnv(t)
	bin := t.TempDir()
	write(t, filepath.Join(bin, "brain"), "#!/bin/sh\ncat > \"$HOME/hook-input-$3\"\n")
	if err := os.Chmod(filepath.Join(bin, "brain"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", e.home)
	c := Case{ID: "ex", Stage: "exclusion", Hook: "no-herdr-env",
		Probes: []Probe{{Harness: "claude"}, {Harness: "codex"}, {Harness: "fake"}}}
	if _, err := e.r.Run(context.Background(), []Case{c}); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"claude", "codex", "fake"} {
		p := fake
		if h != "fake" {
			var err error
			if p, err = adapter.Load(h); err != nil {
				t.Fatal(err)
			}
		}
		raw, err := os.ReadFile(filepath.Join(e.home, "hook-input-"+h))
		if err != nil {
			t.Fatal(err)
		}
		if in, err := p.Parse(raw); err != nil || !strings.HasPrefix(in.SessionID, "brain-eval-") {
			t.Errorf("%s: Parse(%s) = %+v, %v; want the probe's start input", h, raw, in, err)
		}
	}
}

// AC (E16-S12): a case with a probe of a profile with eval.dir runs in that fixed folder. The
// runner makes it when it is missing and empties it before each case, as a fresh git folder.
// A case of other profiles keeps a fresh temp folder; two fixed folders in one case are an
// error.
func TestFixedEvalDir(t *testing.T) {
	brainOnPath(t)
	e := newEnv(t)
	fixed := filepath.Join(t.TempDir(), "fixed")
	fsys := fakeProfiles(t)
	fsys["fake.toml"].Data = append(fsys["fake.toml"].Data, "eval.dir = "+strconv.Quote(fixed)+"\n"...)
	e.r.Profiles = fsys
	p, err := adapter.LoadFS(fsys, "fake")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(e.home, ".fake"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := (&install.Installer{Home: e.home, Record: e.r.Install, Binary: "/opt/brain/bin/brain"}).Install(p); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(fixed, 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(fixed, "stale.txt"), "from an old case")
	where := filepath.Join(t.TempDir(), "where")
	// Each step fails unless the folder is a fresh git folder; it logs the folder.
	step := "test -d .git && test ! -e stale.txt && test ! -e made.txt && touch made.txt && pwd -P >> " + shellQuote(where)
	newCase := func(id string, harnesses ...string) Case {
		c := Case{ID: id, Stage: "exclusion", Hook: "no-herdr-env", Setup: []string{step}}
		for _, h := range harnesses {
			c.Probes = append(c.Probes, Probe{Harness: h})
		}
		return c
	}
	if _, err := e.r.Run(context.Background(), []Case{newCase("a", "fake"), newCase("b", "claude", "fake"), newCase("c", "claude")}); err != nil {
		t.Fatal(err)
	}
	realFixed, err := filepath.EvalSymlinks(filepath.Dir(fixed))
	if err != nil {
		t.Fatal(err)
	}
	scratch, err := filepath.EvalSymlinks(e.r.Scratch)
	if err != nil {
		t.Fatal(err)
	}
	log, err := os.ReadFile(where)
	if err != nil {
		t.Fatal(err)
	}
	dirs := strings.Split(strings.TrimSpace(string(log)), "\n")
	want := filepath.Join(realFixed, "fixed")
	if len(dirs) != 3 || dirs[0] != want || dirs[1] != want || !strings.HasPrefix(dirs[2], scratch+string(filepath.Separator)) {
		t.Errorf("case folders %q, want %s twice, then a folder in %s", dirs, want, scratch)
	}

	// A second profile with another fixed folder.
	other := strings.Replace(string(fsys["fake.toml"].Data), `kind = "fake"`, `kind = "fake2"`, 1)
	fsys["fake2.toml"] = &fstest.MapFile{Data: []byte(strings.Replace(other, strconv.Quote(fixed), strconv.Quote(fixed+"2"), 1))}
	if _, err := e.r.Run(context.Background(), []Case{newCase("d", "fake", "fake2")}); err == nil || !strings.Contains(err.Error(), "two fixed folders") {
		t.Errorf("Run(two fixed folders) = %v, want an error", err)
	}
}
