package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
	"github.com/aryanVijaywargia/herdr-brain/internal/tool"
)

const toolsUsage = `usage: brain tools`

// Tools runs `brain tools` with args after "tools" (E17-S5). It lists all approved
// tools that the caller can run, the global ones and those of its workspace, in approval
// order, with their input schemas. The brief catalog lists only the first 10.
func Tools(ctx context.Context, e Env, args []string) (err error) {
	if len(args) != 0 {
		return fmt.Errorf("unexpected argument %q\n%s", args[0], toolsUsage)
	}
	c, err := e.Open(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, c.Close()) }()
	return newCaller(e, c).listTools(ctx)
}

// listTools prints each approved tool that the caller sees. [NEW] the output form:
//
//	<name> (item <id>, <scope>): <description>
//	  brain run <name> --<input> <value> ...
//	  --<input>: <type>, pattern <pattern>
func (cl caller) listTools(ctx context.Context) error {
	var b strings.Builder
	err := cl.Store.Write(ctx, func(tx *sql.Tx) error {
		ts, err := store.Tools(ctx, tx)
		if err != nil {
			return err
		}
		for _, t := range ts {
			if !t.Ready() || !cl.sees(t.WorkspaceKey) {
				continue
			}
			c, err := tool.Parse(t.Contract)
			if err != nil {
				return fmt.Errorf("tool item %d: %w", t.ID, err)
			}
			writeTool(&b, t, c, cl.target(t.Scope))
		}
		return nil
	})
	if err != nil {
		return err
	}
	if b.Len() == 0 {
		_, err = fmt.Fprintln(cl.Stdout, "no approved tools")
		return err
	}
	_, err = fmt.Fprint(cl.Stdout, b.String())
	return err
}

// writeTool writes the entry of tool t, with contract c, in scope target.
func writeTool(b *strings.Builder, t store.Tool, c tool.Contract, target string) {
	names := slices.Sorted(maps.Keys(c.Inputs))
	fmt.Fprintf(b, "%s (item %d, %s): %s\n  brain run %s", t.Name, t.ID, target,
		strings.Join(strings.Fields(c.Description), " "), t.Name)
	for _, n := range names {
		fmt.Fprintf(b, " --%s <value>", n)
	}
	b.WriteString("\n")
	for _, n := range names {
		fmt.Fprintf(b, "  --%s: %s, pattern %s\n", n, c.Inputs[n].Type, c.Inputs[n].Pattern)
	}
}
