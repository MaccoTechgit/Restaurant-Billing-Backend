package main

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"
)

// =====================================================================
//  Double-entry ledger.
//  Every sale, payment, purchase, stock write-off and payroll run is
//  turned into a balanced journal entry (debits == credits) automatically.
// =====================================================================

type acctRow struct {
	ID     int64
	Code   string
	Name   string
	Type   string
	Parent int64
	Cash   bool
	Active bool
	Sys    string
}

var defaultChart = []acctRow{
	{0, "1000", "Cash on Hand", "asset", 0, true, true, "cash"},
	{0, "1010", "Bank Account", "asset", 0, true, true, "bank"},
	{0, "1020", "Card Settlements Receivable", "asset", 0, false, true, "card_clearing"},
	{0, "1100", "Accounts Receivable", "asset", 0, false, true, "ar"},
	{0, "1200", "Inventory", "asset", 0, false, true, "inventory"},
	{0, "1300", "Prepaid Expenses", "asset", 0, false, true, "prepaid"},
	{0, "1400", "Input Tax Receivable", "asset", 0, false, true, "tax_input"},
	{0, "1500", "Fixed Assets", "asset", 0, false, true, "fixed_assets"},
	{0, "1590", "Accumulated Depreciation", "asset", 0, false, true, "accum_dep"},
	{0, "1600", "Petty Cash", "asset", 0, true, true, "petty_cash"},
	{0, "2000", "Accounts Payable", "liability", 0, false, true, "ap"},
	{0, "2100", "Output Tax Payable", "liability", 0, false, true, "tax_payable"},
	{0, "2200", "Gift Card Liability", "liability", 0, false, true, "gift_liability"},
	{0, "2300", "Payroll Payable", "liability", 0, false, true, "payroll_payable"},
	{0, "2310", "Social Security Payable", "liability", 0, false, true, "social_payable"},
	{0, "2320", "Income Tax Withheld Payable", "liability", 0, false, true, "wht_payable"},
	{0, "2400", "Loyalty Points Liability", "liability", 0, false, true, "loyalty_liability"},
	{0, "2500", "Accrued Expenses", "liability", 0, false, true, "accrued"},
	{0, "2600", "Tips Payable", "liability", 0, false, true, "tips_payable"},
	{0, "2700", "Driver Payable", "liability", 0, false, true, "driver_payable"},
	{0, "2800", "Customer Deposits", "liability", 0, false, true, "deposits"},
	{0, "3000", "Owner's Equity", "equity", 0, false, true, "equity"},
	{0, "3100", "Retained Earnings", "equity", 0, false, true, "retained"},
	{0, "4000", "Food & Beverage Sales", "revenue", 0, false, true, "sales"},
	{0, "4010", "Delivery Income", "revenue", 0, false, true, "delivery_income"},
	{0, "4020", "Service Charge Income", "revenue", 0, false, true, "service_income"},
	{0, "4030", "Gift Card Breakage Income", "revenue", 0, false, true, "breakage"},
	{0, "4040", "Other Income", "revenue", 0, false, true, "other_income"},
	{0, "4900", "Sales Discounts", "revenue", 0, false, true, "discounts"},
	{0, "5000", "Cost of Goods Sold", "cogs", 0, false, true, "cogs"},
	{0, "5010", "Waste & Spoilage", "expense", 0, false, true, "waste_expense"},
	{0, "5020", "Inventory Adjustments", "expense", 0, false, true, "inv_adjust"},
	{0, "6000", "Salaries & Wages", "expense", 0, false, true, "payroll_expense"},
	{0, "6010", "Employer Social Security", "expense", 0, false, true, "social_expense"},
	{0, "6020", "Gratuity Expense", "expense", 0, false, true, "gratuity_expense"},
	{0, "6100", "Rent", "expense", 0, false, true, "rent"},
	{0, "6110", "Utilities", "expense", 0, false, true, "utilities"},
	{0, "6120", "Marketing", "expense", 0, false, true, "marketing"},
	{0, "6130", "Repairs & Maintenance", "expense", 0, false, true, "repairs"},
	{0, "6140", "Bank & Card Fees", "expense", 0, false, true, "bank_fees"},
	{0, "6150", "Delivery & Driver Costs", "expense", 0, false, true, "driver_expense"},
	{0, "6160", "Depreciation Expense", "expense", 0, false, true, "depreciation"},
	{0, "6170", "General & Administrative", "expense", 0, false, true, "general_expense"},
	{0, "6180", "Loyalty Rewards Expense", "expense", 0, false, true, "loyalty_expense"},
	{0, "6190", "Cash Over / Short", "expense", 0, false, true, "cash_over_short"},
	{0, "6200", "Foreign Exchange Difference", "expense", 0, false, true, "fx_diff"},
}

type chart struct {
	mu    sync.RWMutex
	byKey map[string]int64
	byID  map[int64]acctRow
}

func (a *App) loadChart() {
	c := &chart{byKey: map[string]int64{}, byID: map[int64]acctRow{}}
	rows, err := a.db.rdb.Query(`SELECT id,code,name,type,parent_id,is_cash,active,sys FROM accounts ORDER BY code`)
	if err == nil {
		for rows.Next() {
			var r acctRow
			rows.Scan(&r.ID, &r.Code, &r.Name, &r.Type, &r.Parent, &r.Cash, &r.Active, &r.Sys)
			c.byID[r.ID] = r
			c.byKey[r.Code] = r.ID
			if r.Sys != "" {
				c.byKey[r.Sys] = r.ID
			}
		}
		rows.Close()
	}
	a.chart.Store(c)
}

func (a *App) acct(key string) (int64, bool) {
	c := a.chart.Load()
	if c == nil {
		return 0, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	id, ok := c.byKey[key]
	return id, ok
}

func (a *App) acctInfo(id int64) acctRow {
	if c := a.chart.Load(); c != nil {
		c.mu.RLock()
		defer c.mu.RUnlock()
		return c.byID[id]
	}
	return acctRow{}
}

func (a *App) seedChart(tx Querier) {
	for _, r := range defaultChart {
		cash := 0
		if r.Cash {
			cash = 1
		}
		tx.Exec(`INSERT OR IGNORE INTO accounts(code,name,type,is_cash,active,sys) VALUES(?,?,?,?,1,?)`, r.Code, r.Name, r.Type, cash, r.Sys)
	}
}

// JLine is one side of a journal entry. Acct is a system key ("sales") or account code ("4000").
type JLine struct {
	Acct   string
	Dr, Cr int64
	Memo   string
}

func Dr(acct string, amt int64) JLine { return JLine{Acct: acct, Dr: amt} }
func Cr(acct string, amt int64) JLine { return JLine{Acct: acct, Cr: amt} }

func (a *App) autoPost() bool { return a.settingBool("accounting", "auto_post", true) }

// periodClosed reports whether the date falls in a closed accounting period.
func periodClosed(q Querier, date string) bool {
	var n int
	q.QueryRow(`SELECT COUNT(*) FROM records WHERE res='periods' AND status='closed'
		AND json_extract(data,'$.from')<=? AND json_extract(data,'$.to')>=?`, date, date).Scan(&n)
	return n > 0
}

// post writes a balanced journal entry. Zero-amount lines are skipped.
func (a *App) post(q Querier, branch int64, date, source, memo string, uid int64, lines ...JLine) (int64, error) {
	var dr, cr int64
	clean := make([]JLine, 0, len(lines))
	for _, l := range lines {
		if l.Dr == 0 && l.Cr == 0 {
			continue
		}
		if l.Dr < 0 || l.Cr < 0 { // a negative debit is a credit
			if l.Dr < 0 {
				l.Cr, l.Dr = -l.Dr, 0
			}
			if l.Cr < 0 {
				l.Dr, l.Cr = -l.Cr, 0
			}
		}
		dr += l.Dr
		cr += l.Cr
		clean = append(clean, l)
	}
	if len(clean) == 0 {
		return 0, nil
	}
	if dr != cr {
		return 0, uerr("journal is not balanced (debits %.2f, credits %.2f)", float64(dr)/100, float64(cr)/100)
	}
	if periodClosed(q, date) {
		return 0, uerr("the accounting period containing %s is closed", date)
	}
	ids := make([]int64, len(clean))
	for i, l := range clean {
		id, ok := a.acct(l.Acct)
		if !ok {
			return 0, uerr("account %q is missing from the chart of accounts", l.Acct)
		}
		ids[i] = id
	}
	res, err := q.Exec(`INSERT INTO journals(branch_id,date,ref,memo,source,status,created_by,created_at) VALUES(?,?,?,?,?,'posted',?,?)`,
		branch, date, "", trunc(memo, 200), source, uid, nowUnix())
	if err != nil {
		return 0, err
	}
	jid, _ := res.LastInsertId()
	q.Exec(`UPDATE journals SET ref=? WHERE id=?`, fmt.Sprintf("JE-%06d", jid), jid)
	for i, l := range clean {
		if _, err := q.Exec(`INSERT INTO journal_lines(journal_id,branch_id,account_id,debit,credit,date,memo) VALUES(?,?,?,?,?,?,?)`,
			jid, branch, ids[i], l.Dr, l.Cr, date, trunc(l.Memo, 200)); err != nil {
			return 0, err
		}
	}
	return jid, nil
}

// postSimple: one debit, one credit. No-op when auto-posting is off.
func (a *App) postSimple(q Querier, branch int64, date, source, memo, dr, cr string, amt, uid int64) error {
	if amt == 0 || !a.autoPost() {
		return nil
	}
	_, err := a.post(q, branch, date, source, memo, uid, Dr(dr, amt), Cr(cr, amt))
	return err
}

// reverseJournal posts the mirror image of a journal (used for refunds / voids).
func (a *App) reverseJournal(q Querier, jid int64, uid int64, memo string) error {
	rows, err := q.Query(`SELECT branch_id,date,account_id,debit,credit FROM journal_lines WHERE journal_id=?`, jid)
	if err != nil {
		return err
	}
	type ln struct {
		b, acc, dr, cr int64
		date           string
	}
	var ls []ln
	for rows.Next() {
		var l ln
		rows.Scan(&l.b, &l.date, &l.acc, &l.dr, &l.cr)
		ls = append(ls, l)
	}
	rows.Close()
	if len(ls) == 0 {
		return nil
	}
	res, err := q.Exec(`INSERT INTO journals(branch_id,date,memo,source,status,created_by,created_at) VALUES(?,?,?,?,'posted',?,?)`,
		ls[0].b, time.Now().Format("2006-01-02"), memo, fmt.Sprintf("reversal:%d", jid), uid, nowUnix())
	if err != nil {
		return err
	}
	nid, _ := res.LastInsertId()
	q.Exec(`UPDATE journals SET ref=? WHERE id=?`, fmt.Sprintf("JE-%06d", nid), nid)
	for _, l := range ls {
		q.Exec(`INSERT INTO journal_lines(journal_id,branch_id,account_id,debit,credit,date,memo) VALUES(?,?,?,?,?,?,?)`,
			nid, l.b, l.acc, l.cr, l.dr, time.Now().Format("2006-01-02"), "reversal")
	}
	q.Exec(`UPDATE journals SET status='reversed' WHERE id=?`, jid)
	return nil
}

// ---------- ledger queries (reports) ----------

type AccBal struct {
	ID     int64  `json:"id"`
	Code   string `json:"code"`
	Name   string `json:"name"`
	Type   string `json:"type"`
	Debit  int64  `json:"debit"`
	Credit int64  `json:"credit"`
	Bal    int64  `json:"balance"` // natural-side balance (assets/expenses = dr-cr, others cr-dr)
}

// balances returns one row per account for date range [from,to] ("" = open ended).
func (a *App) balances(q Querier, branch int64, from, to string) []AccBal {
	where, args := "1=1", []any{}
	if from != "" {
		where += " AND l.date>=?"
		args = append(args, from)
	}
	if to != "" {
		where += " AND l.date<=?"
		args = append(args, to)
	}
	if branch != 0 {
		where += " AND l.branch_id=?"
		args = append(args, branch)
	}
	rows, err := q.Query(`SELECT l.account_id,SUM(l.debit),SUM(l.credit) FROM journal_lines l
		JOIN journals j ON j.id=l.journal_id AND j.status<>'draft' WHERE `+where+` GROUP BY l.account_id`, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	sums := map[int64][2]int64{}
	for rows.Next() {
		var id, d, c int64
		rows.Scan(&id, &d, &c)
		sums[id] = [2]int64{d, c}
	}
	var out []AccBal
	if ch := a.chart.Load(); ch != nil {
		ch.mu.RLock()
		for id, r := range ch.byID {
			s := sums[id]
			b := AccBal{ID: id, Code: r.Code, Name: r.Name, Type: r.Type, Debit: s[0], Credit: s[1]}
			if r.Type == "asset" || r.Type == "expense" || r.Type == "cogs" {
				b.Bal = s[0] - s[1]
			} else {
				b.Bal = s[1] - s[0]
			}
			out = append(out, b)
		}
		ch.mu.RUnlock()
	}
	sortAcc(out)
	return out
}

func sortAcc(l []AccBal) {
	for i := 1; i < len(l); i++ {
		for j := i; j > 0 && strings.Compare(l[j].Code, l[j-1].Code) < 0; j-- {
			l[j], l[j-1] = l[j-1], l[j]
		}
	}
}

var _ = sql.ErrNoRows
