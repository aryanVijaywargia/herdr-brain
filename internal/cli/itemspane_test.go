package cli

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// listItems makes two retired items (the global item 1 and one of w14) and two archive
// candidates at day 183 (one of w14 and one global), and returns them in list order. The
// item of w15 is a candidate there, and the pinned item never ages.
func (f *fixture) listItems(t *testing.T) []int64 {
	t.Helper()
	own, _ := f.archiveItems(t)
	f.mustArchive(t, 91, "--all")
	c1 := f.addActive(t, ptr("ws_a14"), "deploy with make")
	c2 := f.addActive(t, nil, "write tests first")
	return []int64{1, own, c1, c2}
}

// itemsScreen opens the review pane of a new proposal at day 183 from pane w14:p1, types the
// keys, and returns the screen. rows 0 is a pane with no size.
func (f *fixture) itemsScreen(t *testing.T, keys string, rows, cols int) string {
	t.Helper()
	return f.itemsKeys(t, typed(keys), rows, cols)
}

// itemsKeys is itemsScreen with keys that can run a change before a key.
func (f *fixture) itemsKeys(t *testing.T, k *keys, rows, cols int) string {
	t.Helper()
	id := f.remember(t, "use yarn")
	var out bytes.Buffer
	e := f.env(f.sock, "w14:p1", "w14", &out)
	e.Now = func() time.Time { return days(183) }
	p := &reviewPane{Env: e, id: id, keys: k}
	if rows > 0 {
		p.size = func() (int, int) { return rows, cols }
	}
	if err := p.run(context.Background()); err != nil {
		t.Fatalf("pane(%q) = %v\n%s", string(k.keys), err, out.String())
	}
	return out.String()
}

// states returns the state of each item ids.
func (f *fixture) states(t *testing.T, ids []int64) []store.State {
	t.Helper()
	r := f.rows(t)
	s := make([]store.State, len(ids))
	for i, id := range ids {
		it, _ := r.item(t, id)
		s[i] = it.State
	}
	return s
}

func wantStates(t *testing.T, got []store.State, want ...store.State) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("states = %v, want %v", got, want)
	}
}

const (
	retired = store.StateRetired
	active  = store.StateActive
)

// AC 1: i lists the retired items and the archive candidates of the workspace and the global
// items; another workspace's item stays out. q goes back to the proposal. The list changes
// nothing. Golden screen.
func TestItemsPaneList(t *testing.T) {
	inZone(t)
	f := newFixture(t)
	ids := f.listItems(t)
	before := f.rows(t).newest
	screen := f.itemsScreen(t, "ij q", 0, 0)
	if strings.Contains(screen, "blog uses hugo") || strings.Contains(screen, "the yearly release steps") {
		t.Errorf("screen =\n%s\nwant no item of w15 and no pinned item", screen)
	}
	if !strings.HasSuffix(screen, "\n"+itemKeys) {
		t.Errorf("screen =\n%s\nwant the proposal again after q", screen)
	}
	wantStates(t, f.states(t, ids), retired, retired, active, active)
	if after := f.rows(t).newest; after != before {
		t.Errorf("the list wrote output generation %d", after)
	}
	f.paneGolden(t, "items", screen)
}

// AC 2: r restores the item at the cursor after one confirm; another key changes nothing.
func TestItemsPaneRestoreOne(t *testing.T) {
	f := newFixture(t)
	ids := f.listItems(t)
	if screen := f.itemsScreen(t, "irn", 0, 0); !strings.Contains(screen, "nothing changed") {
		t.Errorf("screen =\n%s\nwant nothing changed after n", screen)
	}
	wantStates(t, f.states(t, ids), retired, retired, active, active)
	screen := f.itemsScreen(t, "iry", 0, 0)
	if !strings.Contains(screen, "restore item 1? press y to confirm") || !strings.Contains(screen, "restored item 1; u undoes it") {
		t.Errorf("screen =\n%s\nwant the confirm and the restore of item 1", screen)
	}
	wantStates(t, f.states(t, ids), active, retired, active, active)
	if !strings.Contains(f.brief(t), liveText) {
		t.Errorf("brief =\n%s\nwant the restored item 1", f.brief(t))
	}
}

// AC 3 and AC 4: you select several items, archive or restore them with one confirm, and u
// undoes the whole action. An action takes only the selected items of its kind.
func TestItemsPaneBulk(t *testing.T) {
	cases := []struct {
		name          string
		keys          string
		after, undone []store.State
		screen        func(ids []int64) string
	}{
		{name: "archive", keys: "ijj j ay", after: []store.State{retired, retired, retired, retired},
			undone: []store.State{retired, retired, active, active}, screen: func(ids []int64) string {
				return fmt.Sprintf("archived 2 items (%d, %d); u undoes it", ids[2], ids[3])
			}},
		{name: "restore", keys: "i j ry", after: []store.State{active, active, active, active},
			undone: []store.State{retired, retired, active, active}, screen: func(ids []int64) string {
				return fmt.Sprintf("restored 2 items (%d, %d); u undoes it", ids[0], ids[1])
			}},
		// The selection holds a retired item and a candidate: restore takes the retired one.
		{name: "mixed restore", keys: "i jj ry", after: []store.State{active, retired, active, active},
			undone: []store.State{retired, retired, active, active}, screen: func(ids []int64) string { return fmt.Sprintf("restored item %d; u undoes it", ids[0]) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			ids := f.listItems(t)
			screen := f.itemsScreen(t, c.keys, 0, 0)
			if strings.Count(screen, "press y to confirm") != 1 {
				t.Errorf("screen =\n%s\nwant one confirm", screen)
			}
			if want := c.screen(ids); !strings.Contains(screen, want) {
				t.Errorf("screen =\n%s\nwant %q", screen, want)
			}
			wantStates(t, f.states(t, ids), c.after...)

			f = newFixture(t)
			ids = f.listItems(t)
			screen = f.itemsScreen(t, c.keys+"uu", 0, 0)
			if !strings.Contains(screen, "undid the last action on") || !strings.Contains(screen, "no action to undo") {
				t.Errorf("screen =\n%s\nwant one undo, then nothing to undo", screen)
			}
			wantStates(t, f.states(t, ids), c.undone...)
		})
	}
}

// An item that another surface changes while the confirm or the undo waits stops the whole
// action: nothing is written, and the change is not undone.
func TestItemsPaneChangedMeanwhile(t *testing.T) {
	f := newFixture(t)
	ids := f.listItems(t)
	pin := func() {
		if _, err := f.pin(t, fmt.Sprint(ids[2])); err != nil {
			t.Fatal(err)
		}
	}
	// Archive the two candidates; item ids[2] gets a pin before the y.
	screen := f.itemsKeys(t, &keys{keys: []rune("ijj j ay"), before: map[int]func(){7: pin}}, 0, 0)
	if want := fmt.Sprintf("item %d changed in another surface", ids[2]); !strings.Contains(screen, want) {
		t.Errorf("screen =\n%s\nwant %q", screen, want)
	}
	wantStates(t, f.states(t, ids), retired, retired, active, active)

	// Restore the two retired items, then item ids[0] gets a pin before the u.
	f = newFixture(t)
	ids = f.listItems(t)
	pin = func() {
		if _, err := f.pin(t, fmt.Sprint(ids[0])); err != nil {
			t.Fatal(err)
		}
	}
	screen = f.itemsKeys(t, &keys{keys: []rune("i j ryu"), before: map[int]func(){6: pin}}, 0, 0)
	if want := fmt.Sprintf("item %d changed in another surface", ids[0]); !strings.Contains(screen, want) {
		t.Errorf("screen =\n%s\nwant %q", screen, want)
	}
	wantStates(t, f.states(t, ids), active, active, active, active)
	if it, _ := f.rows(t).item(t, ids[0]); !it.Pinned {
		t.Errorf("item %d lost its pin: the undo undid the other change", ids[0])
	}
}

// AC 1: a list taller than the pane shows the items around the cursor, each on one row, and
// the keys stay at the bottom of every screen: the list, the confirm, the result and the undo
// of a bulk action of 14 items.
func TestItemsPaneTallList(t *testing.T) {
	inZone(t)
	f := newFixture(t)
	f.listItems(t)
	for i := range 12 {
		f.addActive(t, ptr("ws_a14"), fmt.Sprintf("a long rule number %d that does not fit on one row of the pane", i))
	}
	const rows, cols = 12, 60
	screen := f.itemsScreen(t, "i"+strings.Repeat(" j", 16)+"ayu"+strings.Repeat("k", 2)+" ", rows, cols)
	frames := strings.Split(screen, clearScreen)[1:]
	for _, fr := range frames {
		if n := screenRows(fr, cols); n > rows {
			t.Errorf("screen takes %d rows, want at most %d:\n%s", n, rows, fr)
		}
	}
	// The end of input closes the list, and the pane shows the proposal again.
	last := frames[len(frames)-2]
	if !strings.HasSuffix(last, "\n"+itemsKeys+"\n") || !strings.Contains(last, "> [x] item") {
		t.Errorf("last screen =\n%s\nwant the selected cursor item and the keys at the bottom", last)
	}
	confirm := slices.IndexFunc(frames, func(fr string) bool { return strings.Contains(fr, "press y to confirm") })
	if confirm < 0 || !strings.Contains(screen, "archived 14 items; u undoes it") || !strings.Contains(screen, "undid the last action on 14 items") {
		t.Fatalf("screen =\n%s\nwant the confirm, the archive and the undo of 14 items", screen)
	}
	f.paneGolden(t, "items-short-pane", clearScreen+frames[confirm])
}
