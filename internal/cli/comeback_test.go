package cli

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// itemDraft stores the active item "use pnpm" in shop with two comebacks on revision 1, and the
// draft of its change request, as E15-S1 makes them. It returns the item id and the draft id.
func (f *fixture) itemDraft(t *testing.T) (item, draft int64) {
	t.Helper()
	ctx := context.Background()
	ws := "ws_a14"
	item = f.addActive(t, &ws, "use pnpm")
	s := openStore(t, f.store)
	defer closeStore(t, s)
	write(t, s, func(tx *sql.Tx) error {
		for _, name := range []string{"a1", "a2"} {
			key := store.SessionKey{HerdrSession: "default", Harness: "claude", Kind: store.KeyCapture, ID: name}
			sid, err := store.OpenSession(ctx, tx, key, "w14:p1", ws, testNow)
			if err != nil {
				return err
			}
			seg, err := store.StartSegment(ctx, tx, sid, "w14:p1", ws, "visible", testNow)
			if err != nil {
				return err
			}
			o, err := store.AddObservation(ctx, tx, store.Observation{CaptureIdentity: name, Source: store.SourceHerdr,
				SegmentID: seg, Event: "idle", Excerpt: "no, use pnpm in " + name, Status: store.ObservationComplete,
				Sender: store.SenderUnconfirmed, Counts: true, At: testNow})
			if err != nil {
				return err
			}
			if _, err := store.AddOutcome(ctx, tx, store.Outcome{ItemID: item, Revision: 1, SessionID: sid,
				Kind: store.OutcomeComeback, ObservationID: o, At: testNow}); err != nil {
				return err
			}
		}
		if err := store.QueueItemChange(ctx, tx, item, 1, testNow); err != nil {
			return err
		}
		crs, err := store.ChangeRequests(ctx, tx)
		if err != nil {
			return err
		}
		if _, err := store.StartChange(ctx, tx, crs[0].ID, testNow, testNow); err != nil {
			return err
		}
		base := int64(1)
		draft, err = store.InsertProposal(ctx, tx, store.Proposal{ItemID: &item, BaseRevision: &base,
			Text: "use pnpm, never npm", Scope: store.ScopeWorkspace, Target: "workspace w14 (shop)",
			Diff: "- use pnpm\n+ use pnpm, never npm", Status: store.ProposalPending, Created: testNow})
		if err != nil {
			return err
		}
		_, err = store.EndChange(ctx, tx, crs[0].ID, store.ChangeCommitted, "", &draft, testNow)
		return err
	})
	return item, draft
}

// changeRequests returns the change requests, by id.
func (f *fixture) changeRequests(t *testing.T) []store.ChangeRequest {
	t.Helper()
	s := openStore(t, f.store)
	defer closeStore(t, s)
	var crs []store.ChangeRequest
	write(t, s, func(tx *sql.Tx) (err error) {
		crs, err = store.ChangeRequests(context.Background(), tx)
		return err
	})
	return crs
}

// AC (§7.4, E15-S1): the draft of an item revision shows the comeback excerpts and offers a
// change request.
func TestItemDraftShowsComebacks(t *testing.T) {
	f := newFixture(t)
	_, draft := f.itemDraft(t)
	out := f.mustReview(t, fmt.Sprint(draft))
	for _, want := range []string{"excerpts:\n  unconfirmed, session", "no, use pnpm in a1", "no, use pnpm in a2",
		`--change "<text>"`} {
		if !strings.Contains(out, want) {
			t.Errorf("show lacks %q:\n%s", want, out)
		}
	}
}

// AC: --change on the draft of an item revision queues its request again on the same key; its
// next draft redrafts the changed draft (internal/extract).
func TestItemDraftChange(t *testing.T) {
	f := newFixture(t)
	item, draft := f.itemDraft(t)
	out := f.mustReview(t, fmt.Sprint(draft), "--change", "make it a skill")
	if !strings.Contains(out, "change asked (request 1)") {
		t.Errorf("--change printed %q", out)
	}
	crs := f.changeRequests(t)
	if len(crs) != 1 || crs[0].Source != store.ChangeItem || crs[0].SourceID != item || crs[0].Revision != 1 ||
		crs[0].Status != store.ChangeQueued || crs[0].NewProposalID == nil || *crs[0].NewProposalID != draft {
		t.Errorf("change requests = %+v, want the item request queued again on draft %d", crs, draft)
	}
	r := f.rows(t)
	if p := r.proposals[len(r.proposals)-1]; p.Status != store.ProposalChanged || p.ChangeRequest == nil ||
		*p.ChangeRequest != "make it a skill" {
		t.Errorf("draft = %+v, want changed with the request", p)
	}
}

// AC: a reject of the draft of an item revision rejects only that proposal: the item stays
// active, and its claim has no rejection.
func TestItemDraftReject(t *testing.T) {
	f := newFixture(t)
	item, draft := f.itemDraft(t)
	f.mustReview(t, fmt.Sprint(draft), "--reject")
	r := f.rows(t)
	if p := r.proposals[len(r.proposals)-1]; p.Status != store.ProposalRejected {
		t.Errorf("draft status = %s, want rejected", p.Status)
	}
	if it, _ := r.item(t, item); it.State != store.StateActive || it.CurrentRevision != 1 {
		t.Errorf("item = %+v, want active at revision 1", it)
	}
	s := openStore(t, f.store)
	defer closeStore(t, s)
	write(t, s, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM rejections`).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			t.Errorf("rejections = %d, want 0", n)
		}
		return nil
	})
}
