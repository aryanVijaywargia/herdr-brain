package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

// A rename updates the label and the last seen time, and keeps the key.
func TestSeeWorkspace(t *testing.T) {
	s := openM1(t)
	f := addFixture(t, s)
	later := testNow.Add(time.Hour)
	var got Workspace
	write(t, s, func(tx *sql.Tx) error {
		if err := SeeWorkspace(context.Background(), tx, f.ws.Key, "renamed", later); err != nil {
			return err
		}
		var err error
		got, err = WorkspaceByKey(context.Background(), tx, f.ws.Key)
		return err
	})
	if got.Label != "renamed" || !got.LastSeen.Equal(later) || got.HerdrWorkspaceID != f.ws.HerdrWorkspaceID {
		t.Errorf("after SeeWorkspace: %+v, want label %q, last seen %v, id %q", got, "renamed", later, f.ws.HerdrWorkspaceID)
	}
}

func TestSeeWorkspaceUnknownKey(t *testing.T) {
	s := openM1(t)
	err := s.Write(context.Background(), func(tx *sql.Tx) error {
		return SeeWorkspace(context.Background(), tx, "no such key", "x", testNow)
	})
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("SeeWorkspace(unknown key) = %v, want sql.ErrNoRows", err)
	}
}

// A close sets the status to closed and keeps the rest of the row.
func TestCloseWorkspace(t *testing.T) {
	s := openM1(t)
	f := addFixture(t, s)
	var got Workspace
	write(t, s, func(tx *sql.Tx) error {
		if err := CloseWorkspace(context.Background(), tx, f.ws.Key); err != nil {
			return err
		}
		var err error
		got, err = WorkspaceByKey(context.Background(), tx, f.ws.Key)
		return err
	})
	if got.Status != WorkspaceClosed || got.Label != f.ws.Label {
		t.Errorf("after CloseWorkspace: %+v, want status %q and label %q", got, WorkspaceClosed, f.ws.Label)
	}
}

func TestCloseWorkspaceUnknownKey(t *testing.T) {
	s := openM1(t)
	err := s.Write(context.Background(), func(tx *sql.Tx) error {
		return CloseWorkspace(context.Background(), tx, "no such key")
	})
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("CloseWorkspace(unknown key) = %v, want sql.ErrNoRows", err)
	}
}
