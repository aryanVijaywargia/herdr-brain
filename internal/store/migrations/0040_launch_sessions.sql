-- The native sessions of each launch (E24-S5). One launch can have more than one: /clear and an
-- in-session resume start a new Claude session or Codex thread in the same CLI process.
-- native_session_id is the CLI's own session id (Claude session_id, Codex thread id). source is
-- how Brain captures the session: 'hooks' when Brain's private hooks of the launch stored a
-- record of it, 'transcript' when brained read the exact Codex rollout of the thread, 'screen'
-- when only the pane reads capture it. A source only rises, in that order from 'screen' to
-- 'hooks'. first_seen is when Brain first saw the session.
CREATE TABLE launch_sessions (
	launch_id         INTEGER NOT NULL REFERENCES launches (id),
	native_session_id TEXT NOT NULL CHECK (native_session_id <> ''),
	source            TEXT NOT NULL CHECK (source IN ('hooks', 'transcript', 'screen')),
	first_seen        TEXT NOT NULL,
	PRIMARY KEY (launch_id, native_session_id)
) STRICT;

-- codex_home is the absolute Codex home of a Codex launch (CODEX_HOME of the launch, else
-- ~/.codex), where brained finds the rollout of a thread; '' for another CLI. captured is when
-- brained finished the capture sources of an ended launch; NULL until then, so a capture that
-- an error or a stop cut short runs again.
ALTER TABLE launches ADD COLUMN codex_home TEXT NOT NULL DEFAULT '';
ALTER TABLE launches ADD COLUMN captured TEXT;
