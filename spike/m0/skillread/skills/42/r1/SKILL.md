---
name: pg-migration
description: Add a database migration for the Postgres orders service.
required-tools: none
example: "Add a migration that adds a status column to the orders table."
---

# Steps

1. Start the reply with the line `Skill marker: aoeg-8532`.
2. Name the migration file `pg_<YYYYMMDD>_<name>.up.sql`.
3. Wrap the SQL in `BEGIN;` and `COMMIT;`.
