package install

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
)

// The hook record entries of E11-S5: each goes to its own event array with the tool matcher,
// the record keeps them apart from the start entry, and uninstall removes all of them and
// gives back the original bytes. The start entry bytes do not change.
func TestRecordEntries(t *testing.T) {
	in := newInstaller(t)
	p := profile(t, "claude")
	orig := fixture(t, "claude")
	path := put(t, in, p, orig)

	expect(t, "Install", Added)(in.Install(p))
	start := read(t, path)
	rec, err := Read(in.Record)
	if err != nil {
		t.Fatal(err)
	}
	startEntry := rec.Entries[p.Kind]
	for _, h := range p.Hooks()[1:] {
		expect(t, "InstallHook "+h.CLIEvent, Added)(in.InstallHook(p, h))
	}
	for _, h := range p.Hooks()[1:] {
		expect(t, "InstallHook again "+h.CLIEvent, Present)(in.InstallHook(p, h))
	}
	rec, err = Read(in.Record)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Entries[p.Kind] != startEntry || len(rec.Records) != 4 {
		t.Fatalf("record = %+v, want the same start entry and 4 hook record entries", rec)
	}
	var doc struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(read(t, path), &doc); err != nil {
		t.Fatal(err)
	}
	for _, h := range p.Hooks() {
		e, ok := rec.entry(p.Kind, h)
		if !ok || !rec.Has(p.Kind, h) {
			t.Fatalf("no record entry for %+v", h)
		}
		gs := doc.Hooks[h.CLIEvent]
		g := gs[len(gs)-1]
		if want := " hook --profile claude --event " + h.Event; len(g.Hooks) != 1 ||
			!strings.HasSuffix(g.Hooks[0].Command, want) || g.Matcher != h.Matcher || !strings.Contains(e.Text, want) {
			t.Errorf("%s group = %+v, want the matcher %q and a command that ends in %q", h.CLIEvent, g, h.Matcher, want)
		}
	}
	eq := func(a, b json.RawMessage) bool { return string(a) == string(b) }
	if !slices.EqualFunc(startGroups(t, read(t, path)), startGroups(t, start), eq) {
		t.Errorf("the start array changed after the hook record entries")
	}

	if st, err := in.WouldUninstall(p); err != nil || st != Removed {
		t.Errorf("WouldUninstall = %q, %v, want removed", st, err)
	}
	expect(t, "Uninstall", Removed)(in.Uninstall(p))
	if got := read(t, path); string(got) != string(orig) {
		t.Errorf("after uninstall:\n%s\nwant the original:\n%s", got, orig)
	}
	if _, err := os.Stat(in.Record); !os.IsNotExist(err) {
		if rec, err := Read(in.Record); err != nil || len(rec.Entries) != 0 || len(rec.Records) != 0 {
			t.Errorf("record after uninstall = %+v, %v, want no entries", rec, err)
		}
	}
	expect(t, "Uninstall again", NotInstalled)(in.Uninstall(p))
}
