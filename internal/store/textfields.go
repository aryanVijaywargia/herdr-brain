package store

// A Field is one TEXT column of the store.
type Field struct {
	Table, Column string
}

// ItemTextFields lists every column that can hold item text or text derived from it. Forget
// removes the text from each of them (§6.7, S47). New derived records join this list before
// they ship: the schema test fails on a TEXT column that is in neither this list nor
// noItemTextFields.
func ItemTextFields() []Field {
	return []Field{
		{"items", "anchors"}, // The paths that the item text names (§9.1).
		{"item_revisions", "text"},
		{"proposals", "text"},
		{"proposals", "target"},
		{"proposals", "diff"},
		{"proposals", "expected_benefit"},
		{"proposals", "change_request"},
		{"applied_edits", "content_before"},
		{"applied_edits", "content_after"},
		// The claim key text (§7.2): forget removes it from the claim of the item, and the
		// claim id stays.
		{"claims", "situation"},
		{"claims", "rule"},
		// Your reason for a rejection of the claim of the item (§6.7, §7.2).
		{"rejections", "reason"},
		// The prompts that led to a proposal of the item or matched it (§6.7, E14-S1). Forget
		// deletes the rows, with their hashes; brain forget --session deletes them by session.
		{"prompt_index", "text"},
	}
}

// noItemTextFields lists the TEXT columns that never hold item text: ids, keys, hashes,
// enums, times, paths of Brain's own files and herdr names.
func noItemTextFields() []Field {
	return []Field{
		{"workspaces", "key"},
		{"workspaces", "herdr_session"},
		{"workspaces", "herdr_workspace_id"},
		{"workspaces", "label"},
		{"workspaces", "root_folder"},
		{"workspaces", "status"},
		{"workspaces", "last_seen"},
		{"items", "claim_id"}, // The claim id holds no text (§7.2).
		{"items", "kind"},
		{"items", "scope"},
		{"items", "workspace_key"},
		{"items", "state"},
		{"items", "created"},
		{"proposals", "scope"},
		{"proposals", "trigger_prompts"}, // Prompt hashes (§11).
		{"proposals", "conflicts_with"},  // Item ids (§11).
		{"proposals", "status"},
		{"proposals", "surface"},
		{"proposals", "created"},
		{"proposals", "decided"},
		{"item_revisions", "scope"},
		{"item_revisions", "workspace_key"},
		{"item_revisions", "state"},
		{"item_revisions", "author"},
		{"item_revisions", "origin"},
		{"item_revisions", "conflicts_with"},
		{"item_revisions", "created"},
		{"outputs", "folder"},
		{"outputs", "manifest_hash"},
		{"outputs", "built_at"},
		{"applied_edits", "file"},
		{"applied_edits", "content_hash"},
		{"applied_edits", "applied"},
		{"applied_edits", "rolled_back"},
		{"applied_edits", "revisions_before"},
		{"applied_edits", "revisions_after"},
		{"deliveries", "profile"},
		{"deliveries", "herdr_session"},
		{"deliveries", "pane_id"},
		{"deliveries", "agent_session_id"},
		{"deliveries", "workspace_key"},
		{"deliveries", "brief_hash"},
		{"deliveries", "item_revisions"},
		{"deliveries", "cli_version"},
		{"deliveries", "result"},
		{"deliveries", "withheld_reason"},
		{"deliveries", "at"},
		{"deliveries", "agent_check"},
		// The path of the CLI's own transcript file, not item text: brain forget --session
		// clears it (E15-S4, §6.7).
		{"deliveries", "transcript_path"},
		{"review_panes", "herdr_session"},
		{"review_panes", "session_identity"},
		{"review_panes", "pane_id"},
		{"review_panes", "beside_pane_id"},
		{"review_panes", "opened"},
		{"review_panes", "closed"},
		{"tombstones", "claim_id"},
		{"tombstones", "source_keys"},
		{"tombstones", "forgotten_at"},
		{"learning_settings", "scope"},
		{"learning_settings", "herdr_session"},
		{"learning_settings", "target"}, // A workspace key or a herdr pane id.
		{"learning_settings", "learning"},
		{"learning_settings", "changed"},
		{"sessions", "herdr_session"},
		{"sessions", "harness"},
		{"sessions", "key_kind"},
		{"sessions", "key_id"}, // An agent session id or a random capture id.
		{"sessions", "agent_session_id"},
		{"sessions", "pane_id"},
		{"sessions", "workspace_key"},
		{"sessions", "started"},
		{"sessions", "ended"},
		{"segments", "pane_id"},
		{"segments", "workspace_key"},
		{"segments", "read_source"},
		{"segments", "started"},
		{"segments", "ended"},
		{"observations", "capture_identity"},
		{"observations", "source"},
		{"observations", "event"},
		// Pane text, not item text: forget finds it by session (ForgetSession), never by a
		// text search (§6.7).
		{"observations", "excerpt"},
		{"observations", "status"},
		{"observations", "sender"},
		{"observations", "at"},
		{"cursors", "source"},
		{"cursors", "position"}, // A capture identity or a byte offset (§6.4).
		// A captured command and its folder, not item text: forget finds them by session
		// (ForgetSession), as the excerpt.
		{"command_records", "argv"},
		{"command_records", "cwd"},
		{"forgotten_captures", "capture_identity"}, // No text (§6.4).
		{"claims", "id"},                           // The claim id holds no text (§7.2).
		{"claims", "same_as"},
		{"claims", "same_as_status"},
		{"claims", "created"},
		{"candidates", "claim_id"},
		{"candidates", "scope"},
		{"candidates", "workspace_key"},
		{"candidates", "kind"},
		{"candidates", "harnesses"},
		{"candidates", "first_seen"},
		{"candidates", "last_seen"},
		{"candidates", "status"},
		{"evidence_links", "capture_identity"},
		{"evidence_links", "workspace_key"},
		{"evidence_links", "harness"},
		{"evidence_links", "sender"},
		{"evidence_links", "at"},
		{"rejections", "claim_id"},
		{"rejections", "scope"},
		{"rejections", "workspace_key"},
		{"rejections", "at"},
		{"rejections", "reopened"},
		{"extraction_batches", "workspace_key"},
		{"extraction_batches", "first_capture"},
		{"extraction_batches", "last_capture"},
		{"extraction_batches", "status"},
		{"extraction_batches", "reason"}, // A fixed reason of Brain; no model text.
		{"extraction_batches", "at"},
		{"extraction_calls", "workspace_key"},
		{"extraction_calls", "at"},
		{"extraction_watermarks", "workspace_key"},
		{"extraction_watermarks", "capture_identity"},
		{"extractor_settings", "workspace_key"},
		{"extractor_settings", "extraction"},
		{"extractor_settings", "cli"},
		{"extractor_settings", "model"},
		{"extractor_settings", "changed"},
		{"change_requests", "source"},
		{"change_requests", "workspace_key"},
		{"change_requests", "status"},
		{"change_requests", "reason"}, // A fixed reason of Brain; no model text.
		{"change_requests", "created"},
		{"change_requests", "at"},
		{"prompt_index", "hash"}, // A prompt hash: forget deletes it with the row of its text.
		{"prompt_index", "workspace_key"},
		{"prompt_index", "proposal_ids"},
		{"prompt_index", "at"},
		// A suggestion holds ids only; forget deletes it with the prompt index rows (E14-S3).
		{"suggestions", "herdr_session"},
		{"suggestions", "workspace_key"},
		{"suggestions", "agent_pane_id"},
		{"suggestions", "review_pane_id"},
		{"suggestions", "prompt_hash"},
		{"suggestions", "reserved"},
		{"suggestions", "shown_at"},
		{"suggestions", "result"},
		// An outcome holds ids only (E15-S1, §11).
		{"outcomes", "kind"},
		{"outcomes", "at"},
	}
}
