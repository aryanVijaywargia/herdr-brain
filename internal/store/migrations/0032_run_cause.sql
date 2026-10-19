-- The cause of a failed automation run (#352): the failed step and its exit code as brain run
-- prints them ("step 2: npm: exit status 1"), or the refusal of the call.
-- No output of the tool. NULL for the other results and for the rows of an older Brain.
-- [NEW] the column.
ALTER TABLE automation_runs ADD COLUMN cause TEXT;
