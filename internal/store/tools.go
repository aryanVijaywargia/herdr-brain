package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/tool"
)

// Tool is the tools row of a tool item: the contract of its current revision, the one
// hash of that contract, and the hash that you approved.
type Tool struct {
	ID           int64
	Name         string
	Scope        Scope
	WorkspaceKey *string
	// Contract is the whole text of the current revision: tool.toml and run.sh (tool.Files).
	Contract     string
	ContractHash string
	Cwd          string
	TimeoutS     int64
	Effects      string
	ReadOnly     bool
	// ApprovedHash is the contract hash that you approved, nil while the revision is not active.
	ApprovedHash *string
	Approved     time.Time
	// State is the state of the tool item.
	State State
}

// Ready says whether the tools row lets t run: an active tool whose contract hash is the one
// that you approved. The brief catalog and brain tools list only such tools (E17-S5). brain run
// checks the tool files too (output.MayRun).
func (t Tool) Ready() bool {
	return t.State == StateActive && t.ApprovedHash != nil && *t.ApprovedHash == t.ContractHash
}

// ErrToolName means that another active tool in the same scope has the name already.
var ErrToolName = errors.New("another active tool in this scope has this name")

// ActiveTools reads the current revision of each active or disputed tool item, in item id
// order. All their revision files stay.
func ActiveTools(ctx context.Context, tx *sql.Tx) ([]ItemRevision, error) {
	return activeOfKind(ctx, tx, KindTool)
}

// ToolText is the text of a tool of an item: its current revision or a pending proposal of it.
type ToolText struct {
	ItemID  int64
	ClaimID string
	Text    string
}

// ToolTexts returns the tools that a pane of workspace workspaceKey sees, in item id order: the
// current revision of each active or disputed tool item, and the text of each pending proposal
// of a tool item, of global items and of the items of that workspace ("" for global items
// only). A pending proposal to global, as a promotion, is global already. A new tool draft must
// not repeat one of them (#337, #339).
func ToolTexts(ctx context.Context, tx *sql.Tx, workspaceKey string) ([]ToolText, error) {
	ts, err := query(ctx, tx, `SELECT i.id, i.claim_id, r.text FROM items i
		JOIN item_revisions r ON r.item_id = i.id AND r.revision = i.current_revision
		WHERE i.kind = ?1 AND i.state IN (?2, ?3) AND (i.workspace_key IS NULL OR i.workspace_key = ?4)
		UNION ALL
		SELECT i.id, i.claim_id, p.text FROM proposals p JOIN items i ON i.id = p.item_id
		WHERE i.kind = ?1 AND p.status = ?5 AND p.text != ''
		AND (p.scope = ?6 OR i.workspace_key IS NULL OR i.workspace_key = ?4)
		ORDER BY 1`,
		func(rows *sql.Rows) (ToolText, error) {
			var t ToolText
			err := rows.Scan(&t.ItemID, &t.ClaimID, &t.Text)
			return t, err
		}, KindTool, StateActive, StateDisputed, workspaceKey, ProposalPending, ScopeGlobal)
	if err != nil {
		return nil, fmt.Errorf("read tool texts: %w", err)
	}
	return ts, nil
}

// syncTool writes the tools row of item, when it is a tool, from its current revision.
// A contract that breaks a tool form rule refuses the write (tool.Parse), and so does an active
// tool whose name another active tool of its scope has (ErrToolName). Each revision write that
// makes a tool active is your decision (approve, restore, undo, promotion, link), so it
// approves the contract hash of that revision. Any other state clears the approval, and the
// tool does not run; a retired tool keeps the rest of its row. Any other item passes.
func syncTool(ctx context.Context, tx *sql.Tx, item int64) error {
	it, err := ItemByID(ctx, tx, item)
	if err != nil || it.Kind != KindTool {
		return err
	}
	if it.State == StateRetired {
		// A retired revision needs no contract: forget writes one with no text.
		_, err := tx.ExecContext(ctx, `UPDATE tools SET approved_hash = NULL, approved = NULL WHERE id = ?`, item)
		if err != nil {
			return fmt.Errorf("retire tool %d: %w", item, err)
		}
		return nil
	}
	cur, err := ItemRevisionByKey(ctx, tx, item, it.CurrentRevision)
	if err != nil {
		return err
	}
	c, err := tool.Parse(cur.Text)
	if err != nil {
		return fmt.Errorf("tool item %d: %w", item, err)
	}
	t := Tool{ID: item, Name: c.Name, Scope: cur.Scope, WorkspaceKey: cur.WorkspaceKey, Contract: cur.Text,
		ContractHash: tool.Hash(cur.Text, string(cur.Scope), ptrString(cur.WorkspaceKey)), Cwd: c.Cwd,
		TimeoutS: c.TimeoutS, Effects: c.Effects, ReadOnly: c.ReadOnly}
	if it.State == StateActive {
		other, err := nameTaken(ctx, tx, KindTool, cur, c.Name, func(text string) (string, error) {
			o, err := tool.Parse(text)
			return o.Name, err
		})
		if err != nil {
			return err
		}
		if other != 0 {
			return fmt.Errorf("tool %q: item %d: %w", c.Name, other, ErrToolName)
		}
		t.ApprovedHash, t.Approved = &t.ContractHash, cur.Created
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO tools (id, name, scope, workspace_key, contract, contract_hash,
		cwd, timeout_s, effects, read_only, approved_hash, approved) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET name = excluded.name, scope = excluded.scope,
		workspace_key = excluded.workspace_key, contract = excluded.contract,
		contract_hash = excluded.contract_hash, cwd = excluded.cwd, timeout_s = excluded.timeout_s,
		effects = excluded.effects, read_only = excluded.read_only, approved_hash = excluded.approved_hash,
		approved = excluded.approved`,
		t.ID, t.Name, t.Scope, t.WorkspaceKey, t.Contract, t.ContractHash, t.Cwd, t.TimeoutS, t.Effects,
		t.ReadOnly, t.ApprovedHash, timeArg(t.Approved))
	if err != nil {
		return fmt.Errorf("write tool %d: %w", item, err)
	}
	return nil
}

// toolColumns are the columns of a Tool, for scanTool.
const toolColumns = `t.id, t.name, t.scope, t.workspace_key, t.contract, t.contract_hash, t.cwd, t.timeout_s, t.effects,
	t.read_only, t.approved_hash, t.approved, i.state FROM tools t JOIN items i ON i.id = t.id`

// scanTool scans one row of toolColumns.
func scanTool(row interface{ Scan(...any) error }) (Tool, error) {
	var t Tool
	err := row.Scan(&t.ID, &t.Name, &t.Scope, &t.WorkspaceKey, &t.Contract, &t.ContractHash, &t.Cwd, &t.TimeoutS,
		&t.Effects, &t.ReadOnly, &t.ApprovedHash, timeDest{&t.Approved}, &t.State)
	return t, err
}

// ToolByID reads the tools row of tool item id. It returns sql.ErrNoRows when there is none.
func ToolByID(ctx context.Context, tx *sql.Tx, id int64) (Tool, error) {
	t, err := scanTool(tx.QueryRowContext(ctx, `SELECT `+toolColumns+` WHERE t.id = ?`, id))
	if err != nil {
		return Tool{}, fmt.Errorf("read tool %d: %w", id, err)
	}
	return t, nil
}

// Tools reads every tools row, in approval order, then item id order; a tool with no approval
// comes last (E17-S5). [NEW] the order: by approval time, so the catalog stays stable.
func Tools(ctx context.Context, tx *sql.Tx) ([]Tool, error) {
	ts, err := query(ctx, tx, `SELECT `+toolColumns+` ORDER BY t.approved IS NULL, t.approved, t.id`,
		func(rows *sql.Rows) (Tool, error) { return scanTool(rows) })
	if err != nil {
		return nil, fmt.Errorf("read tools: %w", err)
	}
	return ts, nil
}

// ToolByName finds the tool name that a pane of workspace workspaceKey can call: a tool of
// that workspace first, else a global tool ("" finds only a global one). Of the tools with the
// name in one scope, the approved one comes first, then the newest. A retired tool is not
// found. It returns sql.ErrNoRows when there is none; a tool of another workspace is never
// found. [NEW] the order.
func ToolByName(ctx context.Context, tx *sql.Tx, name, workspaceKey string) (Tool, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT t.id FROM tools t JOIN items i ON i.id = t.id
		WHERE t.name = ?1 AND i.state != 'retired'
		AND (t.scope = 'global' OR (?2 != '' AND t.workspace_key = ?2))
		ORDER BY t.scope = 'workspace' DESC, t.approved_hash IS NOT NULL DESC, t.id DESC LIMIT 1`,
		name, workspaceKey).Scan(&id)
	if err != nil {
		return Tool{}, fmt.Errorf("find tool %q: %w", name, err)
	}
	return ToolByID(ctx, tx, id)
}
