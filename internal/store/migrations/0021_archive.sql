-- The keep time of an archive candidate (E16-S7): brain archive --keep sets it, and a new
-- 90-day period starts there. NULL when you never kept the item.
ALTER TABLE items ADD COLUMN kept_at TEXT;
