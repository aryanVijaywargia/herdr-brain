package cli

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"path/filepath"
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

// setClaude chooses claude as the extractor CLI.
func (f *fixture) setClaude(t *testing.T) {
	t.Helper()
	f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) error {
		return store.SetExtractorCLI(ctx, tx, store.ExtractorClaude, "", testNow)
	})
}

// AC: brain extract off for a workspace records its switch by the Brain key and leaves
// learning on. Status tells extraction off apart from capture (learning) off.
func TestExtractSwitch(t *testing.T) {
	f := newStatusFixture(t)
	f.setClaude(t)
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
		"  waiting in workspace ws_a14: 1 batch, the oldest 3h old; no extractor CLI is chosen; run brain setup\n"} {
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
	f.setClaude(t)
	f.signalSession(t, "ws_a14", "a1")
	s := openStore(t, f.store)
	cli := &failingCLI{}
	p := extract.Pass{Store: s, Runner: cli, HerdrSession: "default", DefaultOn: true,
		Now: func() time.Time { return testNow.Add(150 * time.Minute) }}
	if err := p.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	closeStore(t, s)
	if cli.calls != 1 {
		t.Fatalf("calls = %d, want 1", cli.calls)
	}
	want := "  waiting in workspace ws_a14: 1 batch, the oldest 3h old; the last claude call failed: claude: exit status 1\n"
	if status := f.mustStatus(t); !strings.Contains(status, want) {
		t.Errorf("brain status:\n%s\nwant %q", status, want)
	}
	f.failed(t, "", "extraction: the last extractor call for workspace ws_a14 failed (claude: exit status 1), so its signals wait; "+
		"fix: run claude in a terminal and check that it works and is signed in")
}

// noClaimsCLI is an extractor CLI that works and finds no claim.
type noClaimsCLI struct{}

func (noClaimsCLI) Run(context.Context, extract.Command) ([]byte, error) {
	return []byte(`{"type":"result","is_error":false,"structured_output":{"claims":[]}}`), nil
}

// AC #263: after a failed call, a later call that works: status and doctor no longer say that
// the last call failed. The failed batch stays in the failed batches, with its time.
func TestExtractorWorksAfterFailure(t *testing.T) {
	f := newDoctorFixture(t)
	f.setClaude(t)
	f.signalSession(t, "ws_a14", "a1")
	s := openStore(t, f.store)
	run := func(r extract.Runner, at time.Time) {
		t.Helper()
		p := extract.Pass{Store: s, Runner: r, HerdrSession: "default", DefaultOn: true, Now: func() time.Time { return at }}
		if err := p.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	run(&failingCLI{}, testNow.Add(150*time.Minute))
	closeStore(t, s)
	f.signalSession(t, "ws_b14", "b1")
	s = openStore(t, f.store)
	run(noClaimsCLI{}, testNow.Add(151*time.Minute))
	closeStore(t, s)
	status := f.mustStatus(t)
	for _, want := range []string{
		"  waiting in workspace ws_a14: 1 batch, the oldest 3h old; the next pass takes them\n",
		"  failed batches: 1; their evidence waits for a retry, or with no try left for new evidence of its session\n",
		"workspace ws_a14: failed, claude: exit status 1, 30m ago\n",
	} {
		if !strings.Contains(status, want) {
			t.Errorf("brain status:\n%s\nwant %q", status, want)
		}
	}
	if strings.Contains(status, "call failed") {
		t.Errorf("brain status:\n%s\nwant no failed last call", status)
	}
	if out, err := f.doctor(t, f.sock, ""); err != nil || out != "all checks passed\n" {
		t.Errorf("Doctor() = %v\n%s\nwant all checks passed: no failed last call", err, out)
	}
}

// AC #282: the gates and caps come from the config file. Status shows each value that is not
// the default, and the caps line uses it. A bad value is a failed doctor check and a status line.
func TestExtractionConfig(t *testing.T) {
	f := newDoctorFixture(t)
	f.setClaude(t)
	config := filepath.Join(filepath.Dir(f.store), "config.toml")
	putFile(t, config, "live_suggestions = \"on\"\n[gates]\ncorrection = 3\n[caps]\nproposals_per_day = 8\n")
	status := f.mustStatus(t)
	for _, want := range []string{
		"  config: gates.correction = 3 (default 2), caps.proposals_per_day = 8 (default 15)\n",
		"  last 24h: 0 of 40 extractor calls, 0 of 8 new proposals\n",
	} {
		if !strings.Contains(status, want) {
			t.Errorf("brain status:\n%s\nwant %q", status, want)
		}
	}
	putFile(t, config, "[caps]\ncalls_per_day = 0\n")
	want := "caps.calls_per_day = 0, want 1 or more"
	if status := f.mustStatus(t); !strings.Contains(status, want+"; Brain makes no extractor call until you fix the file\n") {
		t.Errorf("brain status:\n%s\nwant %q", status, want)
	}
	f.failed(t, "", want+", so Brain makes no extractor call")
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

// AC #282: the review queue line of status uses the gates of the config: with
// gates.correction = 1, a proposal of one correction is in the queue.
func TestStatusQueueConfigGate(t *testing.T) {
	f := newStatusFixture(t)
	putFile(t, filepath.Join(filepath.Dir(f.store), "config.toml"), "[gates]\ncorrection = 1\n")
	c := f.correct(t, f.claim(t), "ws_a14", "a1")
	if _, err := f.propose(t, c, "workspace w14 (shop)"); err != nil {
		t.Fatal(err)
	}
	if status := f.mustStatus(t); !strings.Contains(status, "review queue: 1 pending, the oldest 3h old\n") {
		t.Errorf("brain status:\n%s\nwant 1 pending at the configured gate", status)
	}
}

// AC #372: when a daily cap is full, brain review (the list in the queue pane of the review
// action), the review pane and the brain status cap line say that learning waits, which cap is
// full, and the local time when room comes back: when the call or the proposal that keeps the
// cap full is a day old. With room left, none of them has the line.
func TestCapFullLines(t *testing.T) {
	inZone(t)
	// [NEW] test limits: caps of 1 call and 1 proposal, so one of each fills them.
	for _, tc := range []struct {
		name, config, line string
		call               bool // add an extractor call 3 hours before testNow
	}{
		{"room left", "", "", true},
		// The call of 09:00 UTC is a day old at 09:00 UTC, 14:30 in +05:30.
		{"call cap", "[caps]\ncalls_per_day = 1\n",
			"learning paused: the daily cap of 1 extractor calls is reached; room comes back at 14:30", true},
		// The proposal of 12:00 UTC is a day old at 12:00 UTC, 17:30 in +05:30.
		{"proposal cap", "[caps]\nproposals_per_day = 1\n",
			"learning paused: the daily cap of 1 new proposals is reached; room comes back at 17:30", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newStatusFixture(t)
			putFile(t, filepath.Join(filepath.Dir(f.store), "config.toml"), tc.config)
			pid, err := f.propose(t, f.correct(t, f.claim(t), "ws_a14", "a1"), "workspace w14 (shop)")
			if err != nil {
				t.Fatal(err)
			}
			if tc.call {
				f.storeWrite(t, func(ctx context.Context, tx *sql.Tx) error {
					_, err := tx.ExecContext(ctx, `INSERT INTO extraction_calls (workspace_key, at) VALUES ('ws_a14', ?)`,
						testNow.Add(-3*time.Hour).Format(time.RFC3339Nano))
					return err
				})
			}
			list, err := f.review(t)
			if err != nil {
				t.Fatal(err)
			}
			surfaces := map[string]string{"brain review": list, "review pane": f.paneScreen(t, pid, "q", 0, 0),
				"brain status": f.mustStatus(t)}
			for name, out := range surfaces {
				if tc.line == "" {
					if strings.Contains(out, "learning paused") {
						t.Errorf("%s:\n%s\nwant no pause line", name, out)
					}
					continue
				}
				if !strings.Contains(out, tc.line+"\n") {
					t.Errorf("%s:\n%s\nwant %q", name, out, tc.line)
				}
			}
			if tc.line != "" && !strings.Contains(surfaces["brain status"], " new proposals; "+tc.line+"\n") {
				t.Errorf("brain status:\n%s\nwant the line on the last 24h cap line", surfaces["brain status"])
			}
		})
	}
}
