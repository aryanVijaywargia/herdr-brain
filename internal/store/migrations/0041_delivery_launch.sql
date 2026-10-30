-- The launch of a receipt (E24-S4): the launch record (launches.id) of a `brain claude` or
-- `brain codex` launch whose start hook wrote the receipt. NULL for a receipt of a session that
-- Brain did not launch, and of an older store. The secret launch ID never goes in the store.
ALTER TABLE deliveries ADD COLUMN launch_id INTEGER REFERENCES launches (id);
