package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/aryanVijaywargia/herdr-brain/internal/tool"
)

// Tool is the tools row of a tool item (§11): the contract of its current revision, the one
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
}

// ErrToolName means that another active tool in the same scope has the name already (§10.1).
var ErrToolName = errors.New("another active tool in this scope has this name")

// ActiveTools reads the current revision of each active or disputed tool item, in item id
// order. All their revision files stay (§7.5 step 8).
func ActiveTools(ctx context.Context, tx *sql.Tx) ([]ItemRevision, error) {
	return activeOfKind(ctx, tx, KindTool)
}

// syncTool writes the tools row of item, when it is a tool, from its current revision (§11).
// A contract that breaks a §10.1 rule refuses the write (tool.Parse), and so does an active
// tool whose name another active tool of its scope has (ErrToolName). Each revision write that
// makes a tool active is your decision (approve, restore, undo, promotion, link), so it
// approves the contract hash of that revision. Any other state clears the approval, and the
// tool does not run. Any other item passes.
func syncTool(ctx context.Context, tx *sql.Tx, item int64) error {
	it, err := ItemByID(ctx, tx, item)
	if err != nil || it.Kind != KindTool {
		return err
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

// ToolByID reads the tools row of tool item id. It returns sql.ErrNoRows when there is none.
func ToolByID(ctx context.Context, tx *sql.Tx, id int64) (Tool, error) {
	var t Tool
	err := tx.QueryRowContext(ctx, `SELECT id, name, scope, workspace_key, contract, contract_hash, cwd,
		timeout_s, effects, read_only, approved_hash, approved FROM tools WHERE id = ?`, id).Scan(
		&t.ID, &t.Name, &t.Scope, &t.WorkspaceKey, &t.Contract, &t.ContractHash, &t.Cwd, &t.TimeoutS,
		&t.Effects, &t.ReadOnly, &t.ApprovedHash, timeDest{&t.Approved})
	if err != nil {
		return Tool{}, fmt.Errorf("read tool %d: %w", id, err)
	}
	return t, nil
}
