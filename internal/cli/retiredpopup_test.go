package cli

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/herdr"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// retiredPopup runs the popup that Pane routes `brain pane retired` to, at day 183 with
// environ and the keys, and returns the whole output.
func (f *fixture) retiredPopup(t *testing.T, environ []string, keys string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	e := f.env(f.sock, "", "", &out)
	e.Environ = environ
	e.Now = func() time.Time { return days(183) }
	err := popupOf([]string{"retired"})(context.Background(), e, bufio.NewReader(strings.NewReader(keys)), func() (int, int) { return 0, 0 })
	return out.String(), err
}

// lastScreen is the last screen of out.
func lastScreen(out string) string { return out[strings.LastIndex(out, clearScreen)+len(clearScreen):] }

// E20-S11 AC 1 and 5: `brain pane retired` routes to the items list in popup mode, and
// `brain pane recall` still routes to the recall popup. Other args keep the usage.
func TestPanePopupRoute(t *testing.T) {
	f := newFixture(t)
	f.listItems(t)
	out, err := f.retiredPopup(t, f.popupEnviron("w14:p1", "w14"), "q")
	if err != nil || !strings.HasPrefix(lastScreen(out), "retired items and archive candidates") ||
		!strings.HasSuffix(out, "\n"+itemsPopupKeys+"\n") {
		t.Errorf("pane retired = %v, screen =\n%s\nwant the items list with the popup keys", err, out)
	}
	for _, args := range [][]string{{"recall"}, {"retired"}} {
		if popupOf(args) == nil {
			t.Errorf("popupOf(%q) = nil, want a popup", args)
		}
	}
	for _, args := range [][]string{nil, {"review"}, {"retired", "x"}, {"items"}} {
		if popupOf(args) != nil {
			t.Errorf("popupOf(%q) is a popup, want nil", args)
		}
	}
	if !strings.Contains(paneUsage, "brain pane retired") {
		t.Errorf("paneUsage = %q, want brain pane retired", paneUsage)
	}
	if _, err := parsePane([]string{"retired", "x"}); err == nil || !strings.Contains(err.Error(), paneUsage) {
		t.Errorf("parsePane(retired x) = %v, want the usage", err)
	}
}

// E20-S11 AC 1 and 5: the popup takes the focused pane and its workspace from the action
// context, and lists the same items as key i of the review pane in that pane: an item of
// another workspace stays out.
func TestRetiredPopupWorkspace(t *testing.T) {
	inZone(t)
	f := newFixture(t)
	f.listItems(t)
	out, err := f.retiredPopup(t, f.popupEnviron("w14:p1", "w14"), "q")
	if err != nil {
		t.Fatal(err)
	}
	popup := lastScreen(out)
	pane := f.itemsScreen(t, "iq", 0, 0)
	frames := strings.Split(pane, clearScreen)
	list := frames[len(frames)-2] // the list before q goes back to the proposal
	if want := strings.Replace(list, itemsKeys, itemsPopupKeys, 1); popup != want {
		t.Errorf("popup =\n%s\nwant the review pane list\n%s", popup, want)
	}
	if strings.Contains(popup, "blog uses hugo") {
		t.Errorf("popup =\n%s\nwant no item of w15", popup)
	}
	f.panes["w15:p1"] = herdr.Pane{ID: "w15:p1", WorkspaceID: "w15", TabID: "w15:t1"}
	out, err = f.retiredPopup(t, f.popupEnviron("w15:p1", "w15"), "q")
	if err != nil || !strings.Contains(lastScreen(out), "blog uses hugo") {
		t.Errorf("popup from w15 = %v,\n%s\nwant the item of w15", err, out)
	}
}

// E20-S11 AC 2: the popup has the keys and the checks of the review pane list: select,
// restore and archive with one confirm, undo, and the revision check. i opens nothing there.
func TestRetiredPopupKeys(t *testing.T) {
	cases := []struct {
		name  string
		keys  string
		after []store.State
		want  string
	}{
		{name: "archive", keys: "jj j ay", after: []store.State{retired, retired, retired, retired}, want: "archived 2 items"},
		{name: "restore", keys: " j ry", after: []store.State{active, active, active, active}, want: "restored 2 items"},
		{name: "no confirm", keys: "rn", after: []store.State{retired, retired, active, active}, want: "nothing changed"},
		{name: "undo", keys: "jj j ayuu", after: []store.State{retired, retired, active, active}, want: "no action to undo"},
		{name: "i stays", keys: "iry", after: []store.State{active, retired, active, active}, want: "restored item 1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			ids := f.listItems(t)
			out, err := f.retiredPopup(t, f.popupEnviron("w14:p1", "w14"), c.keys)
			if err != nil || !strings.Contains(out, c.want) || strings.Count(out, "press y to confirm") > 1 {
				t.Errorf("popup(%q) = %v,\n%s\nwant %q after at most one confirm", c.keys, err, out, c.want)
			}
			wantStates(t, f.states(t, ids), c.after...)
		})
	}
	// The revision check: a pin of a candidate before the y stops the archive.
	f := newFixture(t)
	ids := f.listItems(t)
	var out bytes.Buffer
	e := f.env(f.sock, "", "", &out)
	e.Environ = f.popupEnviron("w14:p1", "w14")
	e.Now = func() time.Time { return days(183) }
	k := &keys{keys: []rune("jj j ay"), before: map[int]func(){6: func() {
		if _, err := f.pin(t, fmt.Sprint(ids[2])); err != nil {
			t.Fatal(err)
		}
	}}}
	if err := (&itemsPane{Env: popupEnvOf(t, e), popup: true, keys: k}).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("item %d changed in another surface", ids[2]); !strings.Contains(out.String(), want) {
		t.Errorf("screen =\n%s\nwant %q", out.String(), want)
	}
	wantStates(t, f.states(t, ids), retired, retired, active, active)
}

// popupEnvOf is e with the caller of its popup env.
func popupEnvOf(t *testing.T, e Env) Env {
	t.Helper()
	var err error
	if e.Environ, err = popupEnv(e.Environ); err != nil {
		t.Fatal(err)
	}
	return e
}

// E20-S11 AC 3 and 5: q closes the popup, also at a confirm, and the keys after it do nothing. A popup with no
// confirmed caller shows the error until a key.
func TestRetiredPopupClose(t *testing.T) {
	f := newFixture(t)
	ids := f.listItems(t)
	out, err := f.retiredPopup(t, f.popupEnviron("w14:p1", "w14"), "qry")
	if err != nil || strings.Contains(out, "press y to confirm") {
		t.Errorf("popup(qry) = %v,\n%s\nwant a close at q", err, out)
	}
	wantStates(t, f.states(t, ids), retired, retired, active, active)
	// q at the confirm closes the popup too: the keys after it do nothing (Sol review).
	out, err = f.retiredPopup(t, f.popupEnviron("w14:p1", "w14"), "rqry")
	if err != nil || strings.Count(out, "press y to confirm") != 1 || strings.Contains(out, "restored") {
		t.Errorf("popup(rqry) = %v,\n%s\nwant a close at the q of the confirm", err, out)
	}
	wantStates(t, f.states(t, ids), retired, retired, active, active)
	for _, environ := range [][]string{f.popupEnviron("w99:p1", "w99"), {"HERDR_ENV=1"}} {
		out, err := f.retiredPopup(t, environ, "x")
		if err == nil || !strings.Contains(out, err.Error()) || !strings.HasSuffix(out, "Press a key to close.") {
			t.Errorf("popup(%v) = %v,\n%s\nwant the error and a key to close", environ, err, out)
		}
	}
}
