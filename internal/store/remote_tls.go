package store

import "database/sql"

// RemoteTLS is the instance's self-signed TLS certificate row (single-row
// table; plan 2026-11-24-remote-tls-v1). The API layer decides what to
// expose: fingerprint/SANs/expiry are public-ish admin info, the key never
// leaves the server, and the cert PEM is offered only in the generate
// response (for optional install behind an external proxy).
type RemoteTLS struct {
	CertPEM     string `json:"-"`
	KeyPEM      string `json:"-"`
	Fingerprint string `json:"fingerprint"`
	SANs        string `json:"sans"` // comma-separated host list
	NotAfter    string `json:"not_after"`
	CreatedAt   string `json:"created_at"`
}

// RemoteTLSGet returns the current certificate row, ErrNotFound when the
// instance has not generated one (TLS listener then fails handshakes until
// a cert exists — or, in a proxied deployment, is simply not used).
func RemoteTLSGet(db *sql.DB) (*RemoteTLS, error) {
	var t RemoteTLS
	err := db.QueryRow(`SELECT cert_pem, key_pem, fingerprint, sans, not_after, created_at
		FROM remote_tls WHERE id=1`).
		Scan(&t.CertPEM, &t.KeyPEM, &t.Fingerprint, &t.SANs, &t.NotAfter, &t.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// RemoteTLSPut replaces the certificate atomically (regenerate = one write:
// no window where the row is missing and handshakes would fail).
func RemoteTLSPut(db *sql.DB, t *RemoteTLS) error {
	_, err := db.Exec(`INSERT INTO remote_tls (id, cert_pem, key_pem, fingerprint, sans, not_after, created_at)
		VALUES (1, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET cert_pem=excluded.cert_pem, key_pem=excluded.key_pem,
			fingerprint=excluded.fingerprint, sans=excluded.sans,
			not_after=excluded.not_after, created_at=excluded.created_at`,
		t.CertPEM, t.KeyPEM, t.Fingerprint, t.SANs, t.NotAfter, t.CreatedAt)
	return err
}

// RemoteTLSDelete removes the certificate (new handshakes fail until a new
// one is generated; live channels and established TLS sessions are
// unaffected — the listener's GetCertificate reads per handshake).
func RemoteTLSDelete(db *sql.DB) error {
	_, err := db.Exec(`DELETE FROM remote_tls WHERE id=1`)
	return err
}
