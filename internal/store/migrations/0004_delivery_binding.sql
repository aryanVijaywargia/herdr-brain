-- The store binding of a receipt (E7-S2): the herdr session of the binding that
-- selected this store. All real herdr sessions share the main store, so the pane id alone
-- does not say which session a receipt is for. It is NULL on the receipts of older stores.
ALTER TABLE deliveries ADD COLUMN herdr_session TEXT;
