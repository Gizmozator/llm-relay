-- 0014: remember a model's visibility from before it joined its group.
--
-- Membership hides a model (visibility='internal') and leaving the group
-- used to restore a blanket 'public' — which un-hid models an admin had
-- hidden by hand before the join. The membership row now records the
-- pre-join value and the restore puts that back.
--
-- Existing rows get 'public': the blanket rule always restored public so
-- far, so this keeps the current behaviour for rows already membered.
ALTER TABLE model_group_members ADD COLUMN prev_visibility TEXT NOT NULL DEFAULT 'public';
