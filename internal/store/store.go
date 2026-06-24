// Package store is the persistence layer (SQLite via the pure-Go modernc driver,
// so no CGO/gcc is needed on Windows). It holds wallet metadata + sealed private
// keys, RPC endpoints, and vault init params. Plaintext keys are never stored.
package store

import (
	"database/sql"
	"errors"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct{ db *sql.DB }

// Wallet is a managed account. EncPrivKey is hex(nonce||ciphertext) from the vault.
type Wallet struct {
	ID         int64     `json:"id"`
	Label      string    `json:"label"`
	Network    string    `json:"network"` // evm | solana | bitcoin
	Address    string    `json:"address"`
	EncPrivKey string    `json:"-"` // never serialized to the client
	GroupName  string    `json:"group"`
	ProxyURL   string    `json:"proxyUrl,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
}

// RPCEndpoint is one node URL within a chain group.
type RPCEndpoint struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	ChainID   int    `json:"chainId"`
	URL       string `json:"url"`
	GroupName string `json:"group"`
}

// Proxy is one proxy URL within a group (used for OpenSea poll/snipe, not mint sends).
type Proxy struct {
	ID        int64  `json:"id"`
	URL       string `json:"url"` // http(s)://[user:pass@]host:port or socks5://...
	GroupName string `json:"group"`
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // sqlite: single writer avoids "database is locked"
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS wallets (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  label        TEXT NOT NULL DEFAULT '',
  network      TEXT NOT NULL DEFAULT 'evm',
  address      TEXT NOT NULL,
  enc_privkey  TEXT NOT NULL,
  group_name   TEXT NOT NULL DEFAULT 'default',
  proxy_url    TEXT NOT NULL DEFAULT '',
  created_at   INTEGER NOT NULL,
  UNIQUE(network, address)
);
CREATE TABLE IF NOT EXISTS rpc_endpoints (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  name        TEXT NOT NULL DEFAULT '',
  chain_id    INTEGER NOT NULL,
  url         TEXT NOT NULL,
  group_name  TEXT NOT NULL DEFAULT 'main'
);
CREATE TABLE IF NOT EXISTS proxies (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  url         TEXT NOT NULL,
  group_name  TEXT NOT NULL DEFAULT 'default'
);`
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}
	if err := s.migrateTasks(); err != nil {
		return err
	}
	return s.migrateActivity()
}

// --- settings (used to persist vault salt/verifier) ---

var ErrNotFound = errors.New("not found")

func (s *Store) GetSetting(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return v, err
}

func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(
		`INSERT INTO settings(key,value) VALUES(?,?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// --- wallets ---

func (s *Store) AddWallet(w Wallet) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO wallets(label,network,address,enc_privkey,group_name,proxy_url,created_at)
		 VALUES(?,?,?,?,?,?,?)`,
		w.Label, w.Network, w.Address, w.EncPrivKey, w.GroupName, w.ProxyURL, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) ListWallets() ([]Wallet, error) {
	rows, err := s.db.Query(
		`SELECT id,label,network,address,enc_privkey,group_name,proxy_url,created_at
		 FROM wallets ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Wallet
	for rows.Next() {
		var w Wallet
		var ts int64
		if err := rows.Scan(&w.ID, &w.Label, &w.Network, &w.Address, &w.EncPrivKey,
			&w.GroupName, &w.ProxyURL, &ts); err != nil {
			return nil, err
		}
		w.CreatedAt = time.Unix(ts, 0)
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *Store) GetWallet(id int64) (Wallet, error) {
	var w Wallet
	var ts int64
	err := s.db.QueryRow(
		`SELECT id,label,network,address,enc_privkey,group_name,proxy_url,created_at
		 FROM wallets WHERE id=?`, id).
		Scan(&w.ID, &w.Label, &w.Network, &w.Address, &w.EncPrivKey, &w.GroupName, &w.ProxyURL, &ts)
	if errors.Is(err, sql.ErrNoRows) {
		return w, ErrNotFound
	}
	w.CreatedAt = time.Unix(ts, 0)
	return w, err
}

func (s *Store) DeleteWallet(id int64) error {
	_, err := s.db.Exec(`DELETE FROM wallets WHERE id=?`, id)
	return err
}

// --- rpc endpoints ---

func (s *Store) AddRPC(e RPCEndpoint) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO rpc_endpoints(name,chain_id,url,group_name) VALUES(?,?,?,?)`,
		e.Name, e.ChainID, e.URL, e.GroupName)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) ListRPC() ([]RPCEndpoint, error) {
	rows, err := s.db.Query(`SELECT id,name,chain_id,url,group_name FROM rpc_endpoints ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RPCEndpoint
	for rows.Next() {
		var e RPCEndpoint
		if err := rows.Scan(&e.ID, &e.Name, &e.ChainID, &e.URL, &e.GroupName); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) ListRPCByChain(chainID int) ([]RPCEndpoint, error) {
	rows, err := s.db.Query(
		`SELECT id,name,chain_id,url,group_name FROM rpc_endpoints WHERE chain_id=? ORDER BY id`, chainID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RPCEndpoint
	for rows.Next() {
		var e RPCEndpoint
		if err := rows.Scan(&e.ID, &e.Name, &e.ChainID, &e.URL, &e.GroupName); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) DeleteRPC(id int64) error {
	_, err := s.db.Exec(`DELETE FROM rpc_endpoints WHERE id=?`, id)
	return err
}

// --- proxies ---

func (s *Store) AddProxy(p Proxy) (int64, error) {
	if p.GroupName == "" {
		p.GroupName = "default"
	}
	res, err := s.db.Exec(`INSERT INTO proxies(url,group_name) VALUES(?,?)`, p.URL, p.GroupName)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) ListProxies() ([]Proxy, error) {
	rows, err := s.db.Query(`SELECT id,url,group_name FROM proxies ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Proxy
	for rows.Next() {
		var p Proxy
		if err := rows.Scan(&p.ID, &p.URL, &p.GroupName); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) ListProxiesByGroup(group string) ([]Proxy, error) {
	rows, err := s.db.Query(`SELECT id,url,group_name FROM proxies WHERE group_name=? ORDER BY id`, group)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Proxy
	for rows.Next() {
		var p Proxy
		if err := rows.Scan(&p.ID, &p.URL, &p.GroupName); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) DeleteProxy(id int64) error {
	_, err := s.db.Exec(`DELETE FROM proxies WHERE id=?`, id)
	return err
}
