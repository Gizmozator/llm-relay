import type { UsageRow } from '../api';

/** Which label the Usage detail Key cell shows for one row: the key's own
 *  name, "deleted key", or the federation marker.
 *
 *  The origin is decided by whether the row ever had a LOCAL key
 *  (key_local) — never by the username. Spoke-origin rows on the hub carry
 *  the stamped hub-side identity (a real username) with the synthetic
 *  federation key (api_key_id 0), so an empty key_name on them means
 *  "federation", not "deleted key" (ticket #15). A deleted key is the
 *  remaining case: the row referenced a local key whose row is gone. */
export function keyOrigin(
  r: Pick<UsageRow, 'key_name' | 'key_local' | 'username'>,
): 'name' | 'deleted' | 'federation' {
  if (r.key_name) return 'name';
  return r.key_local ? 'deleted' : 'federation';
}
