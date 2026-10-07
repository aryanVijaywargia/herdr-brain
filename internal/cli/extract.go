package cli

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

const extractUsage = `usage: brain extract on|off [--workspace <workspace id>]`

// Extract runs `brain extract` with args after "extract" (E13-S4, §7.3 D9). It records the
// extraction switch of the whole store, or of one workspace by its Brain key. It is apart from
// brain learn: with extraction off, capture and the signal rules keep running, and Brain makes
// no extractor call there.
func Extract(ctx context.Context, e Env, args []string) (err error) {
	on, ws, err := parseExtract(args)
	if err != nil {
		return err
	}
	// The switch touches no output, so the call does not repair it either.
	c, err := e.open(ctx, openNoRepair)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, c.Close()) }()
	key, where := "", "the whole store"
	if ws != "" {
		m, err := workspaceKey(c, ws)
		if err != nil {
			return err
		}
		key, where = m.Key, fmt.Sprintf("workspace %s (%s, key %s)", ws, m.Label, m.Key)
	}
	var now bool
	var s store.ExtractorSetting
	err = c.Store.Write(ctx, func(tx *sql.Tx) error {
		if err := store.SetExtraction(ctx, tx, key, on, e.Now()); err != nil {
			return err
		}
		if now, err = store.ExtractionOn(ctx, tx, key); err != nil {
			return err
		}
		s, err = store.Extractor(ctx, tx)
		return err
	})
	if err != nil {
		return err
	}
	msg := fmt.Sprintf("extraction is %s for %s. Capture and the signal rules keep running.\n", onOffWord(on), where)
	switch {
	case on && !now:
		msg += "Extraction stays off there: the whole store is off (brain extract on turns it on).\n"
	case on && s.CLI == "":
		msg += "No extractor CLI is chosen, so Brain makes no call yet: run brain setup.\n"
	}
	_, err = io.WriteString(e.Stdout, msg)
	return err
}

// parseExtract parses the arguments of brain extract: on or off, and an optional workspace.
func parseExtract(args []string) (on bool, ws string, err error) {
	if len(args) == 0 || args[0] != "on" && args[0] != "off" {
		return false, "", fmt.Errorf("want on or off\n%s", extractUsage)
	}
	fs := flag.NewFlagSet("extract", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Func("workspace", "", func(v string) error {
		// An empty value, such as --workspace "$w" with no $w, must not switch the whole store.
		if v == "" {
			return errors.New("is empty")
		}
		ws = v
		return nil
	})
	if err := fs.Parse(args[1:]); err != nil {
		return false, "", fmt.Errorf("%w\n%s", err, extractUsage)
	}
	if fs.NArg() != 0 {
		return false, "", fmt.Errorf("unexpected argument %q\n%s", fs.Arg(0), extractUsage)
	}
	return args[0] == "on", ws, nil
}
