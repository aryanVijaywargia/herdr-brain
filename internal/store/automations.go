package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/aryanVijaywargia/herdr-brain/internal/tool"
)

// Binding is the text of an automation item (§10.4): one approved tool, pinned to its contract
// hash, on the stop trigger of the CLI of the bound agent, in one pinned folder. The workspace
// key is the item's own. [NEW] the text form: TOML with these keys.
type Binding struct {
	Tool         string `toml:"tool"`    // the tool name, for you to read
	ToolID       int64  `toml:"tool_id"` // the tools row
	ContractHash string `toml:"contract_hash"`
	// Folder is the pinned folder: a clean absolute path, or one with a leading ~ for home, as a
	// tool cwd names it.
	Folder  string `toml:"folder"`
	CLI     string `toml:"cli"` // the CLI kind of the bound agent
	Trigger string `toml:"trigger"`
}

const stopTrigger = "working → idle or working → done"

// StopTrigger returns the tested "agent stopped working" condition of CLI kind cli, and false
// when it has none (§10.4, M0 E17-S1 "Stop condition per CLI"). Focus decides which of the two
// changes comes. A CLI with no tested condition (agy now) cannot get a binding.
func StopTrigger(cli string) (string, bool) {
	switch cli {
	case "claude", "codex":
		return stopTrigger, true
	}
	return "", false
}

// ParseBinding reads the binding text of an automation item.
func ParseBinding(text string) (Binding, error) {
	var b Binding
	md, err := toml.Decode(text, &b)
	if err != nil {
		return Binding{}, fmt.Errorf("automation binding: %w", err)
	}
	if un := md.Undecoded(); len(un) > 0 {
		return Binding{}, fmt.Errorf("automation binding: unknown key %s", un[0])
	}
	if b.Tool == "" || b.ToolID <= 0 || b.ContractHash == "" || b.Folder == "" || b.CLI == "" || b.Trigger == "" {
		return Binding{}, errors.New("automation binding: want tool, tool_id, contract_hash, folder, cli and trigger")
	}
	return b, nil
}

// Text is the binding text of b.
func (b Binding) Text() string {
	var s strings.Builder
	// The encoder fails only on a type it cannot encode; Binding has none.
	if err := toml.NewEncoder(&s).Encode(b); err != nil {
		panic(fmt.Sprintf("store: encode binding: %v", err))
	}
	return s.String()
}

// Benefit is the expected benefit of a proposal of b [NEW].
func (b Binding) Benefit() string {
	return fmt.Sprintf("runs tool %s in %s after each stop of a %s agent, with no prompt", b.Tool, b.Folder, b.CLI)
}

// ErrBindingRefused means that the approval refuses a binding (§10.4): its tool has a required
// input, the approved cwd of the tool is not the pinned folder, the tool is not an approved tool
// that the workspace can call at the pinned hash, or the CLI has no tested stop condition.
var ErrBindingRefused = errors.New("the binding is refused")

// CheckBinding checks binding b of the workspace with key workspaceKey before you approve it.
// home is the home folder that a leading ~ of the tool cwd names (tool.Contract.Dir).
func CheckBinding(ctx context.Context, tx *sql.Tx, b Binding, workspaceKey, home string) error {
	switch trigger, ok := StopTrigger(b.CLI); {
	case !ok:
		return fmt.Errorf("%s has no tested stop condition: %w", b.CLI, ErrBindingRefused)
	case b.Trigger != trigger:
		return fmt.Errorf("trigger %q is not the stop condition of %s (%s): %w", b.Trigger, b.CLI, trigger, ErrBindingRefused)
	}
	t, err := ToolByID(ctx, tx, b.ToolID)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("tool %d does not exist: %w", b.ToolID, ErrBindingRefused)
	}
	if err != nil {
		return err
	}
	if t.Scope == ScopeWorkspace && ptrString(t.WorkspaceKey) != workspaceKey {
		return fmt.Errorf("tool %s (item %d) is a tool of another workspace: %w", t.Name, t.ID, ErrBindingRefused)
	}
	c, err := tool.Parse(t.Contract)
	if err != nil {
		return fmt.Errorf("tool %s (item %d): %w", t.Name, t.ID, err)
	}
	if len(c.Inputs) > 0 {
		return fmt.Errorf("tool %s (item %d) has a required input: %w", t.Name, t.ID, ErrBindingRefused)
	}
	if dir := filepath.Clean(c.Dir(home)); dir != filepath.Clean(tool.Dir(b.Folder, home)) {
		return fmt.Errorf("the cwd of tool %s (item %d) is %s, not the pinned folder %s: %w", t.Name, t.ID, dir, b.Folder,
			ErrBindingRefused)
	}
	if t.ApprovedHash == nil || *t.ApprovedHash != b.ContractHash || t.ContractHash != b.ContractHash {
		return fmt.Errorf("tool %s (item %d) is not approved at the pinned contract hash; make a new proposal with brain automation add: %w",
			t.Name, t.ID, ErrBindingRefused)
	}
	return nil
}

// Automation is the automations row of an automation item (§11).
type Automation struct {
	ID           int64
	WorkspaceKey string
	Folder       string
	Trigger      string
	CLI          string
	ToolID       int64
	ContractHash string
	Approved     time.Time // zero while the revision is not active
	Enabled      bool      // on; false is paused
	LastResult   string    // "" before the first run (E17-S11)
	LastRun      time.Time
}

// syncAutomation writes the automations row of item, when it is an automation, from its
// current revision (§11). An active revision is your approval of its exact binding, so it
// records the approval time; a new row starts enabled, and an existing row keeps its pause.
// Any other state clears the approval, so the binding does not run. Any other item passes.
func syncAutomation(ctx context.Context, tx *sql.Tx, item int64) error {
	it, err := ItemByID(ctx, tx, item)
	if err != nil || it.Kind != KindAutomation {
		return err
	}
	if it.State != StateActive {
		if _, err := tx.ExecContext(ctx, `UPDATE automations SET approved = NULL WHERE id = ?`, item); err != nil {
			return fmt.Errorf("stop automation %d: %w", item, err)
		}
		return nil
	}
	cur, err := ItemRevisionByKey(ctx, tx, item, it.CurrentRevision)
	if err != nil {
		return err
	}
	b, err := ParseBinding(cur.Text)
	if err != nil {
		return fmt.Errorf("automation item %d: %w", item, err)
	}
	if cur.WorkspaceKey == nil {
		return fmt.Errorf("automation item %d: a binding is a workspace item: %w", item, ErrBindingRefused)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO automations (id, workspace_key, folder, trigger_condition, cli_kind,
		tool_id, contract_hash, approved, enabled) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1)
		ON CONFLICT (id) DO UPDATE SET workspace_key = excluded.workspace_key, folder = excluded.folder,
		trigger_condition = excluded.trigger_condition, cli_kind = excluded.cli_kind, tool_id = excluded.tool_id,
		contract_hash = excluded.contract_hash, approved = excluded.approved`,
		item, *cur.WorkspaceKey, b.Folder, b.Trigger, b.CLI, b.ToolID, b.ContractHash, timeArg(cur.Created))
	if err != nil {
		return fmt.Errorf("write automation %d: %w", item, err)
	}
	return nil
}

const automationColumns = `id, workspace_key, folder, trigger_condition, cli_kind, tool_id, contract_hash, approved,
	enabled, COALESCE(last_result, ''), last_run`

func scanAutomation(row interface{ Scan(...any) error }) (Automation, error) {
	var a Automation
	err := row.Scan(&a.ID, &a.WorkspaceKey, &a.Folder, &a.Trigger, &a.CLI, &a.ToolID, &a.ContractHash,
		timeDest{&a.Approved}, &a.Enabled, &a.LastResult, timeDest{&a.LastRun})
	return a, err
}

// AutomationByID reads the automations row of item id. It returns sql.ErrNoRows when there is
// none: the binding was never approved.
func AutomationByID(ctx context.Context, tx *sql.Tx, id int64) (Automation, error) {
	a, err := scanAutomation(tx.QueryRowContext(ctx, `SELECT `+automationColumns+` FROM automations WHERE id = ?`, id))
	if err != nil {
		return Automation{}, fmt.Errorf("read automation %d: %w", id, err)
	}
	return a, nil
}

// ActiveAutomations reads the automations row of each active automation item, in id order.
func ActiveAutomations(ctx context.Context, tx *sql.Tx) ([]Automation, error) {
	return query(ctx, tx, `SELECT `+automationColumns+` FROM automations
		WHERE id IN (SELECT id FROM items WHERE kind = ? AND state = ?) ORDER BY id`,
		func(rows *sql.Rows) (Automation, error) { return scanAutomation(rows) }, KindAutomation, StateActive)
}

// SetAutomationEnabled turns automation id on or pauses it. Pause and resume are one
// transaction each, so the launch mark of a run (E17-S11) sees them (§10.4).
func SetAutomationEnabled(ctx context.Context, tx *sql.Tx, id int64, on bool) error {
	res, err := tx.ExecContext(ctx, `UPDATE automations SET enabled = ? WHERE id = ?`, on, id)
	if err != nil {
		return fmt.Errorf("set automation %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set automation %d: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("automation %d: %w", id, sql.ErrNoRows)
	}
	return nil
}

// StopCall is one occurrence of the minimal learned automation case (§10.4, E17-S10) [NEW]: a
// ran tool call from the agent's pane (origin agent: a Brain-origin run never counts), whose
// newest observation of its capture session at the call is a stop of the agent. A stop is a
// hook stop record, or a pane read whose agent status is idle or done right after a pane read
// whose status is working: the tested working → idle or working → done (M0 E17-S1). A denied
// approval (blocked → idle) is no stop.
type StopCall struct {
	ToolID       int64
	ContractHash string // the hash of the checked contract that the call ran
	Stop         int64  // the stop observation: the evidence of the occurrence
	Harness      string // the CLI kind of the session
	WorkspaceKey string // the workspace of the stop's segment; "" when it has no Brain key
	PaneID       string
}

// StopCalls returns the occurrences of the learned automation case in the sessions of herdr
// session herdrSession, in call order.
func StopCalls(ctx context.Context, tx *sql.Tx, herdrSession string) ([]StopCall, error) {
	return query(ctx, tx, `SELECT c.tool_id, c.contract_hash, o.id, s.harness, COALESCE(g.workspace_key, ''), s.pane_id
		FROM tool_calls c JOIN sessions s ON s.id = c.session_id
		JOIN observations o ON o.id = (SELECT o2.id FROM observations o2 JOIN segments g2 ON g2.id = o2.segment_id
			WHERE g2.session_id = s.id AND o2.at <= c.at ORDER BY o2.at DESC, o2.id DESC LIMIT 1)
		JOIN segments g ON g.id = o.segment_id
		WHERE c.result = ?1 AND c.origin = ?2 AND s.herdr_session = ?3 AND o.status != ?4 AND o.brain_origin = 0
		AND ((o.source = ?5 AND o.event = 'stop') OR (o.source = ?6 AND o.event IN ('idle', 'done')
			AND (SELECT o3.source || ' ' || o3.event FROM observations o3 JOIN segments g3 ON g3.id = o3.segment_id
				WHERE g3.session_id = s.id AND (o3.at < o.at OR o3.at = o.at AND o3.id < o.id)
				ORDER BY o3.at DESC, o3.id DESC LIMIT 1) = ?6 || ' working'))
		ORDER BY c.id`,
		func(rows *sql.Rows) (StopCall, error) {
			var c StopCall
			err := rows.Scan(&c.ToolID, &c.ContractHash, &c.Stop, &c.Harness, &c.WorkspaceKey, &c.PaneID)
			return c, err
		}, CallRan, OriginAgent, herdrSession, ObservationGap, SourceHook, SourceHerdr)
}

// In reports whether cwd, the folder of an agent, is the pinned folder of a. home is the home
// folder that a leading ~ names. herdr gives the real path of a folder, and the binding can
// name it through a link (/tmp on macOS), so the links are resolved when the clean paths
// differ. [NEW] the exact folder: a subfolder is another folder.
func (a Automation) In(cwd, home string) bool {
	if cwd == "" {
		return false
	}
	folder := filepath.Clean(tool.Dir(a.Folder, home))
	if filepath.Clean(cwd) == folder {
		return true
	}
	c, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return false // a folder that cannot be read is not the pinned folder
	}
	f, err := filepath.EvalSymlinks(folder)
	return err == nil && c == f
}
