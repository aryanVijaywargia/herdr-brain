package launch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/aryanVijaywargia/herdr-brain/internal/paths"
	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// StartFile is the file of a launch folder that holds the receipt of the brief that the launch
// gave with a flag (E24-S4), as store.Delivery JSON. The start hook of the launch writes the
// receipt from it, and forget reads which items the brief holds from it.
const StartFile = "start.json"

// ForgetBriefs removes the Brain brief from the brief.md of each launch folder in the store
// folder dir that holds the text of item (forget, E24-S4). It keeps the part of the user's
// own --append-system-prompt-file. Brain rewrites the file and does not delete it: the E24-S1
// spike did not check whether Claude reads the file again after its start, and with the
// snapshot off Claude builds the system prompt again on each request, so a running Claude
// may read it again. The text that a running Claude already has stays there.
func ForgetBriefs(dir string, item int64) error {
	return eachBrief(dir, item, cutBrief)
}

// BriefsHold reports whether the brief.md of a launch folder in the store folder dir still
// holds the Brain brief with the text of item: forget stays pending until then.
func BriefsHold(dir string, item int64) (bool, error) {
	held := false
	err := eachBrief(dir, item, func(string, []byte, int) error {
		held = true
		return nil
	})
	return held, err
}

// eachBrief calls fn for each launch folder in the store folder dir whose brief.md holds the
// Brain brief with item, with the file bytes b and the start at of the Brain brief in them.
// A brief counts by the (item, revision) pairs of its receipt, never by a text search.
func eachBrief(dir string, item int64, fn func(folder string, b []byte, at int) error) error {
	es, err := os.ReadDir(paths.Launches(dir))
	// No launches folder: no launch of this store gave a brief yet.
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range es {
		folder := filepath.Join(paths.Launches(dir), e.Name())
		d, b, ok, err := keptBrief(folder)
		if err != nil {
			return err
		}
		if !ok || !slices.ContainsFunc(d.ItemRevisions, func(r store.RevisionRef) bool { return r.Item == item }) {
			continue
		}
		if at, ok := brainPart(b, *d.BriefHash); ok {
			if err := fn(folder, b, at); err != nil {
				return err
			}
		}
	}
	return nil
}

// keptBrief reads the brief receipt and brief.md of the launch folder folder. ok is false
// when the folder has no brief: a Codex launch, a withheld brief, or a launch that ended while
// the read ran.
func keptBrief(folder string) (d store.Delivery, b []byte, ok bool, err error) {
	data, err := os.ReadFile(filepath.Join(folder, StartFile))
	if err == nil {
		b, err = os.ReadFile(filepath.Join(folder, briefFile))
	}
	if errors.Is(err, fs.ErrNotExist) {
		return d, nil, false, nil
	}
	if err != nil {
		return d, nil, false, err
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return d, nil, false, fmt.Errorf("read %s: %w", filepath.Join(folder, StartFile), err)
	}
	return d, b, d.BriefHash != nil, nil
}

// brainPart returns the start of the Brain brief in the brief.md bytes b: the brief whose
// SHA-256 is hash is all of b, or the part after a blank line that ends the user's own text,
// as buildClaude writes it. ok is false when b holds no such part: it was cut already, or it
// is still being written.
func brainPart(b []byte, hash string) (at int, ok bool) {
	for at := 0; at < len(b); {
		sum := sha256.Sum256(b[at:])
		if hex.EncodeToString(sum[:]) == hash {
			return at, true
		}
		i := bytes.Index(b[at+1:], []byte("\n\n"))
		if i < 0 {
			return 0, false
		}
		at += i + 3
	}
	return 0, false
}

// cutBrief rewrites the brief.md of the launch folder folder, whose bytes b hold the Brain
// brief from at, with only the user's text before it. The rename replaces the file in one
// step, so a reader sees the old file or the new one.
func cutBrief(folder string, b []byte, at int) error {
	f, err := os.CreateTemp(folder, ".brief-*") // 0600, as the other launch files
	if err != nil {
		return err
	}
	_, err = f.Write(bytes.TrimSuffix(b[:at], []byte("\n\n")))
	if err = errors.Join(err, f.Close()); err != nil {
		return errors.Join(err, os.Remove(f.Name()))
	}
	return os.Rename(f.Name(), filepath.Join(folder, briefFile))
}

// cutForgotten removes the Brain brief from the brief.md of the launch folder folder in the
// store folder dir when an item of it is forgotten. A forget can run between the compile of
// the brief and the write of brief.md: its sweep then finds no file, so the launch checks
// after its write. A forget erases the text of each revision of the item, and a revision
// holds text otherwise.
func cutForgotten(ctx context.Context, dir, folder string) (err error) {
	d, b, ok, err := keptBrief(folder)
	if err != nil || !ok {
		return err
	}
	st, err := store.Open(ctx, dir, busyTimeout)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, st.Close()) }()
	forgotten := false
	err = st.Write(ctx, func(tx *sql.Tx) error {
		for _, r := range d.ItemRevisions {
			rev, err := store.ItemRevisionByKey(ctx, tx, r.Item, r.Revision)
			if err != nil {
				return err
			}
			forgotten = forgotten || rev.Text == ""
		}
		return nil
	})
	if err != nil || !forgotten {
		return err
	}
	if at, ok := brainPart(b, *d.BriefHash); ok {
		return cutBrief(folder, b, at)
	}
	return nil
}
