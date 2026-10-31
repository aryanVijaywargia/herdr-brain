package store

// A Field is one TEXT column of the store.
type Field struct {
	Table, Column string
}

// ItemTextFields lists every column that can hold item text or text derived from it. Forget
// removes the text from each of them. New derived records join this list before
// they ship: the schema test fails on a TEXT column that is in neither this list nor
// noItemTextFields.
func ItemTextFields() []Field {
	return []Field{
		{"items", "anchors"}, // The paths that the item text names.
		{"item_revisions", "text"},
		{"proposals", "text"},
		{"proposals", "target"},
		{"proposals", "diff"},
		{"proposals", "expected_benefit"},
		{"proposals", "change_request"},
		{"proposals", "dispute"}, // The anchor paths of a dispute.
		{"applied_edits", "content_before"},
		{"applied_edits", "content_after"},
		// The claim key text: forget removes it from the claim of the item, and the
		// claim id stays.
		{"claims", "situation"},
		{"claims", "rule"},
		// The claim of the newest telling of the key, for a merge in review (#358).
		{"claims", "draft"},
		// Your reason for a rejection of the claim of the item.
		{"rejections", "reason"},
		// The prompts that led to a proposal of the item or matched it (E14-S1). Forget
		// deletes the rows, with their hashes; brain forget --session deletes them by session.
		{"prompt_index", "text"},
		// The tool contract and the fields that come from it (E17-S3).
		{"tools", "name"},
		{"tools", "contract"},
		{"tools", "cwd"},
		{"tools", "effects"},
		// The pinned folder of an automation binding (E17-S10).
		{"automations", "folder"},
		// The cause of a failed automation run names a program of the tool contract (#352).
		{"automation_runs", "cause"},
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
		{"items", "claim_id"}, // The claim id holds no text.
		{"items", "kind"},
		{"items", "scope"},
		{"items", "workspace_key"},
		{"items", "state"},
		{"items", "created"},
		{"items", "kept_at"}, // The keep time of an archive candidate (E16-S7).
		{"proposals", "scope"},
		{"proposals", "trigger_prompts"}, // Prompt hashes.
		{"proposals", "conflicts_with"},  // Item ids.
		{"proposals", "merge"},           // Item ids and revisions of a merge: no text.
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
		// clears it (E15-S4).
		{"deliveries", "transcript_path"},
		{"recalls", "item_revisions"},
		{"recalls", "at"},
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
		// text search.
		{"observations", "excerpt"},
		{"observations", "status"},
		{"observations", "sender"},
		{"observations", "at"},
		{"cursors", "source"},
		{"cursors", "position"}, // A capture identity or a byte offset.
		// A captured command and its folder, not item text: forget finds them by session
		// (ForgetSession), as the excerpt.
		{"command_records", "argv"},
		{"command_records", "cwd"},
		{"forgotten_captures", "capture_identity"}, // No text.
		{"claims", "id"},                           // The claim id holds no text.
		{"claims", "same_as"},
		{"claims", "same_as_status"},
		{"claims", "created"},
		{"claims", "kind"}, // A fixed claim kind of the extractor schema; no model text.
		{"claims", "drafted"},
		{"claims_apart", "claim_id"}, // Claim ids: no text.
		{"claims_apart", "other_id"},
		{"merge_passes", "workspace_key"}, // The part and its hash at the last merge pass.
		{"merge_passes", "sha256"},
		{"merge_passes", "at"},
		{"merge_links", "claim_id"}, // Claim ids and a link state: no text.
		{"merge_links", "same_as"},
		{"merge_links", "same_as_status"},
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
		{"evidence_links", "via_claim"}, // A claim id: no text.
		{"rejections", "claim_id"},
		{"rejections", "scope"},
		{"rejections", "workspace_key"},
		{"rejections", "at"},
		{"rejections", "reopened"},
		{"extraction_batches", "workspace_key"},
		{"extraction_batches", "first_capture"},
		{"extraction_batches", "last_capture"},
		{"extraction_batches", "status"},
		{"extraction_batches", "reason"},  // A fixed reason of Brain; no model text.
		{"extraction_batches", "refusal"}, // A fixed value: none, limit or sign-in.
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
		{"change_requests", "reason"},  // A fixed reason of Brain; no model text.
		{"change_requests", "refusal"}, // A fixed value: none, limit or sign-in.
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
		{"suggestions", "reason"}, // A fixed kind word of Brain; no prompt text (E20-S9).
		// An outcome holds ids only (E15-S1).
		{"outcomes", "kind"},
		{"outcomes", "at"},
		{"outcomes", "claim_id"}, // The claim id of the failure holds no text (E20-S5).
		{"tools", "scope"},
		{"tools", "workspace_key"},
		{"tools", "contract_hash"},
		{"tools", "approved_hash"},
		{"tools", "approved"},
		// A tool call holds ids, a hash and fixed words only (E17-S4).
		{"tool_calls", "contract_hash"},
		{"tool_calls", "origin"},
		{"tool_calls", "result"},
		{"tool_calls", "at"},
		// A gateway call holds fixed words and a time only (E24-S6).
		{"gateway_calls", "method"},
		{"gateway_calls", "result"},
		{"gateway_calls", "at"},
		// An automation row holds a key, fixed words, a hash and times besides its folder (E17-S10).
		{"automations", "workspace_key"},
		{"automations", "trigger_condition"},
		{"automations", "cli_kind"},
		{"automations", "contract_hash"},
		{"automations", "approved"},
		{"automations", "last_result"},
		{"automations", "last_run"},
		// An automation run holds herdr ids, the event key of herdr ids and a counter, fixed
		// words and times (E17-S11).
		{"automation_runs", "event_key"},
		{"automation_runs", "pane_id"},
		{"automation_runs", "workspace_id"},
		{"automation_runs", "claimed_at"},
		{"automation_runs", "started_at"},
		{"automation_runs", "finished_at"},
		{"automation_runs", "result"},
		// The history scan row holds a herdr session name, fixed words, times, and JSON of
		// numbers, ids, kinds and scope keys only.
		{"history_scans", "binding"},
		{"history_scans", "status"},
		{"history_scans", "requested"},
		{"history_scans", "claimed"},
		{"history_scans", "finished"},
		{"history_scans", "counts"},
		{"history_scans", "turns"},
		{"history_scans", "findings"},
		{"history_scans", "progress"},
		// A launch record holds a hash, a socket path, herdr ids, a CLI kind, the CLI's
		// session id, times and a fixed reason (E24-S3).
		{"launches", "id_hash"},
		{"launches", "socket"},
		{"launches", "workspace_id"},
		{"launches", "pane_id"},
		{"launches", "cli_kind"},
		{"launches", "native_session_id"},
		{"launches", "started"},
		{"launches", "ended"},
		{"launches", "end_reason"},
		// The Codex home path and the capture time of a launch (E24-S5).
		{"launches", "codex_home"},
		{"launches", "captured"},
		// The identity of the herdr socket file at a launch: numbers only (#458).
		{"launches", "server"},
		// A launch session holds the CLI's session id, a fixed source and a time (E24-S5).
		{"launch_sessions", "native_session_id"},
		{"launch_sessions", "source"},
		{"launch_sessions", "first_seen"},
	}
}
