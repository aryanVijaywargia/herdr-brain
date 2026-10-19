-- The anchors of a dispute (E16-S6): the paths of the item anchors that changed, moved or
-- were deleted, as a JSON array. An empty array is a proposal of another kind. A pending
-- proposal with anchors is the review of a disputed item; the brief marks the item with its
-- anchors and the date of the proposal.
ALTER TABLE proposals ADD COLUMN dispute TEXT NOT NULL DEFAULT '[]' CHECK (json_type(dispute) = 'array');
