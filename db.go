package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

// Two pools on the same SQLite file (WAL mode):
//   - rdb: many parallel readers
//   - wdb: exactly ONE writer connection, so writes queue in Go instead of
//     failing with "database is locked" under heavy load.
type DB struct {
	rdb *sql.DB
	wdb *sql.DB
}

// Querier is satisfied by *sql.DB and *sql.Tx so helpers work in both.
type Querier interface {
	Exec(q string, args ...any) (sql.Result, error)
	Query(q string, args ...any) (*sql.Rows, error)
	QueryRow(q string, args ...any) *sql.Row
}

const schema = `
CREATE TABLE IF NOT EXISTS users(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  username TEXT NOT NULL UNIQUE COLLATE NOCASE,
  name TEXT NOT NULL,
  email TEXT NOT NULL DEFAULT '',
  phone TEXT NOT NULL DEFAULT '',
  role TEXT NOT NULL,
  branch_id INTEGER NOT NULL DEFAULT 0,
  pass_hash TEXT NOT NULL,
  active INTEGER NOT NULL DEFAULT 1,
  totp_secret TEXT NOT NULL DEFAULT '',
  totp_on INTEGER NOT NULL DEFAULT 0,
  lang TEXT NOT NULL DEFAULT 'en',
  created_at INTEGER NOT NULL,
  last_login INTEGER NOT NULL DEFAULT 0,
  revoked_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS roles(
  key TEXT PRIMARY KEY,
  label TEXT NOT NULL,
  perms TEXT NOT NULL,
  builtin INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS resets(
  token TEXT PRIMARY KEY,
  user_id INTEGER NOT NULL,
  exp INTEGER NOT NULL
);

-- every "master data / document" resource lives here (see resource.go)
CREATE TABLE IF NOT EXISTS records(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  res TEXT NOT NULL,
  branch_id INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT '',
  ref TEXT NOT NULL DEFAULT '',
  data TEXT NOT NULL,
  search TEXT NOT NULL DEFAULT '',
  created_by INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_rec_res ON records(res, branch_id, id);
CREATE INDEX IF NOT EXISTS idx_rec_status ON records(res, status, id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_rec_ref ON records(res, ref) WHERE ref<>'';

CREATE TABLE IF NOT EXISTS orders(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  branch_id INTEGER NOT NULL,
  no TEXT NOT NULL DEFAULT '',
  channel TEXT NOT NULL,            -- dine_in takeaway delivery pickup drive_thru pre_order catering
  source TEXT NOT NULL DEFAULT 'pos',
  status TEXT NOT NULL,             -- held pending preparing ready served completed cancelled refunded
  pay_status TEXT NOT NULL DEFAULT 'unpaid',  -- unpaid partial paid refunded
  table_id INTEGER NOT NULL DEFAULT 0,
  table_name TEXT NOT NULL DEFAULT '',
  customer_id INTEGER NOT NULL DEFAULT 0,
  customer TEXT NOT NULL DEFAULT '',
  phone TEXT NOT NULL DEFAULT '',
  address TEXT NOT NULL DEFAULT '',
  waiter_id INTEGER NOT NULL DEFAULT 0,
  session_id INTEGER NOT NULL DEFAULT 0,
  subtotal INTEGER NOT NULL DEFAULT 0,
  discount INTEGER NOT NULL DEFAULT 0,
  voucher TEXT NOT NULL DEFAULT '',
  disc_type TEXT NOT NULL DEFAULT '',   -- percent | fixed | '' (what the cashier entered)
  disc_value REAL NOT NULL DEFAULT 0,
  service INTEGER NOT NULL DEFAULT 0,
  tax INTEGER NOT NULL DEFAULT 0,
  tip INTEGER NOT NULL DEFAULT 0,
  delivery_fee INTEGER NOT NULL DEFAULT 0,
  total INTEGER NOT NULL DEFAULT 0,
  paid INTEGER NOT NULL DEFAULT 0,
  cost INTEGER NOT NULL DEFAULT 0,
  points_used INTEGER NOT NULL DEFAULT 0,
  note TEXT NOT NULL DEFAULT '',
  reason TEXT NOT NULL DEFAULT '',
  pay_mode TEXT NOT NULL DEFAULT '',   -- how the guest says they will pay: cash card upi ...
  scheduled_at INTEGER NOT NULL DEFAULT 0,
  created_by INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_orders_branch ON orders(branch_id, id);
CREATE INDEX IF NOT EXISTS idx_orders_status ON orders(status, id);
CREATE INDEX IF NOT EXISTS idx_orders_bcreated ON orders(branch_id, created_at);
CREATE INDEX IF NOT EXISTS idx_orders_created ON orders(created_at);
CREATE INDEX IF NOT EXISTS idx_orders_table ON orders(table_id, status);
CREATE TABLE IF NOT EXISTS order_items(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  order_id INTEGER NOT NULL,
  product_id INTEGER NOT NULL,
  name TEXT NOT NULL,
  category TEXT NOT NULL DEFAULT '',
  station TEXT NOT NULL DEFAULT '',
  price INTEGER NOT NULL,           -- unit price incl. options
  tax_pct REAL NOT NULL DEFAULT 0,
  qty INTEGER NOT NULL,
  cost INTEGER NOT NULL DEFAULT 0,  -- unit cost snapshot (recipe)
  options TEXT NOT NULL DEFAULT '',
  note TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'pending'  -- pending preparing ready served void
);
CREATE INDEX IF NOT EXISTS idx_items_order ON order_items(order_id);
CREATE INDEX IF NOT EXISTS idx_items_product ON order_items(product_id);
CREATE TABLE IF NOT EXISTS payments(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  order_id INTEGER NOT NULL,
  branch_id INTEGER NOT NULL,
  session_id INTEGER NOT NULL DEFAULT 0,
  method TEXT NOT NULL,             -- cash card upi wallet gift_card loyalty
  kind TEXT NOT NULL DEFAULT 'payment',  -- payment refund
  amount INTEGER NOT NULL,
  ref TEXT NOT NULL DEFAULT '',
  created_by INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_pay_order ON payments(order_id);
CREATE INDEX IF NOT EXISTS idx_pay_created ON payments(created_at);

CREATE TABLE IF NOT EXISTS ingredients_stock(
  branch_id INTEGER NOT NULL,
  ingredient_id INTEGER NOT NULL,
  qty REAL NOT NULL DEFAULT 0,
  PRIMARY KEY(branch_id, ingredient_id)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS idx_tbl_qr ON records(json_extract(data,'$.qr_token')) WHERE res='tables';
CREATE INDEX IF NOT EXISTS idx_rec_track ON records(json_extract(data,'$.tracking')) WHERE res='deliveries';
CREATE TABLE IF NOT EXISTS stock_moves(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  branch_id INTEGER NOT NULL,
  ingredient_id INTEGER NOT NULL,
  qty REAL NOT NULL,                -- signed
  unit_cost INTEGER NOT NULL DEFAULT 0,
  kind TEXT NOT NULL,               -- purchase sale transfer_in transfer_out adjust count waste return
  ref TEXT NOT NULL DEFAULT '',
  note TEXT NOT NULL DEFAULT '',
  created_by INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_moves_ing ON stock_moves(ingredient_id, id);
CREATE INDEX IF NOT EXISTS idx_moves_branch ON stock_moves(branch_id, id);

CREATE TABLE IF NOT EXISTS accounts(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  code TEXT NOT NULL UNIQUE,
  name TEXT NOT NULL,
  type TEXT NOT NULL,               -- asset liability equity revenue cogs expense
  parent_id INTEGER NOT NULL DEFAULT 0,
  is_cash INTEGER NOT NULL DEFAULT 0,
  active INTEGER NOT NULL DEFAULT 1,
  sys TEXT NOT NULL DEFAULT ''      -- system key used by auto-posting (sales, tax, cash ...)
);
CREATE TABLE IF NOT EXISTS journals(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  branch_id INTEGER NOT NULL DEFAULT 0,
  date TEXT NOT NULL,
  ref TEXT NOT NULL DEFAULT '',
  memo TEXT NOT NULL DEFAULT '',
  source TEXT NOT NULL DEFAULT 'manual',   -- manual order:12 purchase:3 payroll:5 ...
  status TEXT NOT NULL DEFAULT 'posted',   -- draft posted reversed
  created_by INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_journals_date ON journals(date);
CREATE INDEX IF NOT EXISTS idx_journals_source ON journals(source);
CREATE TABLE IF NOT EXISTS journal_lines(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  journal_id INTEGER NOT NULL,
  branch_id INTEGER NOT NULL DEFAULT 0,
  account_id INTEGER NOT NULL,
  debit INTEGER NOT NULL DEFAULT 0,
  credit INTEGER NOT NULL DEFAULT 0,
  date TEXT NOT NULL,
  memo TEXT NOT NULL DEFAULT '',
  dim TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_jl_acc ON journal_lines(account_id, date);
CREATE INDEX IF NOT EXISTS idx_jl_journal ON journal_lines(journal_id);

CREATE TABLE IF NOT EXISTS attendance(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  employee_id INTEGER NOT NULL,
  branch_id INTEGER NOT NULL DEFAULT 0,
  date TEXT NOT NULL,
  check_in INTEGER NOT NULL DEFAULT 0,
  check_out INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT 'present',  -- present absent late leave holiday half_day
  late_min INTEGER NOT NULL DEFAULT 0,
  worked_min INTEGER NOT NULL DEFAULT 0,
  ot_min INTEGER NOT NULL DEFAULT 0,
  source TEXT NOT NULL DEFAULT 'manual',
  lat REAL NOT NULL DEFAULT 0,
  lng REAL NOT NULL DEFAULT 0,
  note TEXT NOT NULL DEFAULT '',
  UNIQUE(employee_id, date)
);
CREATE INDEX IF NOT EXISTS idx_att_date ON attendance(date, branch_id);
CREATE TABLE IF NOT EXISTS leave_balances(
  employee_id INTEGER NOT NULL,
  leave_type_id INTEGER NOT NULL,
  year INTEGER NOT NULL,
  entitled REAL NOT NULL DEFAULT 0,
  used REAL NOT NULL DEFAULT 0,
  PRIMARY KEY(employee_id, leave_type_id, year)
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS audit_log(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  ts INTEGER NOT NULL,
  user_id INTEGER NOT NULL DEFAULT 0,
  user TEXT NOT NULL DEFAULT '',
  role TEXT NOT NULL DEFAULT '',
  branch_id INTEGER NOT NULL DEFAULT 0,
  res TEXT NOT NULL DEFAULT '',
  rec_id INTEGER NOT NULL DEFAULT 0,
  action TEXT NOT NULL,
  detail TEXT NOT NULL DEFAULT '',
  ip TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_audit_ts ON audit_log(ts);
CREATE TABLE IF NOT EXISTS auth_log(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  ts INTEGER NOT NULL,
  username TEXT NOT NULL DEFAULT '',
  user_id INTEGER NOT NULL DEFAULT 0,
  ok INTEGER NOT NULL DEFAULT 0,
  ip TEXT NOT NULL DEFAULT '',
  agent TEXT NOT NULL DEFAULT '',
  note TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS notifications(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  ts INTEGER NOT NULL,
  user_id INTEGER NOT NULL DEFAULT 0,   -- 0 = broadcast to role/branch
  role TEXT NOT NULL DEFAULT '',        -- '' = everyone
  branch_id INTEGER NOT NULL DEFAULT 0,
  level TEXT NOT NULL DEFAULT 'info',
  title TEXT NOT NULL,
  body TEXT NOT NULL DEFAULT '',
  link TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS settings(
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS files(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL,
  mime TEXT NOT NULL,
  size INTEGER NOT NULL,
  path TEXT NOT NULL,
  created_by INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);
`

func openDB(path string) (*DB, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(0)&_pragma=cache_size(-16000)", filepath.ToSlash(path))
	w, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	r, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	r.SetMaxOpenConns(32)
	db := &DB{rdb: r, wdb: w}
	if _, err := w.Exec(schema); err != nil {
		return nil, err
	}
	w.Exec(`ALTER TABLE orders ADD COLUMN pay_mode TEXT NOT NULL DEFAULT ''`)    // older databases
	w.Exec(`ALTER TABLE users ADD COLUMN revoked_at INTEGER NOT NULL DEFAULT 0`) // older databases (error = already there)
	return db, nil
}

func hashPassword(p string) string {
	h, _ := bcrypt.GenerateFromPassword([]byte(p), 10)
	return string(h)
}

// tx runs fn inside a write transaction (serialised through the single writer).
func (a *App) tx(fn func(tx *sql.Tx) error) error {
	tx, err := a.db.wdb.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}
