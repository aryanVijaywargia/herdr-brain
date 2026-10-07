package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// The typed rows of the M1 tables (HLD §11, migration 0002). A nil pointer field is NULL, and
// so is a zero time.Time in a field that the table lets be NULL. Each table has an insert and
// a read by its key; the item commands, the compiler and the hook add the queries they need.

// WorkspaceStatus is workspaces.status.
type WorkspaceStatus string

// The workspace statuses.
const (
	WorkspaceLive   WorkspaceStatus = "live"
	WorkspaceClosed WorkspaceStatus = "closed"
)

// Kind is the kind of an item.
type Kind string

// The item kinds.
const (
	KindFact       Kind = "fact"
	KindPreference Kind = "preference"
	KindDecision   Kind = "decision"
	KindHandoff    Kind = "handoff"
	KindSkill      Kind = "skill"
	KindTool       Kind = "tool"
	KindAutomation Kind = "automation"
)

// Scope says whether an item reaches every workspace or one.
type Scope string

// The scopes.
const (
	ScopeGlobal    Scope = "global"
	ScopeWorkspace Scope = "workspace"
)

// State is the state of an item.
type State string

// The item states.
const (
	StateDraft    State = "draft"
	StateActive   State = "active"
	StateDisputed State = "disputed"
	StateRetired  State = "retired"
)

// Author says who made a revision.
type Author string

// The revision authors.
const (
	AuthorYou   Author = "you"
	AuthorBrain Author = "brain"
)

// Origin says which path made a revision.
type Origin string

// The revision origins.
const (
	OriginRemember  Origin = "remember"
	OriginReview    Origin = "review"
	OriginExtractor Origin = "extractor"
)

// ProposalStatus is proposals.status. [NEW] the values: §11 names none.
type ProposalStatus string

// The proposal statuses.
const (
	ProposalPending    ProposalStatus = "pending"
	ProposalApproved   ProposalStatus = "approved"
	ProposalRejected   ProposalStatus = "rejected"
	ProposalChanged    ProposalStatus = "changed"
	ProposalSuperseded ProposalStatus = "superseded"
)

// Surface is where you decided a proposal.
type Surface string

// The review surfaces.
const (
	SurfaceCLI   Surface = "cli"
	SurfacePopup Surface = "popup"
	SurfacePane  Surface = "pane"
)

// Result is the result of one hook delivery.
type Result string

// The delivery results.
const (
	ResultEmitted  Result = "emitted"
	ResultWithheld Result = "withheld"
	ResultUnmapped Result = "unmapped"
)

// Workspace maps one herdr workspace to Brain's own key (§11 workspace identity).
type Workspace struct {
	Key              string
	HerdrSession     string
	HerdrWorkspaceID string
	Label            string
	RootFolder       *string
	Status           WorkspaceStatus
	LastSeen         time.Time
}

// InsertWorkspace adds w.
func InsertWorkspace(ctx context.Context, tx *sql.Tx, w Workspace) error {
	_, err := insertRow(ctx, tx, "workspace", `INSERT INTO workspaces
		(key, herdr_session, herdr_workspace_id, label, root_folder, status, last_seen)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		w.Key, w.HerdrSession, w.HerdrWorkspaceID, w.Label, w.RootFolder, w.Status, timeArg(w.LastSeen))
	return err
}

// WorkspaceByKey reads the workspace with key. It returns sql.ErrNoRows when there is none.
func WorkspaceByKey(ctx context.Context, tx *sql.Tx, key string) (Workspace, error) {
	var w Workspace
	err := tx.QueryRowContext(ctx, `SELECT key, herdr_session, herdr_workspace_id, label, root_folder,
		status, last_seen FROM workspaces WHERE key = ?`, key).Scan(
		&w.Key, &w.HerdrSession, &w.HerdrWorkspaceID, &w.Label, &w.RootFolder, &w.Status, timeDest{&w.LastSeen})
	if err != nil {
		return Workspace{}, fmt.Errorf("read workspace %s: %w", key, err)
	}
	return w, nil
}

// Item is the current state of one item. Its fields copy its current revision.
type Item struct {
	ID              int64
	ClaimID         string
	CurrentRevision int64
	Kind            Kind
	Scope           Scope
	WorkspaceKey    *string
	State           State
	Pinned          bool
	// Anchors are the git files that the item text names (§9.1), set at approval.
	Anchors []Anchor
	Created time.Time
}

// Anchor is one git file that an item text names (§9.1): the repo root, the path in the repo
// and the blob hash of the file at approval.
type Anchor struct {
	Root string `json:"root"`
	Path string `json:"path"`
	Blob string `json:"blob"`
}

// InsertItem adds it and returns its new id. it.ID is ignored.
func InsertItem(ctx context.Context, tx *sql.Tx, it Item) (int64, error) {
	return insertRow(ctx, tx, "item", `INSERT INTO items
		(claim_id, current_revision, kind, scope, workspace_key, state, pinned, anchors, created)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		it.ClaimID, it.CurrentRevision, it.Kind, it.Scope, it.WorkspaceKey, it.State, it.Pinned,
		jsonList[Anchor](it.Anchors), timeArg(it.Created))
}

// ItemByID reads the item with id. It returns sql.ErrNoRows when there is none.
func ItemByID(ctx context.Context, tx *sql.Tx, id int64) (Item, error) {
	var it Item
	err := tx.QueryRowContext(ctx, `SELECT id, claim_id, current_revision, kind, scope, workspace_key,
		state, pinned, anchors, created FROM items WHERE id = ?`, id).Scan(
		&it.ID, &it.ClaimID, &it.CurrentRevision, &it.Kind, &it.Scope, &it.WorkspaceKey, &it.State,
		&it.Pinned, listDest[Anchor]{&it.Anchors}, timeDest{&it.Created})
	if err != nil {
		return Item{}, fmt.Errorf("read item %d: %w", id, err)
	}
	return it, nil
}

// ItemRevision holds every field of an item that undo can restore (§7.5).
type ItemRevision struct {
	ItemID       int64
	Revision     int64
	Text         string
	Scope        Scope
	WorkspaceKey *string
	State        State
	Pinned       bool
	Author       Author
	Origin       Origin
	ProposalID   *int64
	// UndoesRevision is the revision that this revision undid.
	UndoesRevision *int64
	// ExceptionToItem and ExceptionToRevision link a workspace item to the global item that it
	// is an exception to (§7.4). Both are set, or neither.
	ExceptionToItem     *int64
	ExceptionToRevision *int64
	ConflictsWith       []int64
	Created             time.Time
}

// InsertItemRevision adds r.
func InsertItemRevision(ctx context.Context, tx *sql.Tx, r ItemRevision) error {
	_, err := insertRow(ctx, tx, "item revision", `INSERT INTO item_revisions
		(item_id, revision, text, scope, workspace_key, state, pinned, author, origin, proposal_id,
		undoes_revision, exception_to_item, exception_to_revision, conflicts_with, created)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ItemID, r.Revision, r.Text, r.Scope, r.WorkspaceKey, r.State, r.Pinned, r.Author, r.Origin,
		r.ProposalID, r.UndoesRevision, r.ExceptionToItem, r.ExceptionToRevision,
		jsonList[int64](r.ConflictsWith), timeArg(r.Created))
	return err
}

// ItemRevisionByKey reads one revision of an item. It returns sql.ErrNoRows when there is none.
func ItemRevisionByKey(ctx context.Context, tx *sql.Tx, itemID, revision int64) (ItemRevision, error) {
	r, err := scanRevision(tx.QueryRowContext(ctx, `SELECT `+revisionColumns+` FROM item_revisions r
		WHERE item_id = ? AND revision = ?`, itemID, revision))
	if err != nil {
		return ItemRevision{}, fmt.Errorf("read item %d revision %d: %w", itemID, revision, err)
	}
	return r, nil
}

// revisionColumns are the columns of an item_revisions row r, in the order of scanRevision.
const revisionColumns = `r.item_id, r.revision, r.text, r.scope, r.workspace_key, r.state, r.pinned,
	r.author, r.origin, r.proposal_id, r.undoes_revision, r.exception_to_item, r.exception_to_revision,
	r.conflicts_with, r.created`

func scanRevision(row interface{ Scan(dest ...any) error }) (ItemRevision, error) {
	var r ItemRevision
	err := row.Scan(&r.ItemID, &r.Revision, &r.Text, &r.Scope, &r.WorkspaceKey, &r.State, &r.Pinned,
		&r.Author, &r.Origin, &r.ProposalID, &r.UndoesRevision, &r.ExceptionToItem, &r.ExceptionToRevision,
		listDest[int64]{&r.ConflictsWith}, timeDest{&r.Created})
	return r, err
}

// Proposal is one change that waits for your decision, or that you decided (§7.4).
type Proposal struct {
	ID                int64
	CandidateID       *int64
	ItemID            *int64
	BaseRevision      *int64
	Text              string
	Scope             Scope
	Target            string
	Diff              string
	ExpectedBenefit   string
	PermissionWarning bool
	// ConflictsWith are the items that the proposal conflicts with (§7.4, M2): the active
	// items of the claim that the extractor named.
	ConflictsWith []int64
	// TriggerPrompts are the hashes of the prompts that led to the proposal.
	TriggerPrompts []string
	ChangeRequest  *string
	// Dispute are the paths of the anchors whose change made the item disputed (§9.1). Empty
	// for a proposal of another kind.
	Dispute       []string
	Status        ProposalStatus
	Surface       *Surface
	Created       time.Time
	Decided       time.Time
	ReviewSeconds *int64
}

// InsertProposal adds p and returns its new id. p.ID is ignored.
func InsertProposal(ctx context.Context, tx *sql.Tx, p Proposal) (int64, error) {
	return insertRow(ctx, tx, "proposal", `INSERT INTO proposals
		(candidate_id, item_id, base_revision, text, scope, target, diff, expected_benefit,
		permission_warning, conflicts_with, trigger_prompts, change_request, dispute, status, surface, created,
		decided, review_seconds)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.CandidateID, p.ItemID, p.BaseRevision, p.Text, p.Scope, p.Target, p.Diff, p.ExpectedBenefit,
		p.PermissionWarning, jsonList[int64](p.ConflictsWith), jsonList[string](p.TriggerPrompts), p.ChangeRequest,
		jsonList[string](p.Dispute), p.Status, p.Surface, timeArg(p.Created), timeArg(p.Decided), p.ReviewSeconds)
}

// ProposalByID reads the proposal with id. It returns sql.ErrNoRows when there is none.
func ProposalByID(ctx context.Context, tx *sql.Tx, id int64) (Proposal, error) {
	var p Proposal
	err := tx.QueryRowContext(ctx, `SELECT id, candidate_id, item_id, base_revision, text, scope, target,
		diff, expected_benefit, permission_warning, conflicts_with, trigger_prompts, change_request, dispute,
		status, surface, created, decided, review_seconds FROM proposals WHERE id = ?`, id).Scan(
		&p.ID, &p.CandidateID, &p.ItemID, &p.BaseRevision, &p.Text, &p.Scope, &p.Target, &p.Diff,
		&p.ExpectedBenefit, &p.PermissionWarning, listDest[int64]{&p.ConflictsWith}, listDest[string]{&p.TriggerPrompts}, &p.ChangeRequest,
		listDest[string]{&p.Dispute}, &p.Status, &p.Surface, timeDest{&p.Created}, timeDest{&p.Decided}, &p.ReviewSeconds)
	if err != nil {
		return Proposal{}, fmt.Errorf("read proposal %d: %w", id, err)
	}
	return p, nil
}

// Output is one output generation (§7.5). ManifestHash and BuiltAt stay empty until the
// compiler builds the generation. Current is a copy: the pointer file is the authority.
type Output struct {
	Generation         int64
	Folder             string
	ManifestHash       *string
	BuiltAt            time.Time
	Current            bool
	HoldsForgottenText bool
}

// InsertOutput adds o.
func InsertOutput(ctx context.Context, tx *sql.Tx, o Output) error {
	_, err := insertRow(ctx, tx, "output", `INSERT INTO outputs
		(generation, folder, manifest_hash, built_at, current, holds_forgotten_text)
		VALUES (?, ?, ?, ?, ?, ?)`,
		o.Generation, o.Folder, o.ManifestHash, timeArg(o.BuiltAt), o.Current, o.HoldsForgottenText)
	return err
}

// OutputByGeneration reads one output generation. It returns sql.ErrNoRows when there is none.
func OutputByGeneration(ctx context.Context, tx *sql.Tx, generation int64) (Output, error) {
	var o Output
	err := tx.QueryRowContext(ctx, `SELECT generation, folder, manifest_hash, built_at, current,
		holds_forgotten_text FROM outputs WHERE generation = ?`, generation).Scan(
		&o.Generation, &o.Folder, &o.ManifestHash, timeDest{&o.BuiltAt}, &o.Current, &o.HoldsForgottenText)
	if err != nil {
		return Output{}, fmt.Errorf("read output generation %d: %w", generation, err)
	}
	return o, nil
}

// AppliedEdit logs one file that an output generation wrote (§7.5 step 7). ContentBefore is
// nil when the edit created the file, and ContentAfter when it removed the file.
// ContentBefore is nil too when the forget rule kept the old bytes out (§6.7); then
// RevisionsBefore is not empty.
type AppliedEdit struct {
	ID            int64
	ProposalID    *int64
	File          string
	Generation    int64
	ContentBefore *string
	ContentAfter  *string
	ContentHash   string
	Applied       time.Time
	RolledBack    time.Time
	// RevisionsBefore and RevisionsAfter are the (item id, revision) pairs in the file before
	// and after the edit. They hold no text, so they stay after forget.
	RevisionsBefore []RevisionRef
	RevisionsAfter  []RevisionRef
}

// InsertAppliedEdit adds e and returns its new id. e.ID is ignored.
func InsertAppliedEdit(ctx context.Context, tx *sql.Tx, e AppliedEdit) (int64, error) {
	return insertRow(ctx, tx, "applied edit", `INSERT INTO applied_edits
		(proposal_id, file, generation, content_before, content_after, content_hash, applied, rolled_back,
		revisions_before, revisions_after)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ProposalID, e.File, e.Generation, e.ContentBefore, e.ContentAfter, e.ContentHash,
		timeArg(e.Applied), timeArg(e.RolledBack), jsonList[RevisionRef](e.RevisionsBefore),
		jsonList[RevisionRef](e.RevisionsAfter))
}

// AppliedEditByID reads the applied edit with id. It returns sql.ErrNoRows when there is none.
func AppliedEditByID(ctx context.Context, tx *sql.Tx, id int64) (AppliedEdit, error) {
	var e AppliedEdit
	err := tx.QueryRowContext(ctx, `SELECT id, proposal_id, file, generation, content_before,
		content_after, content_hash, applied, rolled_back, revisions_before, revisions_after
		FROM applied_edits WHERE id = ?`, id).Scan(
		&e.ID, &e.ProposalID, &e.File, &e.Generation, &e.ContentBefore, &e.ContentAfter, &e.ContentHash,
		timeDest{&e.Applied}, timeDest{&e.RolledBack}, listDest[RevisionRef]{&e.RevisionsBefore},
		listDest[RevisionRef]{&e.RevisionsAfter})
	if err != nil {
		return AppliedEdit{}, fmt.Errorf("read applied edit %d: %w", id, err)
	}
	return e, nil
}

// RevisionRef names one revision of one item.
type RevisionRef struct {
	Item     int64 `json:"item"`
	Revision int64 `json:"revision"`
}

// Delivery is the receipt of one hook run (§8.3).
type Delivery struct {
	ID               int64
	Profile          string
	HerdrSession     *string // the store binding: the herdr session of the binding
	PaneID           string
	AgentSessionID   *string
	WorkspaceKey     *string
	BriefHash        *string
	OutputGeneration *int64
	ItemRevisions    []RevisionRef
	CLIVersion       *string
	Result           Result
	WithheldReason   *string
	At               time.Time
	// AgentCheck is the deferred session-id check (§8.3 limit): "pending", "unknown", "match"
	// or "not this pane's agent". nil when the receipt holds no (pane, agent session id) record.
	AgentCheck *string
	// TranscriptPath is the transcript_path of the start hook input (E15-S4). nil when the input
	// had none, or after brain forget --session. Brain never reads the file.
	TranscriptPath *string
	// Recalled are the revisions that brain recall returned on this receipt, newest first. Only
	// TiedReceipt fills it (§8.6); no column holds it.
	Recalled []RevisionRef
}

// InsertDelivery adds d and returns its new id. d.ID is ignored.
func InsertDelivery(ctx context.Context, tx *sql.Tx, d Delivery) (int64, error) {
	return insertRow(ctx, tx, "delivery", `INSERT INTO deliveries
		(profile, herdr_session, pane_id, agent_session_id, workspace_key, brief_hash, output_generation,
		item_revisions, cli_version, result, withheld_reason, at, agent_check, transcript_path)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.Profile, d.HerdrSession, d.PaneID, d.AgentSessionID, d.WorkspaceKey, d.BriefHash, d.OutputGeneration,
		jsonList[RevisionRef](d.ItemRevisions), d.CLIVersion, d.Result, d.WithheldReason, timeArg(d.At), d.AgentCheck,
		d.TranscriptPath)
}

// DeliveryByID reads the delivery with id. It returns sql.ErrNoRows when there is none.
func DeliveryByID(ctx context.Context, tx *sql.Tx, id int64) (Delivery, error) {
	var d Delivery
	err := tx.QueryRowContext(ctx, `SELECT id, profile, herdr_session, pane_id, agent_session_id,
		workspace_key, brief_hash, output_generation, item_revisions, cli_version, result,
		withheld_reason, at, agent_check, transcript_path
		FROM deliveries WHERE id = ?`, id).Scan(
		&d.ID, &d.Profile, &d.HerdrSession, &d.PaneID, &d.AgentSessionID, &d.WorkspaceKey, &d.BriefHash,
		&d.OutputGeneration, listDest[RevisionRef]{&d.ItemRevisions}, &d.CLIVersion, &d.Result,
		&d.WithheldReason, timeDest{&d.At}, &d.AgentCheck, &d.TranscriptPath)
	if err != nil {
		return Delivery{}, fmt.Errorf("read delivery %d: %w", id, err)
	}
	return d, nil
}

// AgentSessions returns the agent session ids of the receipts of paneID in herdr session
// whose agent check is check, each id once. It reads outside a write transaction: in WAL mode
// a read takes no write lock, so the hook waits for the lock once only, in its receipt.
func (s *Store) AgentSessions(ctx context.Context, session, paneID, check string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT agent_session_id FROM deliveries
		WHERE herdr_session = ? AND pane_id = ? AND agent_check = ? AND agent_session_id IS NOT NULL
		ORDER BY agent_session_id`, session, paneID, check)
	if err != nil {
		return nil, fmt.Errorf("read agent sessions of pane %s: %w", paneID, err)
	}
	defer rows.Close() //nolint:errcheck // read-only: rows.Err reports the read errors
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("read agent sessions of pane %s: %w", paneID, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read agent sessions of pane %s: %w", paneID, err)
	}
	return ids, nil
}

// SetAgentCheck changes the agent check of the receipts of (session, paneID, agentSession)
// from from to to.
func SetAgentCheck(ctx context.Context, tx *sql.Tx, session, paneID, agentSession, from, to string) error {
	_, err := tx.ExecContext(ctx, `UPDATE deliveries SET agent_check = ?
		WHERE herdr_session = ? AND pane_id = ? AND agent_session_id = ? AND agent_check = ?`,
		to, session, paneID, agentSession, from)
	if err != nil {
		return fmt.Errorf("set agent check of pane %s: %w", paneID, err)
	}
	return nil
}

// ReviewPane is one review pane that Brain opened. Brain closes or targets only a pane whose
// binding and pane id are in this table (§11).
type ReviewPane struct {
	ID           int64
	HerdrSession string
	// SessionIdentity is the herdr session's continuity identity. herdr 0.9.1 has none (§4.2).
	SessionIdentity *string
	PaneID          string
	BesidePaneID    string
	ProposalID      int64
	Opened          time.Time
	Closed          time.Time
}

// InsertReviewPane adds p and returns its new id. p.ID is ignored.
func InsertReviewPane(ctx context.Context, tx *sql.Tx, p ReviewPane) (int64, error) {
	return insertRow(ctx, tx, "review pane", `INSERT INTO review_panes
		(herdr_session, session_identity, pane_id, beside_pane_id, proposal_id, opened, closed)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		p.HerdrSession, p.SessionIdentity, p.PaneID, p.BesidePaneID, p.ProposalID, timeArg(p.Opened),
		timeArg(p.Closed))
}

// ReviewPaneByID reads the review pane with id. It returns sql.ErrNoRows when there is none.
func ReviewPaneByID(ctx context.Context, tx *sql.Tx, id int64) (ReviewPane, error) {
	var p ReviewPane
	err := tx.QueryRowContext(ctx, `SELECT id, herdr_session, session_identity, pane_id, beside_pane_id,
		proposal_id, opened, closed FROM review_panes WHERE id = ?`, id).Scan(
		&p.ID, &p.HerdrSession, &p.SessionIdentity, &p.PaneID, &p.BesidePaneID, &p.ProposalID,
		timeDest{&p.Opened}, timeDest{&p.Closed})
	if err != nil {
		return ReviewPane{}, fmt.Errorf("read review pane %d: %w", id, err)
	}
	return p, nil
}

// OpenReviewPanes reads the review panes of proposal proposalID that Brain opened, in any herdr
// session, and has not marked closed, oldest first.
func OpenReviewPanes(ctx context.Context, tx *sql.Tx, proposalID int64) ([]ReviewPane, error) {
	ps, err := openReviewPanes(ctx, tx, `proposal_id = ?`, proposalID)
	if err != nil {
		return nil, fmt.Errorf("read review panes of proposal %d: %w", proposalID, err)
	}
	return ps, nil
}

// ReviewPanesBeside reads the review panes that Brain opened beside pane paneID of herdr session
// herdrSession and has not marked closed, oldest first: one agent has one review pane at a
// time (§5.5).
func ReviewPanesBeside(ctx context.Context, tx *sql.Tx, herdrSession, paneID string) ([]ReviewPane, error) {
	ps, err := openReviewPanes(ctx, tx, `herdr_session = ? AND beside_pane_id = ?`, herdrSession, paneID)
	if err != nil {
		return nil, fmt.Errorf("read review panes beside pane %s: %w", paneID, err)
	}
	return ps, nil
}

// openReviewPanes reads the review panes that match where and are not marked closed, by id.
func openReviewPanes(ctx context.Context, tx *sql.Tx, where string, args ...any) ([]ReviewPane, error) {
	return query(ctx, tx, `SELECT id, herdr_session, session_identity, pane_id, beside_pane_id, proposal_id, opened, closed
		FROM review_panes WHERE `+where+` AND closed IS NULL ORDER BY id`,
		func(rows *sql.Rows) (ReviewPane, error) {
			var p ReviewPane
			err := rows.Scan(&p.ID, &p.HerdrSession, &p.SessionIdentity, &p.PaneID, &p.BesidePaneID, &p.ProposalID,
				timeDest{&p.Opened}, timeDest{&p.Closed})
			return p, err
		}, args...)
}

// CloseReviewPane marks the review pane with id closed at now.
func CloseReviewPane(ctx context.Context, tx *sql.Tx, id int64, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `UPDATE review_panes SET closed = ? WHERE id = ?`, timeArg(now), id); err != nil {
		return fmt.Errorf("close review pane %d: %w", id, err)
	}
	return nil
}

// Tombstone is what forget keeps of a claim, so the same evidence does not come back (§6.7).
// It holds no text.
type Tombstone struct {
	ClaimID     string
	SourceKeys  []string
	ForgottenAt time.Time
}

// InsertTombstone adds t.
func InsertTombstone(ctx context.Context, tx *sql.Tx, t Tombstone) error {
	_, err := insertRow(ctx, tx, "tombstone", `INSERT INTO tombstones (claim_id, source_keys, forgotten_at)
		VALUES (?, ?, ?)`, t.ClaimID, jsonList[string](t.SourceKeys), timeArg(t.ForgottenAt))
	return err
}

// TombstonesByClaim reads the tombstones of a claim, oldest first.
func TombstonesByClaim(ctx context.Context, tx *sql.Tx, claimID string) ([]Tombstone, error) {
	rows, err := tx.QueryContext(ctx, `SELECT claim_id, source_keys, forgotten_at FROM tombstones
		WHERE claim_id = ? ORDER BY forgotten_at, rowid`, claimID)
	if err != nil {
		return nil, fmt.Errorf("read tombstones of claim %s: %w", claimID, err)
	}
	defer rows.Close() //nolint:errcheck // read-only: rows.Err reports the read errors
	var ts []Tombstone
	for rows.Next() {
		var t Tombstone
		if err := rows.Scan(&t.ClaimID, listDest[string]{&t.SourceKeys}, timeDest{&t.ForgottenAt}); err != nil {
			return nil, fmt.Errorf("read tombstones of claim %s: %w", claimID, err)
		}
		ts = append(ts, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read tombstones of claim %s: %w", claimID, err)
	}
	return ts, nil
}

// insertRow runs one INSERT and returns the rowid of the new row.
func insertRow(ctx context.Context, tx *sql.Tx, what, query string, args ...any) (int64, error) {
	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("insert %s: %w", what, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("insert %s: %w", what, err)
	}
	return id, nil
}
