package cli

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

func (f *fixture) pin(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := Pin(context.Background(), f.env(f.sock, "w14:p1", "w14", &out), args)
	return out.String(), err
}

func TestPin(t *testing.T) {
	f := newFixture(t)
	item := f.addActive(t, ptr("ws_a14"), "keep me")
	id := fmt.Sprint(item)
	steps := []struct {
		args   []string
		out    string
		pinned bool
		rev    int64
	}{
		{[]string{id}, "is pinned at revision 2", true, 2},
		{[]string{id}, "is pinned already", true, 2},
		{[]string{"--off", id}, "is not pinned at revision 3", false, 3},
		{[]string{"--off", id}, "is not pinned already", false, 3},
	}
	for _, s := range steps {
		out, err := f.pin(t, s.args...)
		if err != nil || !strings.Contains(out, s.out) {
			t.Errorf("Pin(%q) = %q, %v; want %q", s.args, out, err, s.out)
		}
		it, rev := f.rows(t).item(t, item)
		if it.Pinned != s.pinned || it.CurrentRevision != s.rev || rev.Text != "keep me" || rev.Author != store.AuthorYou {
			t.Errorf("after Pin(%q) item = %+v %+v, want pinned %v at revision %d", s.args, it, rev, s.pinned, s.rev)
		}
	}
}

func TestPinErrors(t *testing.T) {
	f := newFixture(t)
	draft := f.remember(t, "a draft")
	draftItem := *f.rows(t).proposal(t, draft).ItemID
	for _, c := range []struct {
		args    []string
		wantErr string
	}{
		{nil, "want one item id"},
		{[]string{"1", "2"}, "want one item id"},
		{[]string{"x"}, "bad item id"},
		{[]string{"--nope", "1"}, "flag provided but not defined"},
		{[]string{"99"}, "item 99: not found in this workspace"},
		{[]string{fmt.Sprint(draftItem)}, "only an active item can be pinned"},
	} {
		if _, err := f.pin(t, c.args...); err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("Pin(%q) = %v, want an error with %q", c.args, err, c.wantErr)
		}
	}
}
