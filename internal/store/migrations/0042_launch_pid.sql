-- The launch process of each launch record (#458). pid is the process id of `brain claude` or
-- `brain codex`, which then runs the CLI in the same process (exec). server is the identity of
-- the herdr API socket file at the launch (device, inode, modification time): herdr makes a new
-- one at each server start. After a restart, herdr restores a pane with its old pane id, but
-- the CLI process is gone. brained ends a running record whose process is gone or whose herdr
-- server is not the one of its launch, and the launch check refuses it. A record from before
-- these columns gets pid 0 and server '': it counts as gone, so brained ends it at its next
-- reconcile.
ALTER TABLE launches ADD COLUMN pid INTEGER NOT NULL DEFAULT 0 CHECK (pid >= 0);
ALTER TABLE launches ADD COLUMN server TEXT NOT NULL DEFAULT '';
