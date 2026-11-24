-- 0016: TLS for machine channels (plan 2026-11-24-remote-tls-v1): the
-- self-signed certificate the relay serves its TLS listener with, generated
-- from the dashboard and pinned by spokes/connectors via its fingerprint.
--
-- Single-row table (id=1 enforced): there is exactly one instance identity
-- certificate. The private key is plaintext-at-rest — it cannot be hashed,
-- and the DB file is already the secret boundary (hub_token lives there
-- under the same posture).
CREATE TABLE remote_tls (
	id          INTEGER PRIMARY KEY CHECK (id = 1),
	cert_pem    TEXT NOT NULL,
	key_pem     TEXT NOT NULL,
	fingerprint TEXT NOT NULL,
	sans        TEXT NOT NULL,
	not_after   TEXT NOT NULL,
	created_at  TEXT NOT NULL
);
