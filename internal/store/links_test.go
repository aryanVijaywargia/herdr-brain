package store

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

// AC #358: a confirmed link points at the claim id that the other claim has, and so do the
// links that pointed at the merged claim: a key never needs two hops. A link of the other claim
// back to the merged claim ends. A pair that you keep apart gets no link again, in either order.
func TestConfirmAndKeepApart(t *testing.T) {
	ctx := context.Background()
	e := newEvidenceStore(t)
	a, b, c, d := e.claim(t, "use pnpm"), e.claim(t, "always use pnpm"), e.claim(t, "pnpm only"), e.claim(t, "no npm")
	write(t, e.Store, func(tx *sql.Tx) error {
		for _, l := range [][2]string{{b, a}, {a, b}, {c, b}, {d, a}} {
			if _, err := SameAs(ctx, tx, l[0], l[1], Match{}); err != nil {
				return err
			}
		}
		to, err := ConfirmLink(ctx, tx, b)
		if err != nil || to != a {
			return fmt.Errorf("ConfirmLink(b) = %s, %w, want a", to, err)
		}
		for id, want := range map[string]Claim{
			b: {SameAs: a, SameAsStatus: SameAsConfirmed},
			a: {},
			c: {SameAs: a, SameAsStatus: SameAsProposed},
			d: {SameAs: a, SameAsStatus: SameAsProposed},
		} {
			got, err := ClaimByID(ctx, tx, id)
			if err != nil {
				return err
			}
			if got.SameAs != want.SameAs || got.SameAsStatus != want.SameAsStatus {
				t.Errorf("claim %s link = %s %s, want %s %s", id, got.SameAs, got.SameAsStatus, want.SameAs, want.SameAsStatus)
			}
		}
		if _, err := ConfirmLink(ctx, tx, b); err == nil {
			t.Errorf("ConfirmLink of a confirmed link: no error")
		}
		if other, err := KeepApart(ctx, tx, c); err != nil || other != a {
			return fmt.Errorf("KeepApart(c) = %s, %w, want a", other, err)
		}
		for _, l := range [][2]string{{c, a}, {a, c}} {
			if got, err := SameAs(ctx, tx, l[0], l[1], Match{Clear: true}); err != nil || got != "" {
				return fmt.Errorf("SameAs of a kept-apart pair = %q, %w, want no link", got, err)
			}
		}
		got, err := ClaimByID(ctx, tx, c)
		if err == nil && (got.SameAs != "" || got.SameAsStatus != "") {
			err = fmt.Errorf("claim c link = %s %s, want none", got.SameAs, got.SameAsStatus)
		}
		return err
	})
	if got := e.claim(t, "always use pnpm"); got != a {
		t.Errorf("the merged key has claim id %s, want %s", got, a)
	}
}

// Sol review of #360: a rejection of the merged claim in a scope holds for the claim it means
// now, so a reworded telling there stays rejected. Your reason stays on the first rejection
// only.
func TestConfirmLinkKeepsRejection(t *testing.T) {
	ctx := context.Background()
	e := newEvidenceStore(t)
	a, b := e.claim(t, "use pnpm"), e.claim(t, "always use pnpm")
	write(t, e.Store, func(tx *sql.Tx) error {
		if _, err := Reject(ctx, tx, Candidate{ClaimID: b, Scope: ScopeWorkspace, WorkspaceKey: "shop", Count: 2},
			"we use npm here", testNow); err != nil {
			return err
		}
		if _, err := SameAs(ctx, tx, b, a, Match{}); err != nil {
			return err
		}
		if _, err := ConfirmLink(ctx, tx, b); err != nil {
			return err
		}
		for key, want := range map[string]bool{"shop": true, "blog": false} {
			if held, err := Rejected(ctx, tx, a, key); err != nil || held != want {
				return fmt.Errorf("Rejected(a, %s) = %v, %w, want %v", key, held, err, want)
			}
		}
		var reason string
		err := tx.QueryRowContext(ctx, `SELECT reason FROM rejections WHERE claim_id = ?`, a).Scan(&reason)
		if err == nil && reason != "" {
			err = fmt.Errorf("reason of the copied rejection = %q, want none", reason)
		}
		return err
	})
}

// Sol review of #360: keep apart ends a proposed link back from the other claim too, so the
// pair leaves the review at once.
func TestKeepApartBothWays(t *testing.T) {
	ctx := context.Background()
	e := newEvidenceStore(t)
	a, b := e.claim(t, "use pnpm"), e.claim(t, "always use pnpm")
	write(t, e.Store, func(tx *sql.Tx) error {
		for _, l := range [][2]string{{a, b}, {b, a}} {
			if _, err := SameAs(ctx, tx, l[0], l[1], Match{}); err != nil {
				return err
			}
		}
		if _, err := KeepApart(ctx, tx, a); err != nil {
			return err
		}
		return nil
	})
	if n := queryInt(t, e.Store, `SELECT count(*) FROM claims WHERE same_as IS NOT NULL OR same_as_status IS NOT NULL`); n != 0 {
		t.Errorf("claims with a link = %d, want none", n)
	}
}
