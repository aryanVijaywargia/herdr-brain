package cli

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/extract"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

func (f *fixture) extract(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := Extract(context.Background(), f.env(f.sock, "w14:p1", "w14", &out), args)
	return out.String(), err
}

// storeWrite runs fn in one write of the fixture's store.
func (f *fixture) storeWrite(t *testing.T, fn func(ctx context.Context, tx *sql.Tx) error) {
	t.Helper()
	s := openStore(t, f.store)
	defer closeStore(t, s)
	write(t, s, func(tx *sql.Tx) error { return fn(context.Background(), tx) })
}

// signalSession adds an ended agent session of herdr session default with one correction in
// workspace key ws at testNow.
func (f *fixture) signalSession(t *testing.T, ws, name string) {
	t.Helper()
	f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) error {
		key := store.SessionKey{HerdrSession: "default", Harness: "claude", Kind: store.KeyAgent, ID: name}
		id, err := store.OpenSession(ctx, tx, key, "w14:p1", ws, testNow)
		if err != nil {
			return err
		}
		seg, err := store.StartSegment(ctx, tx, id, "w14:p1", ws, "recent_unwrapped", testNow)
		if err != nil {
			return err
		}
		if _, err := store.AddObservation(ctx, tx, store.Observation{CaptureIdentity: name, Source: store.SourceHerdr,
			SegmentID: seg, Event: "working", Excerpt: "no, use pnpm instead of npm here", Status: store.ObservationComplete,
			Sender: store.SenderYou, Counts: true, At: testNow}); err != nil {
			return err
		}
		return store.EndSession(ctx, tx, key, testNow)
	})
}

func (f *fixture) setCLI(t *testing.T, cli store.ExtractorCLI) {
	t.Helper()
	f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) error { return store.SetExtractorCLI(ctx, tx, cli, "", testNow) })
}

// AC: brain extract off for a workspace records its switch by the Brain key and leaves
// learning on. Status tells extraction off apart from capture (learning) off.
func TestExtractSwitch(t *testing.T) {
	f := newStatusFixture(t)
	f.setCLI(t, store.ExtractorClaude)
	out, err := f.extract(t, "off", "--workspace", "w14")
	if err != nil || !strings.Contains(out, "extraction is off for workspace w14 (shop, key ws_a14)") {
		t.Fatalf("extract off = %q, %v", out, err)
	}
	f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) error {
		ext, err := store.ExtractionOn(ctx, tx, "ws_a14")
		if err != nil {
			return err
		}
		learn, err := store.LearningOn(ctx, tx, true, "default", "ws_a14", "w14:p1")
		if ext || !learn || err != nil {
			t.Errorf("extraction %t, learning %t, %v; want off and on", ext, learn, err)
		}
		return nil
	})
	status := f.mustStatus(t)
	for _, want := range []string{"extraction: on; off for workspace ws_a14; capture and rules keep running\n",
		"  extractor: claude, its default model\n", "\nlearning: on\n"} {
		if !strings.Contains(status, want) {
			t.Errorf("brain status:\n%s\nwant %q", status, want)
		}
	}

	f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) error {
		return store.SetLearning(ctx, tx, store.LearningSetting{Scope: store.LearningWorkspace, Target: "ws_a14", Changed: testNow})
	})
	if out, err := f.extract(t, "on", "--workspace", "w14"); err != nil || !strings.Contains(out, "extraction is on") {
		t.Fatalf("extract on = %q, %v", out, err)
	}
	status = f.mustStatus(t)
	for _, want := range []string{"extraction: on\n", "learning: on; off for workspace ws_a14\n"} {
		if !strings.Contains(status, want) {
			t.Errorf("brain status:\n%s\nwant %q", status, want)
		}
	}

	if out, err := f.extract(t, "off"); err != nil || !strings.Contains(out, "for the whole store") {
		t.Fatalf("extract off = %q, %v", out, err)
	}
	if out, err := f.extract(t, "on", "--workspace", "w14"); err != nil || !strings.Contains(out, "stays off") {
		t.Errorf("extract on under a store off = %q, %v; want stays off", out, err)
	}
	f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) error {
		s, err := store.Extractor(ctx, tx)
		if s.On || s.CLI != store.ExtractorClaude {
			t.Errorf("store row = %+v, want off with the CLI kept", s)
		}
		return err
	})
	if status := f.mustStatus(t); !strings.Contains(status, "extraction: off for the whole store; capture and rules keep running\n") {
		t.Errorf("brain status:\n%s\nwant the store off", status)
	}
}

func TestExtractArgs(t *testing.T) {
	f := newFixture(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, "want on or off"},
		{[]string{"maybe"}, "want on or off"},
		{[]string{"off", "--workspace", ""}, "is empty"},
		{[]string{"off", "--pane", "w14:p1"}, "flag provided but not defined"},
		{[]string{"off", "extra"}, "unexpected argument"},
		{[]string{"off", "--workspace", "w99"}, "nothing was written"},
	} {
		if _, err := f.extract(t, tc.args...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("extract %q = %v, want %q", tc.args, err, tc.want)
		}
	}
	if out, err := f.extract(t, "on"); err != nil || !strings.Contains(out, "run brain setup") {
		t.Errorf("extract on with no CLI = %q, %v; want the brain setup hint", out, err)
	}
}

// AC: an M1 store upgraded to M2 has no chosen CLI. Its signals wait, and status and doctor
// say to run brain setup.
func TestNoCLIWaits(t *testing.T) {
	f := newDoctorFixture(t)
	f.signalSession(t, "ws_a14", "a1")
	status := f.mustStatus(t)
	for _, want := range []string{"extraction: no extractor CLI is chosen, so Brain makes no extractor call; run brain setup\n",
		"  waiting in workspace ws_a14: 1 batches, the oldest 3h old; no extractor CLI is chosen; run brain setup\n"} {
		if !strings.Contains(status, want) {
			t.Errorf("brain status:\n%s\nwant %q", status, want)
		}
	}
	f.failed(t, "", "extraction: signals wait, and no extractor CLI is chosen; fix: brain setup")
}

// failingCLI is an extractor CLI that fails, as a signed-out CLI does.
type failingCLI struct{ calls int }

func (c *failingCLI) Run(context.Context, extract.Command) ([]byte, error) {
	c.calls++
	return nil, errors.New("claude: exit status 1")
}

// AC S15: a signed-out extractor CLI fails. The signals wait, and status and doctor say why
// from the stored reason.
func TestSignedOutExtractor(t *testing.T) {
	f := newDoctorFixture(t)
	f.setCLI(t, store.ExtractorClaude)
	f.signalSession(t, "ws_a14", "a1")
	s := openStore(t, f.store)
	cli := &failingCLI{}
	p := extract.Pass{Store: s, Runner: cli, HerdrSession: "default", DefaultOn: true,
		Now: func() time.Time { return testNow.Add(2 * time.Hour) }}
	if err := p.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	closeStore(t, s)
	if cli.calls != 1 {
		t.Fatalf("calls = %d, want 1", cli.calls)
	}
	want := "  waiting in workspace ws_a14: 1 batches, the oldest 3h old; the last claude call failed: claude: exit status 1\n"
	if status := f.mustStatus(t); !strings.Contains(status, want) {
		t.Errorf("brain status:\n%s\nwant %q", status, want)
	}
	f.failed(t, "", "extraction: the last extractor call for workspace ws_a14 failed (claude: exit status 1), so its signals wait; "+
		"fix: run claude in a terminal and check that it works and is signed in")
}

// The review queue line of status counts only what the default brain review lists: a
// proposal of a candidate below its gate is not in it (E12-S3).
func TestStatusQueueBelowGate(t *testing.T) {
	f := newStatusFixture(t)
	claim := f.claim(t)
	c := f.correct(t, claim, "ws_a14", "a1")
	if _, err := f.propose(t, c, "workspace w14 (shop)"); err != nil {
		t.Fatal(err)
	}
	if status := f.mustStatus(t); !strings.Contains(status, "review queue: empty\n") {
		t.Errorf("brain status:\n%s\nwant an empty queue below the gate", status)
	}
	f.correct(t, claim, "ws_a14", "a2")
	if status := f.mustStatus(t); !strings.Contains(status, "review queue: 1 pending, the oldest 3h old\n") {
		t.Errorf("brain status:\n%s\nwant 1 pending at the gate", status)
	}
}
