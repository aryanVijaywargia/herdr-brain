package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// runStore is a binding store with one active automation; it returns the store and its id.
func runStore(t *testing.T) (*Store, int64) {
	t.Helper()
	s, tl, bind := bindingStore(t)
	id, err := bind(StateActive, goTestBinding(tl))
	if err != nil {
		t.Fatal(err)
	}
	return s, id
}

func claim(t *testing.T, s *Store, automation int64, key string) int64 {
	t.Helper()
	var id int64
	write(t, s, func(tx *sql.Tx) (err error) {
		id, err = ClaimRun(context.Background(), tx, AutomationRun{AutomationID: automation, EventKey: key, PaneID: "w1:p1",
			WorkspaceID: "w1", Claimed: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)})
		return err
	})
	return id
}

func readRun(t *testing.T, s *Store, id int64) AutomationRun {
	t.Helper()
	var r AutomationRun
	write(t, s, func(tx *sql.Tx) (err error) {
		r, err = AutomationRunByID(context.Background(), tx, id)
		return err
	})
	return r
}

// AC S51: a duplicate event claims nothing. A claim starts as unknown with no launch mark.
func TestClaimRun(t *testing.T) {
	s, a := runStore(t)
	first := claim(t, s, a, "w1:p1|term_1|s1|6")
	if first == 0 {
		t.Fatal("ClaimRun() = 0, want a claim")
	}
	if got := claim(t, s, a, "w1:p1|term_1|s1|6"); got != 0 {
		t.Errorf("ClaimRun(duplicate) = %d, want 0", got)
	}
	// The same seq after a herdr server restart has a new terminal id: a new stop (E17-S1 test).
	if got := claim(t, s, a, "w1:p1|term_2|s1|6"); got == 0 || got == first {
		t.Errorf("ClaimRun(new terminal id) = %d, want a new claim", got)
	}
	r := readRun(t, s, first)
	if r.Result != RunUnknown || !r.Started.IsZero() || !r.Finished.IsZero() || r.PaneID != "w1:p1" || r.Active() {
		t.Errorf("run = %+v, want an unknown claim with no launch mark", r)
	}
}

// AC: two duplicate events at the same time claim once, so they launch once.
func TestClaimRunRace(t *testing.T) {
	s, a := runStore(t)
	const n = 8
	ids := make(chan int64, n)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := s.Write(context.Background(), func(tx *sql.Tx) error {
				id, err := ClaimRun(context.Background(), tx, AutomationRun{AutomationID: a, EventKey: "w1:p1|t|s|9",
					PaneID: "w1:p1", WorkspaceID: "w1", Claimed: time.Now()})
				ids <- id
				return err
			})
			errs <- err
		}()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	claims := 0
	for id := range ids {
		if id != 0 {
			claims++
		}
	}
	if claims != 1 {
		t.Errorf("claims = %d, want 1", claims)
	}
}

// The launch mark, the end and a cancel: an end writes once, so a cancel that comes first
// stays; the automation keeps the last result. AC S62: a run with a launch mark and no end
// stays unknown.
func TestRunEnd(t *testing.T) {
	s, a := runStore(t)
	ctx := context.Background()
	at := time.Date(2026, 10, 5, 11, 0, 0, 0, time.UTC)
	id := claim(t, s, a, "k1")
	write(t, s, func(tx *sql.Tx) error {
		if _, err := CancelRun(ctx, tx, id, at); !errors.Is(err, ErrRunNotActive) {
			t.Errorf("CancelRun(no launch mark) = %v, want ErrRunNotActive", err)
		}
		return StartRun(ctx, tx, id, 4242, at)
	})
	r := readRun(t, s, id)
	if !r.Active() || r.PID != 4242 || r.Result != RunUnknown {
		t.Errorf("run = %+v, want an active unknown run of process 4242", r)
	}
	write(t, s, func(tx *sql.Tx) error {
		if err := StartRun(ctx, tx, id, 1, at); err == nil {
			t.Error("StartRun(again) = nil, want an error")
		}
		got, err := AutomationByID(ctx, tx, a)
		if err != nil || got.LastResult != string(RunUnknown) || !got.LastRun.Equal(at) {
			t.Errorf("automation = %+v, %v, want last result unknown at %v", got, err, at)
		}
		if _, err := CancelRun(ctx, tx, id, at); err != nil {
			return err
		}
		wrote, err := FinishRun(ctx, tx, id, RunOK, "", at)
		if wrote || err != nil {
			t.Errorf("FinishRun(after cancel) = %v, %v, want no write", wrote, err)
		}
		return nil
	})
	if r := readRun(t, s, id); r.Result != RunCancelled || r.Active() {
		t.Errorf("run = %+v, want cancelled", r)
	}
	// Two runs overlap, and the newer one fails first: the older ok that ends later does not hide
	// it. A newer skipped run never ran, so it hides nothing either.
	second, third, skipped := claim(t, s, a, "k2"), claim(t, s, a, "k3"), claim(t, s, a, "k4")
	write(t, s, func(tx *sql.Tx) error {
		for _, id := range []int64{second, third} {
			if err := StartRun(ctx, tx, id, 1, at); err != nil {
				return err
			}
		}
		for _, end := range []struct {
			id     int64
			result RunResult
		}{{third, RunFailed}, {second, RunOK}, {skipped, RunSkipped}} {
			if wrote, err := FinishRun(ctx, tx, end.id, end.result, "", at.Add(time.Minute)); !wrote || err != nil {
				t.Errorf("FinishRun(%d, %s) = %v, %v, want a write", end.id, end.result, wrote, err)
			}
		}
		last, ok, err := LastRun(ctx, tx, a)
		if err != nil || !ok || last.ID != third || last.Result != RunFailed {
			t.Errorf("LastRun() = %+v, %v, %v, want run %d failed", last, ok, err, third)
		}
		got, err := AutomationByID(ctx, tx, a)
		if err != nil || got.LastResult != string(RunFailed) {
			t.Errorf("automation last result = %q, %v, want failed", got.LastResult, err)
		}
		return nil
	})
}

// The folder of an agent is the pinned folder after ~ and links; a subfolder is not.
func TestAutomationIn(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "src", "shop")
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	a := Automation{Folder: "~/src/shop"}
	for _, tt := range []struct {
		cwd  string
		want bool
	}{
		{dir, true}, {dir + "/", true}, {link, true}, {filepath.Join(dir, "sub"), false}, {home, false}, {"", false},
		{filepath.Join(home, "gone"), false},
	} {
		if got := a.In(tt.cwd, home); got != tt.want {
			t.Errorf("In(%q) = %v, want %v", tt.cwd, got, tt.want)
		}
	}
}

// A failed run keeps its cause; a cancel keeps none (#352). The cause names a program of the
// tool contract, so forget of the tool or of the automation clears it.
func TestRunCause(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)
	for _, forget := range []string{"tool", "automation"} {
		t.Run(forget, func(t *testing.T) {
			s, tl, bind := bindingStore(t)
			a, err := bind(StateActive, goTestBinding(tl))
			if err != nil {
				t.Fatal(err)
			}
			failed, cancelled := claim(t, s, a, "k1"), claim(t, s, a, "k2")
			write(t, s, func(tx *sql.Tx) error {
				for _, id := range []int64{failed, cancelled} {
					if err := StartRun(ctx, tx, id, 7, at); err != nil {
						return err
					}
				}
				if _, err := FinishRun(ctx, tx, failed, RunFailed, "step 2: npm: exit status 1", at); err != nil {
					return err
				}
				_, err := CancelRun(ctx, tx, cancelled, at)
				return err
			})
			if r := readRun(t, s, failed); r.Cause != "step 2: npm: exit status 1" {
				t.Errorf("failed run cause = %q", r.Cause)
			}
			if r := readRun(t, s, cancelled); r.Cause != "" {
				t.Errorf("cancelled run cause = %q, want none", r.Cause)
			}
			item := map[string]int64{"tool": tl.ID, "automation": a}[forget]
			var left []Field
			write(t, s, func(tx *sql.Tx) (err error) {
				if err := EraseItem(ctx, tx, item, at); err != nil {
					return err
				}
				left, err = ItemTextLeft(ctx, tx, item)
				return err
			})
			if r := readRun(t, s, failed); len(left) != 0 || r.Cause != "" {
				t.Errorf("after forget of the %s: text left %v, cause %q", forget, left, r.Cause)
			}
			// A run that ends after the forget writes no cause back.
			late := claim(t, s, a, "k3")
			write(t, s, func(tx *sql.Tx) error {
				if err := StartRun(ctx, tx, late, 7, at); err != nil {
					return err
				}
				_, err := FinishRun(ctx, tx, late, RunFailed, "step 2: npm: exit status 1", at)
				return err
			})
			if r := readRun(t, s, late); r.Result != RunFailed || r.Cause != "" {
				t.Errorf("run that ends after forget of the %s = %+v, want failed with no cause", forget, r)
			}
		})
	}
}
