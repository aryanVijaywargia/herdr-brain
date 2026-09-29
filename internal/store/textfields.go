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
	}
}
