-- The (item id, revision) pairs of the file before and after each applied edit (E6-S2). They
-- are non-text columns, so they stay when forget removes the content, and forget finds each
-- audit snapshot that holds an item. Each is a JSON array of {"item": id, "revision": n}
-- objects, the form of deliveries.item_revisions.
--
-- content_before is also NULL when the forget rule kept the old bytes out: then
-- revisions_before is not empty. When the edit created the file, revisions_before is [].
ALTER TABLE applied_edits ADD COLUMN revisions_before TEXT NOT NULL DEFAULT '[]'
	CHECK (json_type(revisions_before) = 'array');
ALTER TABLE applied_edits ADD COLUMN revisions_after TEXT NOT NULL DEFAULT '[]'
	CHECK (json_type(revisions_after) = 'array');
