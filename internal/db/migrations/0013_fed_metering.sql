-- 0013: federation metering/attribution (plan 2026-11-22-relay-federation,
-- task 4).
--
-- Double-entry bookkeeping for federation traffic (tracking, not billing):
--   fed_direction  '' = plain local traffic | 'served' (this relay's
--                  capacity went to a peer) | 'consumed' (this relay's
--                  account used a peer's capacity).
--   fed_peer_id    the counterparty peer (NULL = not applicable: a spoke
--                  served row has no local peer row for the hub).
--   fed_user       the hub-stamped hub-side identity name a serving spoke
--                  sees (spoke-local usernames never leave the spoke).
--
-- Plain ADD COLUMN with defaults: existing rows are untouched and read
-- back as plain local traffic.
ALTER TABLE usage_log ADD COLUMN fed_direction TEXT NOT NULL DEFAULT '';
ALTER TABLE usage_log ADD COLUMN fed_peer_id INTEGER;
ALTER TABLE usage_log ADD COLUMN fed_user TEXT NOT NULL DEFAULT '';
