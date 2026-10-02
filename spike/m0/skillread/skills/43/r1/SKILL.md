---
name: sqlite-migration
description: Add a database migration for the SQLite cache of the CLI.
required-tools: none
example: "Add a migration that adds an expires_at column to the cache table."
---

# Steps

1. Start the reply with the line `Skill marker: ypyk-7849`.
2. Name the migration file `cache_<NNN>_<name>.sql`, with a 3-digit number.
3. End the SQL with the line `PRAGMA user_version = <NNN>;`.
4. End the reply with the line `Cache checked: vfyh-0643`.
