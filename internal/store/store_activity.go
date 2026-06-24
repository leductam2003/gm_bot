package store

import (
	"strings"
	"time"
)

// Mint is one recorded on-chain mint attempt. It powers the Home activity log and the
// cost basis for realized PNL. Status is "minted" (mined OK) or "failed".
type Mint struct {
	ID       int64     `json:"id"`
	Ts       time.Time `json:"ts"`
	ChainID  int       `json:"chainId"`
	Contract string    `json:"contract"`
	TokenID  string    `json:"tokenId"`
	WalletID int64     `json:"walletId"`
	Address  string    `json:"address"`
	TxHash   string    `json:"txHash"`
	CostWei  string    `json:"costWei"`
	Status   string    `json:"status"`
	Sold     bool      `json:"sold"`
}

// Sale is one realized sale (we accepted an offer / a listing sold). Proceeds is the WETH
// received; CostWei is the matched mint cost captured at sale time, so realized PNL is
// just proceeds − cost.
type Sale struct {
	ID          int64     `json:"id"`
	Ts          time.Time `json:"ts"`
	ChainID     int       `json:"chainId"`
	Contract    string    `json:"contract"`
	Collection  string    `json:"collection"`
	TokenID     string    `json:"tokenId"`
	WalletID    int64     `json:"walletId"`
	Address     string    `json:"address"`
	TxHash      string    `json:"txHash"`
	ProceedsWei string    `json:"proceedsWei"`
	CostWei     string    `json:"costWei"`
}

func (s *Store) migrateActivity() error {
	const schema = `
CREATE TABLE IF NOT EXISTS mints (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  ts         INTEGER NOT NULL,
  chain_id   INTEGER NOT NULL,
  contract   TEXT NOT NULL DEFAULT '',
  token_id   TEXT NOT NULL DEFAULT '',
  wallet_id  INTEGER NOT NULL DEFAULT 0,
  address    TEXT NOT NULL DEFAULT '',
  tx_hash    TEXT NOT NULL DEFAULT '',
  cost_wei   TEXT NOT NULL DEFAULT '0',
  status     TEXT NOT NULL DEFAULT 'minted',
  sold       INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS sales (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  ts           INTEGER NOT NULL,
  chain_id     INTEGER NOT NULL,
  contract     TEXT NOT NULL DEFAULT '',
  collection   TEXT NOT NULL DEFAULT '',
  token_id     TEXT NOT NULL DEFAULT '',
  wallet_id    INTEGER NOT NULL DEFAULT 0,
  address      TEXT NOT NULL DEFAULT '',
  tx_hash      TEXT NOT NULL DEFAULT '',
  proceeds_wei TEXT NOT NULL DEFAULT '0',
  cost_wei     TEXT NOT NULL DEFAULT '0'
);`
	_, err := s.db.Exec(schema)
	return err
}

// AddMint records one mint attempt (success or failure).
func (s *Store) AddMint(m Mint) (int64, error) {
	if m.CostWei == "" {
		m.CostWei = "0"
	}
	if m.Status == "" {
		m.Status = "minted"
	}
	res, err := s.db.Exec(
		`INSERT INTO mints(ts,chain_id,contract,token_id,wallet_id,address,tx_hash,cost_wei,status,sold)
		 VALUES(?,?,?,?,?,?,?,?,?,0)`,
		m.Ts.Unix(), m.ChainID, strings.ToLower(m.Contract), m.TokenID, m.WalletID, m.Address, m.TxHash, m.CostWei, m.Status)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// CountMints returns how many mint rows have the given status ("minted"|"failed").
func (s *Store) CountMints(status string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM mints WHERE status=?`, status).Scan(&n)
	return n, err
}

// RecentMints returns the most recent successful mints, newest first.
func (s *Store) RecentMints(limit int) ([]Mint, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(
		`SELECT id,ts,chain_id,contract,token_id,wallet_id,address,tx_hash,cost_wei,status,sold
		 FROM mints WHERE status='minted' ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Mint
	for rows.Next() {
		var m Mint
		var ts int64
		var sold int
		if err := rows.Scan(&m.ID, &ts, &m.ChainID, &m.Contract, &m.TokenID, &m.WalletID, &m.Address, &m.TxHash, &m.CostWei, &m.Status, &sold); err != nil {
			return nil, err
		}
		m.Ts = time.Unix(ts, 0)
		m.Sold = sold != 0
		out = append(out, m)
	}
	return out, rows.Err()
}

// MatchMintCost finds the cost basis of a sold token (the newest unsold matching mint),
// marks that mint sold, and returns its cost in wei ("0" if no match is found).
func (s *Store) MatchMintCost(chainID int, contract, tokenID, address string) string {
	var id int64
	var cost string
	err := s.db.QueryRow(
		`SELECT id,cost_wei FROM mints
		 WHERE status='minted' AND sold=0 AND chain_id=? AND contract=? AND token_id=? AND lower(address)=lower(?)
		 ORDER BY id DESC LIMIT 1`,
		chainID, strings.ToLower(contract), tokenID, address).Scan(&id, &cost)
	if err != nil {
		return "0"
	}
	_, _ = s.db.Exec(`UPDATE mints SET sold=1 WHERE id=?`, id)
	if cost == "" {
		return "0"
	}
	return cost
}

// AddSale records one realized sale.
func (s *Store) AddSale(sale Sale) (int64, error) {
	if sale.ProceedsWei == "" {
		sale.ProceedsWei = "0"
	}
	if sale.CostWei == "" {
		sale.CostWei = "0"
	}
	res, err := s.db.Exec(
		`INSERT INTO sales(ts,chain_id,contract,collection,token_id,wallet_id,address,tx_hash,proceeds_wei,cost_wei)
		 VALUES(?,?,?,?,?,?,?,?,?,?)`,
		sale.Ts.Unix(), sale.ChainID, strings.ToLower(sale.Contract), sale.Collection, sale.TokenID,
		sale.WalletID, sale.Address, sale.TxHash, sale.ProceedsWei, sale.CostWei)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// AllSales returns every recorded sale (realized PNL is aggregated in big.Int by the API,
// since wei values exceed int64 and SQLite can't SUM them reliably as text).
func (s *Store) AllSales() ([]Sale, error) {
	rows, err := s.db.Query(
		`SELECT id,ts,chain_id,contract,collection,token_id,wallet_id,address,tx_hash,proceeds_wei,cost_wei
		 FROM sales ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Sale
	for rows.Next() {
		var sl Sale
		var ts int64
		if err := rows.Scan(&sl.ID, &ts, &sl.ChainID, &sl.Contract, &sl.Collection, &sl.TokenID, &sl.WalletID, &sl.Address, &sl.TxHash, &sl.ProceedsWei, &sl.CostWei); err != nil {
			return nil, err
		}
		sl.Ts = time.Unix(ts, 0)
		out = append(out, sl)
	}
	return out, rows.Err()
}

// ResetActivity clears all recorded mints and sales (the Home "Reset" button).
func (s *Store) ResetActivity() error {
	if _, err := s.db.Exec(`DELETE FROM mints`); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM sales`)
	return err
}
