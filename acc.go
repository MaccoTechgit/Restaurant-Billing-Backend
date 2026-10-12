package main

import (
	"database/sql"
	"encoding/csv"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// =====================================================================
//  Accounting: chart of accounts, journals, financial statements,
//  dashboards, supplier statements, restaurant cost reports, exports.
// =====================================================================

// ---------- sale posting that never blocks a payment ----------

func (a *App) postSaleSafe(tx *sql.Tx, c *Claims, o *Order) {
	if err := a.postSale(tx, c, o); err != nil {
		mk(tx, "posting_exceptions", o.Branch, map[string]any{"source": fmt.Sprintf("order:%d", o.ID), "message": friendlyErr(err), "status": "open"})
		bumpLater("posting_exceptions")
		a.notify(RoleAccManager, o.Branch, "warn", "Sale not posted to ledger", o.No+": "+friendlyErr(err), "#/posting_exceptions")
	}
}

func (a *App) postSale(tx Querier, c *Claims, o *Order) error {
	byAcct := map[string]int64{}
	for _, p := range loadPayments(tx, o.ID) {
		sign := int64(1)
		if p.Kind == "refund" {
			sign = -1
		}
		byAcct[payMethods[p.Method]] += sign * p.Amount
	}
	var lines []JLine
	keys := make([]string, 0, len(byAcct))
	for k := range byAcct {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		lines = append(lines, Dr(k, byAcct[k]))
	}
	rev := o.Subtotal - o.Discount
	if a.settingBool("pos", "tax_inclusive", false) {
		rev -= o.Tax
	}
	lines = append(lines, Cr("sales", rev), Cr("tax_payable", o.Tax), Cr("service_income", o.Service), Cr("tips_payable", o.Tip), Cr("delivery_income", o.DeliveryFee))
	if a.settingBool("accounting", "post_cogs", true) {
		lines = append(lines, Dr("cogs", o.Cost), Cr("inventory", o.Cost))
	}
	_, err := a.post(tx, o.Branch, today(), fmt.Sprintf("order:%d", o.ID), "Sale "+o.No, c.UID, lines...)
	return err
}

func (a *App) retryPosting(tx *sql.Tx, c *Claims, r *Rec) error {
	src := r.S("source")
	if !strings.HasPrefix(src, "order:") {
		return uerr("this item cannot be retried automatically")
	}
	id, _ := strconv.ParseInt(src[6:], 10, 64)
	o, err := loadOrder(tx, id, true)
	if err != nil {
		return uerr("order not found")
	}
	var n int
	tx.QueryRow(`SELECT COUNT(*) FROM journals WHERE source=?`, src).Scan(&n)
	if n > 0 {
		r.Status = "resolved"
		return nil
	}
	if err := a.postSale(tx, c, o); err != nil {
		return err
	}
	r.Status = "resolved"
	return nil
}

func (a *App) handleRetryPost(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	var in struct {
		OrderID int64 `json:"order_id"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	err := a.tx(func(tx *sql.Tx) error {
		o, err := loadOrder(tx, in.OrderID, true)
		if err != nil {
			return uerr("order not found")
		}
		var n int
		tx.QueryRow(`SELECT COUNT(*) FROM journals WHERE source=?`, fmt.Sprintf("order:%d", o.ID)).Scan(&n)
		if n > 0 {
			return uerr("this sale is already posted")
		}
		return a.postSale(tx, c, o)
	})
	if err != nil {
		writeErr(w, 400, friendlyErr(err))
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---------- chart of accounts ----------

func (a *App) handleAccounts(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	b := scopeBranch(c, r)
	to := qStr(r, "to")
	rows := a.queryMaps(`SELECT id,code,name,type,parent_id,is_cash,active,sys FROM accounts ORDER BY code`)
	if qStr(r, "balances") == "1" {
		bal := map[int64]AccBal{}
		for _, x := range a.balances(a.db.rdb, b, "", to) {
			bal[x.ID] = x
		}
		for _, m := range rows {
			x := bal[toInt(m["id"])]
			m["debit"], m["credit"], m["balance"] = x.Debit, x.Credit, x.Bal
		}
	}
	writeJSON(w, 200, rows)
}

func (a *App) handleSaveAccount(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Code, Name, Type string
		Parent           int64 `json:"parent_id"`
		IsCash           bool  `json:"is_cash"`
		Active           *bool `json:"active"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	in.Code, in.Name = strings.TrimSpace(in.Code), strings.TrimSpace(in.Name)
	types := map[string]bool{"asset": true, "liability": true, "equity": true, "revenue": true, "cogs": true, "expense": true}
	if in.Code == "" || in.Name == "" || !types[in.Type] {
		writeErr(w, 400, "code, name and a valid type are required")
		return
	}
	act := 1
	if in.Active != nil && !*in.Active {
		act = 0
	}
	cash := 0
	if in.IsCash {
		cash = 1
	}
	if idS := r.PathValue("id"); idS != "" {
		id, _ := strconv.ParseInt(idS, 10, 64)
		var used int
		a.db.rdb.QueryRow(`SELECT COUNT(*) FROM journal_lines WHERE account_id=?`, id).Scan(&used)
		var curType string
		a.db.rdb.QueryRow(`SELECT type FROM accounts WHERE id=?`, id).Scan(&curType)
		if used > 0 && curType != in.Type {
			writeErr(w, 409, "this account has postings - its type cannot be changed")
			return
		}
		if _, err := a.db.wdb.Exec(`UPDATE accounts SET code=?,name=?,type=?,parent_id=?,is_cash=?,active=? WHERE id=?`, in.Code, in.Name, in.Type, in.Parent, cash, act, id); err != nil {
			writeErr(w, 409, "this code is already used")
			return
		}
	} else if _, err := a.db.wdb.Exec(`INSERT INTO accounts(code,name,type,parent_id,is_cash,active) VALUES(?,?,?,?,?,?)`, in.Code, in.Name, in.Type, in.Parent, cash, act); err != nil {
		writeErr(w, 409, "this code is already used")
		return
	}
	a.loadChart()
	a.audit(r, "accounts", 0, "save", in.Code+" "+in.Name)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---------- journals ----------

func (a *App) handleJournals(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	where, args := "1=1", []any{}
	if b := scopeBranch(c, r); b != 0 {
		where += " AND j.branch_id=?"
		args = append(args, b)
	}
	if v := qStr(r, "from"); v != "" {
		where += " AND j.date>=?"
		args = append(args, v)
	}
	if v := qStr(r, "to"); v != "" {
		where += " AND j.date<=?"
		args = append(args, v)
	}
	if v := qStr(r, "status"); v != "" {
		where += " AND j.status=?"
		args = append(args, v)
	}
	switch v := qStr(r, "source"); v {
	case "":
	case "manual":
		where += " AND j.source='manual'"
	case "auto":
		where += " AND j.source<>'manual'"
	default:
		where += " AND j.source LIKE ?"
		args = append(args, v+"%")
	}
	if v := strings.TrimSpace(qStr(r, "q")); v != "" {
		where += " AND (j.ref LIKE ? OR j.memo LIKE ? OR j.source LIKE ?)"
		args = append(args, "%"+v+"%", "%"+v+"%", "%"+v+"%")
	}
	limit := int(qInt(r, "limit"))
	if limit < 1 || limit > 500 {
		limit = 30
	}
	page := int(max(qInt(r, "page"), 1))
	var total int64
	a.db.rdb.QueryRow(`SELECT COUNT(*) FROM journals j WHERE `+where, args...).Scan(&total)
	rows := a.queryMaps(`SELECT j.id,j.branch_id,j.date,j.ref,j.memo,j.source,j.status,j.created_at,COALESCE(u.name,'system') by_user,
		(SELECT COALESCE(SUM(debit),0) FROM journal_lines WHERE journal_id=j.id) amount FROM journals j LEFT JOIN users u ON u.id=j.created_by
		WHERE `+where+` ORDER BY j.id DESC LIMIT ? OFFSET ?`, append(args, limit, (page-1)*limit)...)
	writeJSON(w, 200, map[string]any{"rows": rows, "total": total, "page": page, "limit": limit})
}

func (a *App) handleJournal(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	rows := a.queryMaps(`SELECT id,branch_id,date,ref,memo,source,status,created_at FROM journals WHERE id=?`, id)
	if len(rows) == 0 || !a.journalAllowed(userFrom(r), id) {
		writeErr(w, 404, "journal not found")
		return
	}
	lines := a.queryMaps(`SELECT l.id,l.account_id,a.code,a.name,l.debit,l.credit,l.memo,l.date FROM journal_lines l JOIN accounts a ON a.id=l.account_id WHERE l.journal_id=? ORDER BY l.id`, id)
	j := rows[0]
	j["lines"] = lines
	writeJSON(w, 200, j)
}

type jlIn struct {
	Account int64  `json:"account"`
	Debit   int64  `json:"debit"`
	Credit  int64  `json:"credit"`
	Memo    string `json:"memo"`
	Dim     string `json:"dim"`
}

func (a *App) handleCreateJournal(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	var in struct {
		Date   string `json:"date"`
		Memo   string `json:"memo"`
		Branch int64  `json:"branch_id"`
		Draft  bool   `json:"draft"`
		Lines  []jlIn `json:"lines"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	var id int64
	err := a.tx(func(tx *sql.Tx) error {
		if _, ok := parseDate(in.Date); !ok {
			return uerr("choose a date")
		}
		if c.Branch != 0 {
			in.Branch = c.Branch
		}
		if len(in.Lines) < 2 {
			return uerr("a journal needs at least two lines")
		}
		var dr, cr int64
		for _, l := range in.Lines {
			if l.Debit < 0 || l.Credit < 0 || (l.Debit > 0 && l.Credit > 0) || l.Account == 0 {
				return uerr("each line needs an account and either a debit or a credit")
			}
			dr += l.Debit
			cr += l.Credit
		}
		if dr != cr {
			return uerr("debits (%.2f) and credits (%.2f) must be equal", float64(dr)/100, float64(cr)/100)
		}
		if dr == 0 {
			return uerr("enter amounts")
		}
		if !in.Draft {
			if err := a.checkPeriodForManual(tx, c, in.Date); err != nil {
				return err
			}
		}
		if err := a.checkDimensionRules(tx, in.Lines); err != nil {
			return err
		}
		status := "posted"
		if in.Draft {
			status = "draft"
		}
		res, err := tx.Exec(`INSERT INTO journals(branch_id,date,memo,source,status,created_by,created_at) VALUES(?,?,?,'manual',?,?,?)`, in.Branch, in.Date, trunc(in.Memo, 200), status, c.UID, nowUnix())
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
		tx.Exec(`UPDATE journals SET ref=? WHERE id=?`, fmt.Sprintf("JE-%06d", id), id)
		for _, l := range in.Lines {
			memo := l.Memo
			if l.Dim != "" {
				memo = strings.TrimSpace(memo + " [" + l.Dim + "]")
			}
			tx.Exec(`INSERT INTO journal_lines(journal_id,branch_id,account_id,debit,credit,date,memo,dim) VALUES(?,?,?,?,?,?,?,?)`, id, in.Branch, l.Account, l.Debit, l.Credit, in.Date, trunc(memo, 200), trunc(l.Dim, 60))
		}
		return nil
	})
	if err != nil {
		writeErr(w, 400, friendlyErr(err))
		return
	}
	a.audit(r, "journals", id, "create", in.Memo)
	writeJSON(w, 201, map[string]any{"id": id})
}

func (a *App) checkPeriodForManual(q Querier, c *Claims, date string) error {
	if periodClosed(q, date) {
		return uerr("the accounting period containing %s is closed", date)
	}
	var n int
	q.QueryRow(`SELECT COUNT(*) FROM records WHERE res='periods' AND status='soft_locked' AND json_extract(data,'$.from')<=? AND json_extract(data,'$.to')>=?`, date, date).Scan(&n)
	if n > 0 && a.level(c.Role, "accounting") < LvApprove {
		return uerr("that period is soft-locked - only an accounting manager can post to it")
	}
	return nil
}

func (a *App) checkDimensionRules(q Querier, lines []jlIn) error {
	for _, rl := range recs(q, "dimension_rules", 0) {
		if rl.Status != "active" || !rl.B("required") {
			continue
		}
		for _, l := range lines {
			if a.acctInfo(l.Account).Type == rl.S("applies_to") && l.Dim == "" {
				return uerr("%s lines need a dimension value (rule #%d)", rl.S("applies_to"), rl.ID)
			}
		}
	}
	return nil
}

func (a *App) handlePostJournal(w http.ResponseWriter, r *http.Request) {
	if !a.journalAllowed(userFrom(r), pathID(r)) {
		writeErr(w, 404, "journal not found")
		return
	}
	c := userFrom(r)
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	err := a.tx(func(tx *sql.Tx) error {
		var st, date string
		if tx.QueryRow(`SELECT status,date FROM journals WHERE id=?`, id).Scan(&st, &date) != nil || st != "draft" {
			return uerr("only draft journals can be posted")
		}
		if err := a.checkPeriodForManual(tx, c, date); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE journals SET status='posted' WHERE id=?`, id)
		return err
	})
	if err != nil {
		writeErr(w, 400, friendlyErr(err))
		return
	}
	a.audit(r, "journals", id, "post", "")
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *App) handleReverseJournal(w http.ResponseWriter, r *http.Request) {
	if !a.journalAllowed(userFrom(r), pathID(r)) {
		writeErr(w, 404, "journal not found")
		return
	}
	c := userFrom(r)
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	err := a.tx(func(tx *sql.Tx) error {
		var st string
		if tx.QueryRow(`SELECT status FROM journals WHERE id=?`, id).Scan(&st) != nil || st != "posted" {
			return uerr("only posted journals can be reversed")
		}
		if err := a.checkPeriodForManual(tx, c, today()); err != nil {
			return err
		}
		return a.reverseJournal(tx, id, c.UID, fmt.Sprintf("Reversal of JE-%06d", id))
	})
	if err != nil {
		writeErr(w, 400, friendlyErr(err))
		return
	}
	a.audit(r, "journals", id, "reverse", "")
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *App) handleDeleteJournal(w http.ResponseWriter, r *http.Request) {
	if !a.journalAllowed(userFrom(r), pathID(r)) {
		writeErr(w, 404, "journal not found")
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	err := a.tx(func(tx *sql.Tx) error {
		var st string
		if tx.QueryRow(`SELECT status FROM journals WHERE id=?`, id).Scan(&st) != nil || st != "draft" {
			return uerr("only draft journals can be deleted")
		}
		tx.Exec(`DELETE FROM journal_lines WHERE journal_id=?`, id)
		tx.Exec(`DELETE FROM journals WHERE id=?`, id)
		return nil
	})
	if err != nil {
		writeErr(w, 400, friendlyErr(err))
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// opening balances: one balanced entry; the difference goes to owner's equity
func (a *App) handleOpening(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	var in struct {
		Date  string `json:"date"`
		Lines []jlIn `json:"lines"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	err := a.tx(func(tx *sql.Tx) error {
		var lines []JLine
		var dr, cr int64
		for _, l := range in.Lines {
			if l.Debit == 0 && l.Credit == 0 {
				continue
			}
			info := a.acctInfo(l.Account)
			lines = append(lines, JLine{Acct: info.Code, Dr: l.Debit, Cr: l.Credit, Memo: "Opening balance"})
			dr += l.Debit
			cr += l.Credit
		}
		if len(lines) == 0 {
			return uerr("enter at least one balance")
		}
		if dr > cr {
			lines = append(lines, Cr("equity", dr-cr))
		} else if cr > dr {
			lines = append(lines, Dr("equity", cr-dr))
		}
		_, err := a.post(tx, 0, in.Date, "opening", "Opening balances", c.UID, lines...)
		return err
	})
	if err != nil {
		writeErr(w, 400, friendlyErr(err))
		return
	}
	a.audit(r, "journals", 0, "opening", in.Date)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---------- financial statements ----------

type statRow struct {
	Code string `json:"code"`
	Name string `json:"name"`
	Bal  int64  `json:"balance"`
	ID   int64  `json:"id"`
}

func sumRows(rs []statRow) (t int64) {
	for _, r := range rs {
		t += r.Bal
	}
	return
}

func (a *App) pnl(q Querier, b int64, from, to string) map[string]any {
	var rev, cogs, exp []statRow
	for _, x := range a.balances(q, b, from, to) {
		if x.Bal == 0 {
			continue
		}
		row := statRow{x.Code, x.Name, x.Bal, x.ID}
		switch x.Type {
		case "revenue":
			rev = append(rev, row)
		case "cogs":
			cogs = append(cogs, row)
		case "expense":
			exp = append(exp, row)
		}
	}
	tr, tc, te := sumRows(rev), sumRows(cogs), sumRows(exp)
	return map[string]any{"revenue": rev, "cogs": cogs, "expenses": exp, "total_revenue": tr, "total_cogs": tc, "gross_profit": tr - tc, "total_expenses": te, "net_income": tr - tc - te}
}

func prevRange(from, to string) (string, string) {
	f, ok1 := parseDate(from)
	t, ok2 := parseDate(to)
	if !ok1 || !ok2 {
		return "", ""
	}
	days := int(t.Sub(f).Hours()/24) + 1
	return f.AddDate(0, 0, -days).Format("2006-01-02"), f.AddDate(0, 0, -1).Format("2006-01-02")
}

func (a *App) handleReportPnL(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	b := scopeBranch(c, r)
	from, to := qStr(r, "from"), qStr(r, "to")
	if from == "" || to == "" {
		from, to = time.Now().Format("2006")+"-01-01", today()
	}
	out := a.pnl(a.db.rdb, b, from, to)
	if qStr(r, "compare") == "1" {
		pf, pt := prevRange(from, to)
		out["previous"] = a.pnl(a.db.rdb, b, pf, pt)
	}
	out["from"], out["to"] = from, to
	writeJSON(w, 200, out)
}

func (a *App) handleReportBS(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	b := scopeBranch(c, r)
	to := qStr(r, "to")
	if to == "" {
		to = today()
	}
	var assets, liab, eq []statRow
	var earnings int64
	for _, x := range a.balances(a.db.rdb, b, "", to) {
		row := statRow{x.Code, x.Name, x.Bal, x.ID}
		switch x.Type {
		case "asset":
			if x.Bal != 0 {
				assets = append(assets, row)
			}
		case "liability":
			if x.Bal != 0 {
				liab = append(liab, row)
			}
		case "equity":
			if x.Bal != 0 {
				eq = append(eq, row)
			}
		case "revenue":
			earnings += x.Bal
		case "cogs", "expense":
			earnings -= x.Bal
		}
	}
	ta, tl, te := sumRows(assets), sumRows(liab), sumRows(eq)+earnings
	writeJSON(w, 200, map[string]any{"to": to, "assets": assets, "liabilities": liab, "equity": eq, "earnings": earnings, "total_assets": ta, "total_liabilities": tl, "total_equity": te,
		"difference": ta - tl - te, "balanced": ta-tl-te == 0})
}

var sourceLabels = map[string]string{"order": "Sales receipts", "supplier_payment": "Supplier payments", "payroll": "Payroll", "cashmove": "Cash in/out", "petty": "Petty cash",
	"collection": "Customer collections", "bill": "Supplier bills", "transfer_cash": "Cash transfers", "claim": "Expense claims", "cod": "COD from drivers", "payout": "Driver payouts",
	"session": "Cash over/short", "giftcard": "Gift card sales", "cheque": "Cheques", "channel": "Channel settlements", "asset": "Fixed assets", "opening": "Opening balance", "manual": "Manual entries",
	"settlement": "Final settlements", "gratuity": "Gratuity", "fx": "FX", "taxreturn": "Tax payments", "wht": "Withholding tax", "accrual": "Accruals", "provider": "Courier providers"}

func (a *App) handleReportCash(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	b := scopeBranch(c, r)
	from, to := qStr(r, "from"), qStr(r, "to")
	if from == "" || to == "" {
		from, to = time.Now().Format("2006")+"-01-01", today()
	}
	bw, args := "", []any{from, to}
	if b != 0 {
		bw = " AND l.branch_id=?"
		args = append(args, b)
	}
	rows := a.queryMaps(`SELECT j.source src,SUM(l.debit) inflow,SUM(l.credit) outflow FROM journal_lines l JOIN journals j ON j.id=l.journal_id AND j.status<>'draft' JOIN accounts a ON a.id=l.account_id AND a.is_cash=1
		WHERE l.date>=? AND l.date<=?`+bw+` GROUP BY j.source`, args...)
	groups := map[string][2]int64{}
	for _, m := range rows {
		k := toStr(m["src"])
		if i := strings.IndexByte(k, ':'); i >= 0 {
			k = k[:i]
		}
		g := groups[k]
		g[0] += toInt(m["inflow"])
		g[1] += toInt(m["outflow"])
		groups[k] = g
	}
	out := []map[string]any{}
	var tin, tout int64
	for k, g := range groups {
		label := sourceLabels[k]
		if label == "" {
			label = k
		}
		out = append(out, map[string]any{"label": label, "inflow": g[0], "outflow": g[1], "net": g[0] - g[1]})
		tin += g[0]
		tout += g[1]
	}
	sort.Slice(out, func(i, j int) bool { return toInt(out[i]["net"]) > toInt(out[j]["net"]) })
	var open int64
	for _, x := range a.balances(a.db.rdb, b, "", func() string { t, _ := parseDate(from); return t.AddDate(0, 0, -1).Format("2006-01-02") }()) {
		if a.acctInfo(x.ID).Cash {
			open += x.Bal
		}
	}
	writeJSON(w, 200, map[string]any{"from": from, "to": to, "rows": out, "inflow": tin, "outflow": tout, "net": tin - tout, "opening": open, "closing": open + tin - tout})
}

func (a *App) handleTrialBalance(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	b := scopeBranch(c, r)
	to := qStr(r, "to")
	if to == "" {
		to = today()
	}
	rows := []map[string]any{}
	var td, tc int64
	for _, x := range a.balances(a.db.rdb, b, "", to) {
		if x.Debit == 0 && x.Credit == 0 {
			continue
		}
		net := x.Debit - x.Credit
		d, cr := int64(0), int64(0)
		if net >= 0 {
			d = net
		} else {
			cr = -net
		}
		td += d
		tc += cr
		rows = append(rows, map[string]any{"code": x.Code, "name": x.Name, "type": x.Type, "debit": d, "credit": cr})
	}
	writeJSON(w, 200, map[string]any{"to": to, "rows": rows, "total_debit": td, "total_credit": tc, "balanced": td == tc})
}

func (a *App) handleLedger(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	b := scopeBranch(c, r)
	acc := qInt(r, "account")
	from, to := qStr(r, "from"), qStr(r, "to")
	if from == "" {
		from = time.Now().Format("2006") + "-01-01"
	}
	if to == "" {
		to = today()
	}
	info := a.acctInfo(acc)
	if info.ID == 0 {
		writeJSON(w, 200, map[string]any{"rows": []any{}})
		return
	}
	natural := int64(1)
	if info.Type != "asset" && info.Type != "expense" && info.Type != "cogs" {
		natural = -1
	}
	bw, bargs := "", []any{}
	if b != 0 {
		bw = " AND l.branch_id=?"
		bargs = []any{b}
	}
	dim := qStr(r, "dim")
	if dim != "" {
		bw += " AND l.dim=?"
		bargs = append(bargs, dim)
	}
	var odr, ocr int64
	a.db.rdb.QueryRow(`SELECT COALESCE(SUM(l.debit),0),COALESCE(SUM(l.credit),0) FROM journal_lines l JOIN journals j ON j.id=l.journal_id AND j.status<>'draft' WHERE l.account_id=? AND l.date<?`+bw,
		append([]any{acc, from}, bargs...)...).Scan(&odr, &ocr)
	opening := (odr - ocr) * natural
	lines := a.queryMaps(`SELECT l.id,l.date,j.id journal_id,j.ref,j.memo jmemo,l.memo,l.debit,l.credit,j.source,l.dim FROM journal_lines l JOIN journals j ON j.id=l.journal_id AND j.status<>'draft'
		WHERE l.account_id=? AND l.date>=? AND l.date<=?`+bw+` ORDER BY l.date,l.id LIMIT 5000`, append([]any{acc, from, to}, bargs...)...)
	run := opening
	for _, m := range lines {
		run += (toInt(m["debit"]) - toInt(m["credit"])) * natural
		m["balance"] = run
	}
	writeJSON(w, 200, map[string]any{"account": info.Code + " · " + info.Name, "opening": opening, "rows": lines, "closing": run})
}

// ---------- accounting dashboard ----------

func (a *App) handleAccDash(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	b := scopeBranch(c, r)
	from, to := qStr(r, "from"), qStr(r, "to")
	if from == "" || to == "" {
		from, to = time.Now().Format("2006-01")+"-01", today()
	}
	a.serveCached(w, a.dashCache, fmt.Sprintf("acc|%d|%s|%s|%d", b, from, to, verOf("accounts")), 5*time.Second, func() ([]byte, error) {
		return jsonBytes(a.computeAccDash(b, from, to)), nil
	})
}

func (a *App) cashBalance(q Querier, b int64, to string) int64 {
	var t int64
	for _, x := range a.balances(q, b, "", to) {
		if a.acctInfo(x.ID).Cash {
			t += x.Bal
		}
	}
	return t
}

func (a *App) computeAccDash(b int64, from, to string) map[string]any {
	q := a.db.rdb
	cur := a.pnl(q, b, from, to)
	pf, pt := prevRange(from, to)
	prev := a.pnl(q, b, pf, pt)
	out := map[string]any{"revenue": cur["total_revenue"], "gross_profit": cur["gross_profit"], "net_income": cur["net_income"],
		"prev_revenue": prev["total_revenue"], "prev_gross_profit": prev["gross_profit"], "prev_net_income": prev["net_income"]}
	out["cash"] = a.cashBalance(q, b, to)
	out["prev_cash"] = a.cashBalance(q, b, pt)
	// trend
	f, _ := parseDate(from)
	t, _ := parseDate(to)
	format := "%Y-%m-%d"
	if t.Sub(f) > 62*24*time.Hour {
		format = "%Y-%m"
	}
	bw, args := "", []any{from, to}
	if b != 0 {
		bw = " AND l.branch_id=?"
		args = append(args, b)
	}
	series := map[string]map[string]int64{}
	for _, m := range a.queryMaps(`SELECT strftime('`+format+`',l.date) bucket,a.type,SUM(l.credit-l.debit) v FROM journal_lines l JOIN journals j ON j.id=l.journal_id AND j.status<>'draft'
		JOIN accounts a ON a.id=l.account_id WHERE a.type IN ('revenue','cogs','expense') AND l.date>=? AND l.date<=?`+bw+` GROUP BY bucket,a.type`, args...) {
		k := toStr(m["bucket"])
		if series[k] == nil {
			series[k] = map[string]int64{}
		}
		series[k][toStr(m["type"])] = toInt(m["v"])
	}
	keys := make([]string, 0, len(series))
	for k := range series {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	trend := []map[string]any{}
	for _, k := range keys {
		s := series[k]
		rev, cogs, exp := s["revenue"], -s["cogs"], -s["expense"]
		trend = append(trend, map[string]any{"label": k, "revenue": rev, "gross_profit": rev - cogs, "net_income": rev - cogs - exp})
	}
	out["trend"] = trend
	// expense mix
	mix := []map[string]any{}
	for _, row := range append(cur["cogs"].([]statRow), cur["expenses"].([]statRow)...) {
		mix = append(mix, map[string]any{"name": row.Name, "value": row.Bal})
	}
	sort.Slice(mix, func(i, j int) bool { return toInt(mix[i]["value"]) > toInt(mix[j]["value"]) })
	if len(mix) > 6 {
		mix = mix[:6]
	}
	out["expense_mix"] = mix
	// cash movement
	cm := map[string][2]int64{}
	for _, m := range a.queryMaps(`SELECT strftime('`+format+`',l.date) bucket,SUM(l.debit) i,SUM(l.credit) o FROM journal_lines l JOIN journals j ON j.id=l.journal_id AND j.status<>'draft'
		JOIN accounts a ON a.id=l.account_id AND a.is_cash=1 WHERE l.date>=? AND l.date<=?`+bw+` GROUP BY bucket`, args...) {
		cm[toStr(m["bucket"])] = [2]int64{toInt(m["i"]), toInt(m["o"])}
	}
	ck := make([]string, 0, len(cm))
	for k := range cm {
		ck = append(ck, k)
	}
	sort.Strings(ck)
	cash := []map[string]any{}
	for _, k := range ck {
		cash = append(cash, map[string]any{"label": k, "inflow": cm[k][0], "outflow": cm[k][1], "net": cm[k][0] - cm[k][1]})
	}
	out["cash_movement"] = cash
	// balance sheet snapshot
	var assets, liab, eqt, earn int64
	top := []map[string]any{}
	for _, x := range a.balances(q, b, "", to) {
		switch x.Type {
		case "asset":
			assets += x.Bal
			if x.Bal != 0 {
				top = append(top, map[string]any{"code": x.Code, "name": x.Name, "balance": x.Bal})
			}
		case "liability":
			liab += x.Bal
		case "equity":
			eqt += x.Bal
		case "revenue":
			earn += x.Bal
		case "cogs", "expense":
			earn -= x.Bal
		}
	}
	sort.Slice(top, func(i, j int) bool { return toInt(top[i]["balance"]) > toInt(top[j]["balance"]) })
	if len(top) > 5 {
		top = top[:5]
	}
	out["assets"], out["liabilities"], out["equity"], out["bs_difference"], out["top_assets"] = assets, liab, eqt+earn, assets-liab-eqt-earn, top
	// branches
	brs := []map[string]any{}
	for _, br := range recs(q, "branches", 0) {
		p := a.pnl(q, br.ID, from, to)
		pp := a.pnl(q, br.ID, pf, pt)
		margin, change := 0.0, 0.0
		if tr := toFloat(p["total_revenue"]); tr != 0 {
			margin = toFloat(p["net_income"]) / tr * 100
		}
		if pr := toFloat(pp["total_revenue"]); pr != 0 {
			change = (toFloat(p["total_revenue"]) - pr) / pr * 100
		}
		brs = append(brs, map[string]any{"name": br.S("name"), "revenue": p["total_revenue"], "gross_profit": p["gross_profit"], "net_income": p["net_income"], "margin": margin, "change": change})
	}
	out["branches"] = brs
	// close controls
	var openPeriods, drafts, failed, recsOpen int64
	q.QueryRow(`SELECT COUNT(*) FROM records WHERE res='periods' AND status IN ('open','soft_locked')`).Scan(&openPeriods)
	q.QueryRow(`SELECT COUNT(*) FROM journals WHERE status='draft'`).Scan(&drafts)
	q.QueryRow(`SELECT COUNT(*) FROM records WHERE res='posting_exceptions' AND status='open'`).Scan(&failed)
	q.QueryRow(`SELECT COUNT(*) FROM records WHERE res='bank_recs' AND status='open'`).Scan(&recsOpen)
	out["open_periods"], out["draft_journals"], out["failed_journals"], out["pending_recs"] = openPeriods, drafts, failed, recsOpen
	out["recent"] = a.queryMaps(`SELECT j.id,j.date,j.ref,j.memo,j.source,(SELECT COALESCE(SUM(debit),0) FROM journal_lines WHERE journal_id=j.id) amount FROM journals j WHERE j.status<>'draft' ORDER BY j.id DESC LIMIT 8`)
	return out
}

// ---------- restaurant accounting reports ----------

type costRow struct {
	Name    string  `json:"name"`
	Qty     int64   `json:"qty"`
	Revenue int64   `json:"revenue"`
	Cost    int64   `json:"cost"`
	Margin  int64   `json:"margin"`
	CostPct float64 `json:"cost_pct"`
	MarginP float64 `json:"margin_pct"`
	Class   string  `json:"class,omitempty"`
}

func (a *App) costRows(b int64, from, to int64, by string) []costRow {
	group := "i.product_id"
	nameCol := "i.name"
	if by == "category" {
		group, nameCol = "i.category", "i.category"
	}
	bw, args := "", []any{from, to}
	if b != 0 {
		bw = " AND o.branch_id=?"
		args = append(args, b)
	}
	rows := a.queryMaps(`SELECT `+nameCol+` name,SUM(i.qty) qty,SUM(i.qty*i.price) revenue,SUM(i.qty*i.cost) cost FROM order_items i JOIN orders o ON o.id=i.order_id
		WHERE o.pay_status='paid' AND o.status NOT IN ('cancelled','refunded') AND i.status<>'void' AND o.created_at>=? AND o.created_at<?`+bw+` GROUP BY `+group+` ORDER BY revenue DESC`, args...)
	out := make([]costRow, 0, len(rows))
	for _, m := range rows {
		x := costRow{Name: toStr(m["name"]), Qty: toInt(m["qty"]), Revenue: toInt(m["revenue"]), Cost: toInt(m["cost"])}
		x.Margin = x.Revenue - x.Cost
		if x.Revenue > 0 {
			x.CostPct = math.Round(float64(x.Cost)/float64(x.Revenue)*1000) / 10
			x.MarginP = math.Round(float64(x.Margin)/float64(x.Revenue)*1000) / 10
		}
		out = append(out, x)
	}
	return out
}

func rangeUnix(r *http.Request) (int64, int64) {
	from, to := qInt(r, "from"), qInt(r, "to")
	if to == 0 {
		to = time.Now().Unix() + 1
	}
	if from == 0 {
		from = to - 30*86400
	}
	return from, to
}

func (a *App) handleFoodCost(w http.ResponseWriter, r *http.Request) {
	from, to := rangeUnix(r)
	b := scopeBranch(userFrom(r), r)
	cats, prods := a.costRows(b, from, to, "category"), a.costRows(b, from, to, "product")
	var rev, cost int64
	for _, x := range cats {
		rev += x.Revenue
		cost += x.Cost
	}
	pct := 0.0
	if rev > 0 {
		pct = math.Round(float64(cost)/float64(rev)*1000) / 10
	}
	var wasteVal int64
	if id, ok := a.acct("waste_expense"); ok {
		a.db.rdb.QueryRow(`SELECT COALESCE(SUM(debit-credit),0) FROM journal_lines WHERE account_id=? AND date>=? AND date<=?`, id, toDay(from), toDay(to)).Scan(&wasteVal)
	}
	writeJSON(w, 200, map[string]any{"revenue": rev, "cost": cost, "cost_pct": pct, "waste": wasteVal, "categories": cats, "products": prods})
}

func (a *App) handleMenuProfit(w http.ResponseWriter, r *http.Request) {
	from, to := rangeUnix(r)
	b := scopeBranch(userFrom(r), r)
	rows := a.costRows(b, from, to, "product")
	var qty int64
	var marginP float64
	for _, x := range rows {
		qty += x.Qty
		marginP += x.MarginP
	}
	avgQty := float64(qty) / math.Max(float64(len(rows)), 1)
	avgM := marginP / math.Max(float64(len(rows)), 1)
	for i := range rows {
		hiP, hiM := float64(rows[i].Qty) >= avgQty, rows[i].MarginP >= avgM
		switch {
		case hiP && hiM:
			rows[i].Class = "star"
		case hiP:
			rows[i].Class = "plowhorse"
		case hiM:
			rows[i].Class = "puzzle"
		default:
			rows[i].Class = "dog"
		}
	}
	writeJSON(w, 200, map[string]any{"rows": rows, "avg_qty": avgQty, "avg_margin": avgM})
}

func (a *App) handleTips(w http.ResponseWriter, r *http.Request) {
	from, to := rangeUnix(r)
	b := scopeBranch(userFrom(r), r)
	bw, args := "", []any{from, to}
	if b != 0 {
		bw = " AND o.branch_id=?"
		args = append(args, b)
	}
	byStaff := a.queryMaps(`SELECT COALESCE(u.name,'?') name,COUNT(*) orders,SUM(o.tip) tips,SUM(o.service) service FROM orders o LEFT JOIN users u ON u.id=o.created_by
		WHERE o.pay_status='paid' AND o.status NOT IN ('cancelled','refunded') AND o.created_at>=? AND o.created_at<?`+bw+` GROUP BY o.created_by ORDER BY tips DESC`, args...)
	byDay := a.queryMaps(`SELECT date(o.created_at,'unixepoch') day,SUM(o.tip) tips,SUM(o.service) service FROM orders o WHERE o.pay_status='paid' AND o.status NOT IN ('cancelled','refunded') AND o.created_at>=? AND o.created_at<?`+bw+` GROUP BY day ORDER BY day`, args...)
	var tips, svc int64
	for _, m := range byDay {
		tips += toInt(m["tips"])
		svc += toInt(m["service"])
	}
	var payable int64
	if id, ok := a.acct("tips_payable"); ok {
		a.db.rdb.QueryRow(`SELECT COALESCE(SUM(credit-debit),0) FROM journal_lines WHERE account_id=?`, id).Scan(&payable)
	}
	writeJSON(w, 200, map[string]any{"tips": tips, "service": svc, "payable": payable, "by_staff": byStaff, "by_day": byDay})
}

func (a *App) handleSupplierStatements(w http.ResponseWriter, r *http.Request) {
	b := scopeBranch(userFrom(r), r)
	sup := a.labelMap("suppliers")
	type agg struct {
		Name                      string
		Bills, Paid, Balance      int64
		Current, D30, D60, D90, O int64
	}
	by := map[int64]*agg{}
	t := today()
	for _, bl := range recs(a.db.rdb, "supplier_bills", b) {
		if bl.Status == "void" {
			continue
		}
		id := bl.I("supplier")
		x := by[id]
		if x == nil {
			x = &agg{Name: sup[id]}
			by[id] = x
		}
		due := bl.I("amount") - bl.I("paid")
		x.Bills += bl.I("amount")
		x.Paid += bl.I("paid")
		x.Balance += due
		if due <= 0 {
			continue
		}
		days := 0
		if dd, ok := parseDate(bl.S("due")); ok {
			if td, ok2 := parseDate(t); ok2 {
				days = int(td.Sub(dd).Hours() / 24)
			}
		}
		switch {
		case days <= 0:
			x.Current += due
		case days <= 30:
			x.D30 += due
		case days <= 60:
			x.D60 += due
		case days <= 90:
			x.D90 += due
		default:
			x.O += due
		}
	}
	rows := []map[string]any{}
	for id, x := range by {
		rows = append(rows, map[string]any{"supplier_id": id, "supplier": x.Name, "billed": x.Bills, "paid": x.Paid, "balance": x.Balance, "current": x.Current, "d1_30": x.D30, "d31_60": x.D60, "d61_90": x.D90, "over_90": x.O})
	}
	sort.Slice(rows, func(i, j int) bool { return toInt(rows[i]["balance"]) > toInt(rows[j]["balance"]) })
	var apLedger int64
	if id, ok := a.acct("ap"); ok {
		a.db.rdb.QueryRow(`SELECT COALESCE(SUM(credit-debit),0) FROM journal_lines WHERE account_id=?`, id).Scan(&apLedger)
	}
	writeJSON(w, 200, map[string]any{"rows": rows, "ledger_ap": apLedger})
}

func (a *App) handleInvValuation(w http.ResponseWriter, r *http.Request) {
	b := scopeBranch(userFrom(r), r)
	rows := a.stockRows(b)
	var total int64
	for _, x := range rows {
		total += x.Value
	}
	var ledger int64
	if id, ok := a.acct("inventory"); ok {
		q, args := `SELECT COALESCE(SUM(debit-credit),0) FROM journal_lines WHERE account_id=?`, []any{id}
		if b != 0 {
			q += ` AND branch_id=?`
			args = append(args, b)
		}
		a.db.rdb.QueryRow(q, args...).Scan(&ledger)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Value > rows[j].Value })
	writeJSON(w, 200, map[string]any{"rows": rows, "total": total, "ledger": ledger, "difference": total - ledger})
}

func (a *App) handleConsolidation(w http.ResponseWriter, r *http.Request) {
	from, to := qStr(r, "from"), qStr(r, "to")
	if from == "" || to == "" {
		from, to = time.Now().Format("2006")+"-01-01", today()
	}
	rows := []map[string]any{}
	var tr, tc, te int64
	for _, br := range append([]*Rec{{ID: 0, Data: map[string]any{"name": "Head office / unassigned"}}}, recs(a.db.rdb, "branches", 0)...) {
		p := a.pnl(a.db.rdb, br.ID, from, to)
		if toInt(p["total_revenue"]) == 0 && toInt(p["total_expenses"]) == 0 && toInt(p["total_cogs"]) == 0 {
			continue
		}
		rows = append(rows, map[string]any{"entity": br.S("name"), "revenue": p["total_revenue"], "cogs": p["total_cogs"], "expenses": p["total_expenses"], "net_income": p["net_income"]})
		tr += toInt(p["total_revenue"])
		tc += toInt(p["total_cogs"])
		te += toInt(p["total_expenses"])
	}
	var elim int64
	for _, ic := range recs(a.db.rdb, "intercompany", 0) {
		if ic.Status == "eliminated" && ic.S("date") >= from && ic.S("date") <= to {
			elim += ic.I("amount")
		}
	}
	writeJSON(w, 200, map[string]any{"from": from, "to": to, "rows": rows, "revenue": tr, "cogs": tc, "expenses": te, "net_income": tr - tc - te, "eliminated": elim})
}

func (a *App) handleUnposted(w http.ResponseWriter, r *http.Request) {
	b := scopeBranch(userFrom(r), r)
	bw, args := "", []any{}
	if b != 0 {
		bw = " AND o.branch_id=?"
		args = append(args, b)
	}
	rows := a.queryMaps(`SELECT o.id,o.no,o.total,o.created_at,o.branch_id FROM orders o WHERE o.pay_status='paid' AND o.status NOT IN ('refunded','cancelled')`+bw+
		` AND NOT EXISTS (SELECT 1 FROM journals j WHERE j.source='order:'||o.id) ORDER BY o.id DESC LIMIT 200`, args...)
	writeJSON(w, 200, rows)
}

// ---------- exports ----------

func (a *App) handleAccExport(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	b := scopeBranch(c, r)
	typ := qStr(r, "type")
	switch typ {
	case "trial_balance", "profit_loss", "balance_sheet", "general_ledger":
	default:
		typ = "general_ledger"
	}
	from, to := qStr(r, "from"), qStr(r, "to")
	if to == "" {
		to = today()
	}
	if from == "" {
		from = time.Now().Format("2006") + "-01-01"
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+typ+`.csv"`)
	w.Write([]byte("\xef\xbb\xbf"))
	cw := csv.NewWriter(w)
	m := func(v int64) string { return fmt.Sprintf("%.2f", float64(v)/100) }
	n := 0
	switch typ {
	case "trial_balance":
		cw.Write([]string{"Code", "Account", "Type", "Debit", "Credit"})
		for _, x := range a.balances(a.db.rdb, b, "", to) {
			if x.Debit != 0 || x.Credit != 0 {
				net := x.Debit - x.Credit
				d, cr := int64(0), int64(0)
				if net >= 0 {
					d = net
				} else {
					cr = -net
				}
				cw.Write([]string{x.Code, csvSafe(x.Name), x.Type, m(d), m(cr)})
				n++
			}
		}
	case "profit_loss":
		cw.Write([]string{"Section", "Code", "Account", "Amount"})
		p := a.pnl(a.db.rdb, b, from, to)
		for _, sec := range []string{"revenue", "cogs", "expenses"} {
			for _, x := range p[sec].([]statRow) {
				cw.Write([]string{sec, x.Code, csvSafe(x.Name), m(x.Bal)})
				n++
			}
		}
		cw.Write([]string{"net_income", "", "Net income", m(toInt(p["net_income"]))})
	case "balance_sheet":
		cw.Write([]string{"Type", "Code", "Account", "Balance"})
		for _, x := range a.balances(a.db.rdb, b, "", to) {
			if x.Bal != 0 && (x.Type == "asset" || x.Type == "liability" || x.Type == "equity") {
				cw.Write([]string{x.Type, x.Code, csvSafe(x.Name), m(x.Bal)})
				n++
			}
		}
	default: // general_ledger / journals
		typ = "general_ledger"
		cw.Write([]string{"Date", "Journal", "Source", "Code", "Account", "Memo", "Debit", "Credit"})
		bw, args := "", []any{from, to}
		if b != 0 {
			bw = " AND l.branch_id=?"
			args = append(args, b)
		}
		for _, x := range a.queryMaps(`SELECT l.date,j.ref,j.source,a.code,a.name,COALESCE(NULLIF(l.memo,''),j.memo) memo,l.debit,l.credit FROM journal_lines l JOIN journals j ON j.id=l.journal_id AND j.status<>'draft'
			JOIN accounts a ON a.id=l.account_id WHERE l.date>=? AND l.date<=?`+bw+` ORDER BY l.date,l.id LIMIT 100000`, args...) {
			cw.Write([]string{toStr(x["date"]), toStr(x["ref"]), toStr(x["source"]), toStr(x["code"]), csvSafe(toStr(x["name"])), csvSafe(toStr(x["memo"])), m(toInt(x["debit"])), m(toInt(x["credit"]))})
			n++
		}
	}
	cw.Flush()
	mk(a.db.wdb, "acc_exports", b, map[string]any{"name": typ + " " + from + " to " + to, "type": typ, "from": from, "to": to, "rows": float64(n), "user": c.User})
	a.audit(r, "accounting", 0, "export", typ)
}

// ---------- bank statement import ----------

func (a *App) handleBankImport(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	var in struct {
		Account int64  `json:"account"`
		Text    string `json:"text"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	n := 0
	err := a.tx(func(tx *sql.Tx) error {
		acc, err := loadRec(tx, in.Account)
		if err != nil || acc.Res != "fin_accounts" {
			return uerr("choose a financial account")
		}
		for _, line := range strings.Split(in.Text, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(strings.ToLower(line), "date") {
				continue
			}
			parts := strings.Split(line, ",")
			if len(parts) < 3 {
				return uerr("each line must be: date,description,amount  (got %q)", line)
			}
			if _, ok := parseDate(strings.TrimSpace(parts[0])); !ok {
				return uerr("bad date in line %q (use YYYY-MM-DD)", line)
			}
			amt := toFloat(strings.TrimSpace(parts[len(parts)-1]))
			dir := "in"
			if amt < 0 {
				dir, amt = "out", -amt
			}
			ref := ""
			if len(parts) > 3 {
				ref = strings.TrimSpace(parts[2])
			}
			mk(tx, "bank_imports", acc.Branch, map[string]any{"account": float64(acc.ID), "date": strings.TrimSpace(parts[0]), "description": strings.TrimSpace(parts[1]),
				"amount": float64(int64(math.Round(amt * 100))), "direction": dir, "reference": ref, "status": "unmatched"})
			n++
		}
		return nil
	})
	if err != nil {
		writeErr(w, 400, friendlyErr(err))
		return
	}
	bumpRes("bank_imports")
	a.audit(r, "bank_imports", 0, "import", fmt.Sprintf("%d lines by %s", n, c.User))
	writeJSON(w, 200, map[string]int{"imported": n})
}

func (a *App) handleEInvoiceFromOrder(w http.ResponseWriter, r *http.Request) {
	var in struct {
		OrderID int64 `json:"order_id"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	var id int64
	err := a.tx(func(tx *sql.Tx) error {
		o, err := loadOrder(tx, in.OrderID, false)
		if err != nil || o.PayStatus != "paid" {
			return uerr("choose a paid order")
		}
		var n int
		tx.QueryRow(`SELECT COUNT(*) FROM records WHERE res='e_invoices' AND json_extract(data,'$.order')=?`, o.No).Scan(&n)
		if n > 0 {
			return uerr("an invoice already exists for this order")
		}
		id = mk(tx, "e_invoices", o.Branch, map[string]any{"no": docNo(tx, "e_invoices", "INV"), "order": o.No, "customer": o.Customer, "date": today(), "subtotal": float64(o.Subtotal - o.Discount),
			"tax": float64(o.Tax), "total": float64(o.Total), "status": "draft"})
		return nil
	})
	if err != nil {
		writeErr(w, 400, friendlyErr(err))
		return
	}
	bumpRes("e_invoices")
	writeJSON(w, 201, map[string]int64{"id": id})
}

// GET /api/acc/budget-vs-actual?budget=<id>
func (a *App) handleBudgetActual(w http.ResponseWriter, r *http.Request) {
	b := scopeBranch(userFrom(r), r)
	bud, err := loadRec(a.db.rdb, qInt(r, "budget"))
	if err != nil || bud.Res != "budgets" {
		list := []map[string]any{}
		for _, x := range recs(a.db.rdb, "budgets", b) {
			list = append(list, map[string]any{"id": x.ID, "name": x.S("name"), "status": x.Status})
		}
		writeJSON(w, 200, map[string]any{"budgets": list})
		return
	}
	bal := map[int64]AccBal{}
	for _, x := range a.balances(a.db.rdb, b, bud.S("from"), bud.S("to")) {
		bal[x.ID] = x
	}
	rows := []map[string]any{}
	var tb, ta int64
	for _, l := range bud.L("lines") {
		id := toInt(l["account"])
		x := bal[id]
		budget := toInt(l["amount"])
		rows = append(rows, map[string]any{"account": x.Code + " · " + x.Name, "type": x.Type, "budget": budget, "actual": x.Bal, "variance": x.Bal - budget})
		tb += budget
		ta += x.Bal
	}
	writeJSON(w, 200, map[string]any{"name": bud.S("name"), "from": bud.S("from"), "to": bud.S("to"), "rows": rows, "total_budget": tb, "total_actual": ta})
}

// journalAllowed: branch-bound users can only touch their own branch's journals.
func (a *App) journalAllowed(c *Claims, id int64) bool {
	if c.Branch == 0 {
		return true
	}
	var b int64
	return a.db.rdb.QueryRow(`SELECT branch_id FROM journals WHERE id=?`, id).Scan(&b) == nil && b == c.Branch
}
