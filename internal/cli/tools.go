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

// listTools prints each approved tool that the caller sees. The output form:
//
//	<name> (item <id>, <scope>): <description>
//	  brain run <name> --<input> <value> ...
//	  --<input>: <type>, pattern <pattern>
func (cl caller) listTools(ctx context.Context) error {
	ts, err := cl.approvedTools(ctx, nil)
	if err != nil {
		return err
	}
	if len(ts) == 0 {
		_, err = fmt.Fprintln(cl.Stdout, "no approved tools")
		return err
	}
	var b strings.Builder
	for _, t := range ts {
		writeTool(&b, t.Tool, t.contract, cl.target(t.Scope))
	}
	_, err = fmt.Fprint(cl.Stdout, b.String())
	return err
}

// approvedTool is an approved tool and its checked contract.
type approvedTool struct {
	store.Tool
	contract tool.Contract
}

// approvedTools returns the approved tools that the caller sees, in approval order. With
// words, it returns only the tools whose name or description match one of them, by the match
// rule of brain recall. A draft is never in it.
func (cl caller) approvedTools(ctx context.Context, words []string) ([]approvedTool, error) {
	var out []approvedTool
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
			if len(words) == 0 || matchScore(c.Name+" "+c.Description, words) > 0 {
				out = append(out, approvedTool{t, c})
			}
		}
		return nil
	})
	return out, err
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
