package store

import "strings"

// wl_sessions persists OpenSea SIWE access tokens per wallet so the whitelist checker doesn't
// re-sign every run (the tokens survive app restarts). Tokens are short-lived JWTs; `exp` is
// their unix expiry. They are session tokens, not keys — wallet private keys are never stored
// here.
func (s *Store) migrateWL() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS wl_sessions (
  address TEXT PRIMARY KEY,
  token   TEXT NOT NULL,
  exp     INTEGER NOT NULL
);`)
	return err
}

// SaveWLSession upserts a wallet's session token + expiry.
func (s *Store) SaveWLSession(address, token string, exp int64) error {
	_, err := s.db.Exec(
		`INSERT INTO wl_sessions(address,token,exp) VALUES(?,?,?)
		 ON CONFLICT(address) DO UPDATE SET token=excluded.token, exp=excluded.exp`,
		strings.ToLower(address), token, exp)
	return err
}

// GetWLSession returns a wallet's stored token + expiry (ok=false if none).
func (s *Store) GetWLSession(address string) (string, int64, bool) {
	var tok string
	var exp int64
	if err := s.db.QueryRow(`SELECT token,exp FROM wl_sessions WHERE address=?`, strings.ToLower(address)).Scan(&tok, &exp); err != nil {
		return "", 0, false
	}
	return tok, exp, true
}

// DeleteWLSession drops a wallet's stored token (on rejection/expiry).
func (s *Store) DeleteWLSession(address string) {
	_, _ = s.db.Exec(`DELETE FROM wl_sessions WHERE address=?`, strings.ToLower(address))
}

// PruneWLSessions removes all tokens that expired at/before nowUnix.
func (s *Store) PruneWLSessions(nowUnix int64) {
	_, _ = s.db.Exec(`DELETE FROM wl_sessions WHERE exp <= ?`, nowUnix)
}
