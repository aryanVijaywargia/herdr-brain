package store

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// TestLearningOn: the read helper of the learning switch (E9-S4). Any off setting that applies
// wins over a narrower on setting; no store row is the default of the store kind.
func TestLearningOn(t *testing.T) {
	at := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	storeOff := LearningSetting{Scope: LearningStore, On: false, Changed: at}
	storeOn := LearningSetting{Scope: LearningStore, On: true, Changed: at}
	wsOff := LearningSetting{Scope: LearningWorkspace, Target: "ws_a", On: false, Changed: at}
	paneOn := LearningSetting{Scope: LearningPane, HerdrSession: "default", Target: "w1:p1", On: true, Changed: at}
	paneOff := LearningSetting{Scope: LearningPane, HerdrSession: "default", Target: "w1:p1", On: false, Changed: at}
	type probe struct {
		session, key, pane string
		want               bool
	}
	tests := []struct {
		name      string
		defaultOn bool
		settings  []LearningSetting
		probes    []probe
	}{
		{name: "main store default", defaultOn: true,
			probes: []probe{{"default", "ws_a", "w1:p1", true}}},
		{name: "test store default", defaultOn: false,
			probes: []probe{{"eval", "ws_a", "w1:p1", false}}},
		{name: "test store turned on", defaultOn: false, settings: []LearningSetting{storeOn},
			probes: []probe{{"eval", "ws_a", "w1:p1", true}}},
		// AC: store off plus pane on is off for that pane.
		{name: "store off, pane on", defaultOn: true, settings: []LearningSetting{storeOff, paneOn},
			probes: []probe{{"default", "ws_a", "w1:p1", false}}},
		{name: "workspace off, pane on", defaultOn: true, settings: []LearningSetting{wsOff, paneOn},
			probes: []probe{{"default", "ws_a", "w1:p1", false}, {"default", "ws_b", "w2:p1", true}, {"default", "", "w2:p1", true}}},
		// Two herdr sessions can both have w1:p1: a pane switch is for one binding only.
		{name: "pane off", defaultOn: true, settings: []LearningSetting{paneOff},
			probes: []probe{{"default", "ws_a", "w1:p1", false}, {"other", "ws_a", "w1:p1", true}, {"default", "ws_a", "w1:p2", true}}},
		// A later switch of the same target replaces the earlier one.
		{name: "pane off, then on", defaultOn: true, settings: []LearningSetting{paneOff, paneOn},
			probes: []probe{{"default", "ws_a", "w1:p1", true}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, s := newTestStore(t)
			ctx := context.Background()
			err := s.Write(ctx, func(tx *sql.Tx) error {
				for _, set := range tt.settings {
					if err := SetLearning(ctx, tx, set); err != nil {
						return err
					}
				}
				for _, p := range tt.probes {
					got, err := LearningOn(ctx, tx, tt.defaultOn, p.session, p.key, p.pane)
					if err != nil {
						return err
					}
					if got != p.want {
						t.Errorf("LearningOn(%s, %q, %s) = %v, want %v", p.session, p.key, p.pane, got, p.want)
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLearningSettings(t *testing.T) {
	_, s := newTestStore(t)
	ctx := context.Background()
	at := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	in := []LearningSetting{
		{Scope: LearningPane, HerdrSession: "default", Target: "w1:p1", On: false, Changed: at},
		{Scope: LearningWorkspace, Target: "ws_a", On: false, Changed: at.Add(time.Second)},
		{Scope: LearningStore, On: true, Changed: at.Add(2 * time.Second)},
	}
	err := s.Write(ctx, func(tx *sql.Tx) error {
		for _, set := range in {
			if err := SetLearning(ctx, tx, set); err != nil {
				return err
			}
		}
		got, err := LearningSettings(ctx, tx)
		if err != nil {
			return err
		}
		want := []LearningSetting{in[2], in[1], in[0]}
		if len(got) != len(want) {
			t.Fatalf("LearningSettings() = %v, want %v", got, want)
		}
		for i := range want {
			if !sameSetting(got[i], want[i]) || !got[i].Changed.Equal(want[i].Changed) {
				t.Errorf("LearningSettings()[%d] = %+v, want %+v", i, got[i], want[i])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// E11-S2: an off and on within one millisecond, or with a clock that went back, still gives
// a later LearningChanged, so a reader sees the off time.
func TestSetLearningOrder(t *testing.T) {
	_, s := newTestStore(t)
	ctx := context.Background()
	at := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	err := s.Write(ctx, func(tx *sql.Tx) error {
		var last time.Time
		for _, set := range []LearningSetting{
			{Scope: LearningStore, On: true, Changed: at},
			{Scope: LearningPane, HerdrSession: "default", Target: "w1:p1", On: false, Changed: at.Add(300 * time.Microsecond)},
			{Scope: LearningPane, HerdrSession: "default", Target: "w1:p1", On: true, Changed: at.Add(-time.Second)},
		} {
			if err := SetLearning(ctx, tx, set); err != nil {
				return err
			}
			got, err := LearningChanged(ctx, tx, "default", "", "w1:p1")
			if err != nil {
				return err
			}
			if !got.After(last) {
				t.Errorf("LearningChanged after %+v = %v, want later than %v", set, got, last)
			}
			last = got
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func sameSetting(a, b LearningSetting) bool {
	return a.Scope == b.Scope && a.HerdrSession == b.HerdrSession && a.Target == b.Target && a.On == b.On
}

// TestLearningChanged: the latest change of the settings that apply to one pane (E11-S2). A
// setting of another workspace, pane or herdr session does not count.
func TestLearningChanged(t *testing.T) {
	t1 := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Minute)
	t3 := t2.Add(time.Minute)
	tests := []struct {
		name     string
		settings []LearningSetting
		key      string
		want     time.Time
	}{
		{name: "none", key: "ws_a"},
		{name: "store", settings: []LearningSetting{{Scope: LearningStore, On: true, Changed: t1}}, key: "ws_a", want: t1},
		{name: "workspace is newer", key: "ws_a", want: t2, settings: []LearningSetting{
			{Scope: LearningStore, On: true, Changed: t1},
			{Scope: LearningWorkspace, Target: "ws_a", On: true, Changed: t2}}},
		{name: "unmapped pane skips workspaces", key: "", want: t1, settings: []LearningSetting{
			{Scope: LearningStore, On: true, Changed: t1},
			{Scope: LearningWorkspace, Target: "ws_a", On: true, Changed: t2}}},
		{name: "pane is newest", key: "ws_a", want: t3, settings: []LearningSetting{
			{Scope: LearningWorkspace, Target: "ws_a", On: false, Changed: t2},
			{Scope: LearningPane, HerdrSession: "default", Target: "w1:p1", On: true, Changed: t3}}},
		{name: "other targets", key: "ws_a", settings: []LearningSetting{
			{Scope: LearningWorkspace, Target: "ws_b", On: false, Changed: t2},
			{Scope: LearningPane, HerdrSession: "other", Target: "w1:p1", On: false, Changed: t2},
			{Scope: LearningPane, HerdrSession: "default", Target: "w1:p2", On: false, Changed: t3}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, s := newTestStore(t)
			ctx := context.Background()
			err := s.Write(ctx, func(tx *sql.Tx) error {
				for _, set := range tt.settings {
					if err := SetLearning(ctx, tx, set); err != nil {
						return err
					}
				}
				got, err := LearningChanged(ctx, tx, "default", tt.key, "w1:p1")
				if err != nil {
					return err
				}
				if !got.Equal(tt.want) {
					t.Errorf("LearningChanged(default, %q, w1:p1) = %v, want %v", tt.key, got, tt.want)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
