-- The launch process of each launch record (#458): the process id of `brain claude` or `brain
-- codex`, which then runs the CLI in the same process (exec). After a restart, herdr restores a
-- pane with its old pane id, but the CLI process is gone. brained ends a running record whose
-- process is gone, and the launch check refuses it. 0 for a record from before this column: it
-- gets no process check.
ALTER TABLE launches ADD COLUMN pid INTEGER NOT NULL DEFAULT 0 CHECK (pid >= 0);
