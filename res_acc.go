package main

import (
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

var expenseAccts = []string{"general_expense", "rent", "utilities", "marketing", "repairs", "bank_fees", "driver_expense", "payroll_expense", "depreciation"}

func payAcct(method string) string {
	switch method {
	case "cash":
		return "cash"
	case "petty":
		return "petty_cash"
	}
	return "bank"
}

func expenseFor(cat string) string {
	switch cat {
	case "utilities":
		return "utilities"
	case "marketing":
		return "marketing"
	case "repairs":
		return "repairs"
	}
	return "general_expense"
}

func init() {
	// Chart of accounts is stored in a typed table; this entry only lets other forms pick an account.
	def("accounts", "accounting", "Chart of accounts", `code*^; name*^~; type:select(asset|liability|equity|revenue|cogs|expense)*^`, NoCreate, NoEdit, NoDelete, Label("{code} · {name}"))

	// ---------------- restaurant accounting ----------------
	def("sales_summaries", "accounting", "Sales summaries",
		`date:date*^; orders:int.^; gross:money.^; discounts:money.; net:money.^; tax:money.; tips:money.; cash:money.; card:money.; other:money.; ledger_sales:money.; difference:money.^; note; status:select(draft|reviewed|posted)^=draft`,
		Scoped, Label("Summary {date}"), Hint("A daily roll-up of sales and payments. Reviewing it checks the day's sales against the ledger."),
		BeforeSave(func(a *App, tx *sql.Tx, c *Claims, r *Rec, old *Rec) error {
			if old != nil && old.Status == "posted" {
				return uerr("a posted summary cannot be changed")
			}
			branch := firstNonZero(r.Branch, c.Branch)
			r.Branch = branch
			day, ok := parseDate(r.S("date"))
			if !ok {
				return uerr("choose a date")
			}
			from, to := day.Unix(), day.AddDate(0, 0, 1).Unix()
			bw, args := "", []any{from, to}
			if branch != 0 {
				bw = " AND branch_id=?"
				args = append(args, branch)
			}
			var n, gross, disc, tax, tips int64
			tx.QueryRow(`SELECT COUNT(*),COALESCE(SUM(subtotal),0),COALESCE(SUM(discount),0),COALESCE(SUM(tax),0),COALESCE(SUM(tip),0) FROM orders WHERE pay_status='paid' AND created_at>=? AND created_at<?`+bw, args...).Scan(&n, &gross, &disc, &tax, &tips)
			var cash, card, other int64
			rows, _ := tx.Query(`SELECT method,SUM(CASE WHEN kind='payment' THEN amount ELSE -amount END) FROM payments WHERE created_at>=? AND created_at<?`+bw+` GROUP BY method`, args...)
			for rows.Next() {
				var m string
				var v int64
				rows.Scan(&m, &v)
				switch m {
				case "cash":
					cash += v
				case "card":
					card += v
				default:
					other += v
				}
			}
			rows.Close()
			r.Set("orders", float64(n))
			r.Set("gross", float64(gross))
			r.Set("discounts", float64(disc))
			r.Set("net", float64(gross-disc))
			r.Set("tax", float64(tax))
			r.Set("tips", float64(tips))
			r.Set("cash", float64(cash))
			r.Set("card", float64(card))
			r.Set("other", float64(other))
			var ledger int64
			if id, ok := a.acct("sales"); ok {
				lw, largs := "account_id=? AND date=?", []any{id, r.S("date")}
				if branch != 0 {
					lw += " AND branch_id=?"
					largs = append(largs, branch)
				}
				tx.QueryRow(`SELECT COALESCE(SUM(credit-debit),0) FROM journal_lines WHERE `+lw, largs...).Scan(&ledger)
			}
			r.Set("ledger_sales", float64(ledger))
			r.Set("difference", float64(gross-disc-ledger))
			return nil
		}),
		Act("review", "Mark reviewed", LvManage, S("draft"), "reviewed", "pri"),
		Act("post", "Approve", LvApprove, S("reviewed"), "posted", "ok"))
	def("summary_policies", "accounting", "Sales summary policies", `name*^~; cadence:select(daily|weekly)=daily^; include_tips:bool=true; auto_create:bool; status:select(active|inactive)^=active`, Scoped)
	def("production_batches", "accounting", "Production batches",
		`name*^~; date:date*^; output_qty:number*^; output_unit:text; lines:lines(ingredient:ref(ingredients)*,qty:number*)*; cost:money.^; unit_cost:money.; status:select(planned|done)^=planned; note`, Scoped,
		Hint("Making sauces, doughs or marinades in bulk. Completing a batch uses up the ingredients and records its cost."),
		BeforeSave(func(a *App, tx *sql.Tx, c *Claims, r *Rec, old *Rec) error {
			var cost float64
			for _, l := range r.L("lines") {
				cost += toFloat(l["qty"]) * float64(a.ingCost(tx, toInt(l["ingredient"])))
			}
			r.Set("cost", cost)
			if q := r.N("output_qty"); q > 0 {
				r.Set("unit_cost", float64(int64(cost/q)))
			}
			return nil
		}),
		ActFn("complete", "Complete batch", LvManage, S("planned"), "ok", func(a *App, tx *sql.Tx, c *Claims, r *Rec, p map[string]any) error {
			branch := firstNonZero(r.Branch, c.Branch, a.defaultBranch(tx))
			for _, l := range r.L("lines") {
				ing := toInt(l["ingredient"])
				if err := a.stockMove(tx, branch, ing, -toFloat(l["qty"]), a.ingCost(tx, ing), "production", fmt.Sprintf("batch:%d", r.ID), r.S("name"), c.UID); err != nil {
					return err
				}
			}
			r.Status = "done"
			return nil
		}))
	def("channel_settlements", "accounting", "Channel settlements",
		`channel:select(card|aggregator|delivery_partner)*^; name~; from:date*; to:date*; gross:money*^; fee:money=0; net:money.^; status:select(pending|settled)^=pending; note`, Scoped, Label("{channel} {from}"),
		Hint("Money that arrives from card machines or delivery apps days after the sale, less their fees."),
		BeforeSave(func(a *App, tx *sql.Tx, c *Claims, r *Rec, old *Rec) error {
			r.Set("net", float64(r.I("gross")-r.I("fee")))
			return nil
		}),
		ActFn("settle", "Money received", LvApprove, S("pending"), "ok", func(a *App, tx *sql.Tx, c *Claims, r *Rec, p map[string]any) error {
			r.Status = "settled"
			from := "card_clearing"
			if r.S("channel") != "card" {
				from = "ar"
			}
			if !a.autoPost() {
				return nil
			}
			_, err := a.post(tx, r.Branch, today(), fmt.Sprintf("channel:%d", r.ID), "Settlement "+r.S("channel"), c.UID, Dr("bank", r.I("net")), Dr("bank_fees", r.I("fee")), Cr(from, r.I("gross")))
			return err
		}))

	// ---------------- AR, AP & treasury ----------------
	def("customer_accounts", "accounting", "Customer accounts", `customer:ref(customers)*^#; credit_limit:money=0^; terms:select(cod|net7|net15|net30)=net15; balance:money.^=0; status:select(active|on_hold|closed)^=active; note`, Scoped, Label("Account #{id}"),
		Hint("House accounts for regular customers who pay later. Charge an order to an account in the POS; record payments under Cash collections."))
	def("cash_collections", "accounting", "Cash collections", `customer:ref(customers)*^; date:date*^; amount:money*^; method:select(cash|bank|cheque|card)=cash; reference; note; status:select(posted)=posted`, Scoped, Label("Receipt #{id}"),
		AfterSave(func(a *App, tx *sql.Tx, c *Claims, r *Rec, old *Rec) error {
			if old != nil {
				return nil
			}
			tx.Exec(`UPDATE records SET data=json_set(data,'$.balance',COALESCE(json_extract(data,'$.balance'),0)-?) WHERE res='customer_accounts' AND ref=?`, r.I("amount"), fmt.Sprint(r.I("customer")))
			bumpLater("customer_accounts")
			return a.postSimple(tx, r.Branch, r.S("date"), fmt.Sprintf("collection:%d", r.ID), "Customer payment", payAcct(r.S("method")), "ar", r.I("amount"), c.UID)
		}))
	def("payment_proposals", "accounting", "Payment proposals", `name*^~; pay_date:date*^; lines:lines(bill:ref(supplier_bills)*,amount:money*)*; total:money.^; method:select(bank|cash)=bank; status:select(draft|approved|executed)^=draft`, Scoped,
		Hint("Choose which supplier bills to pay this week. Executing it records all the payments at once."),
		BeforeSave(func(a *App, tx *sql.Tx, c *Claims, r *Rec, old *Rec) error {
			var t int64
			for _, l := range r.L("lines") {
				t += toInt(l["amount"])
			}
			r.Set("total", float64(t))
			return nil
		}),
		Act("approve", "Approve", LvApprove, S("draft"), "approved", "ok"),
		ActFn("execute", "Execute payments", LvApprove, S("approved"), "ok", func(a *App, tx *sql.Tx, c *Claims, r *Rec, p map[string]any) error {
			for _, l := range r.L("lines") {
				bill, err := loadRec(tx, toInt(l["bill"]))
				if err != nil || bill.Res != "supplier_bills" {
					continue
				}
				if err := a.payBill(tx, c, bill, toInt(l["amount"]), r.S("method"), today(), "proposal "+r.S("name")); err != nil {
					return err
				}
			}
			r.Status = "executed"
			return nil
		}), Confirm("execute", "Pay all these bills now?"))
	def("expense_claims", "accounting", "Expense claims", `employee:ref(employees)*^; date:date*^; category:select(travel|meals|supplies|utilities|marketing|repairs|other)=other^; description*^~; amount:money*^; receipt:file; status:select(submitted|approved|rejected|paid)^=submitted`, Scoped, Label("Claim #{id}"),
		Act("approve", "Approve", LvApprove, S("submitted"), "approved", "ok"), Act("reject", "Reject", LvApprove, S("submitted"), "rejected", "bad"),
		ActFn("pay", "Pay", LvApprove, S("approved"), "ok", func(a *App, tx *sql.Tx, c *Claims, r *Rec, p map[string]any) error {
			r.Status = "paid"
			return a.postSimple(tx, r.Branch, today(), fmt.Sprintf("claim:%d", r.ID), "Expense claim: "+r.S("description"), expenseFor(r.S("category")), payAcct(toStr(p["method"])), r.I("amount"), c.UID)
		}))
	def("petty_cash", "accounting", "Petty cash", `date:date*^; kind:select(expense|replenish)*^; category:select(supplies|transport|cleaning|repairs|other)=other; description*^~; amount:money*^; status:select(posted)=posted`, Scoped, Label("Petty #{id}"),
		Hint("Small day-to-day cash spending from the petty cash box."),
		AfterSave(func(a *App, tx *sql.Tx, c *Claims, r *Rec, old *Rec) error {
			if old != nil {
				return nil
			}
			if r.S("kind") == "replenish" {
				return a.postSimple(tx, r.Branch, r.S("date"), fmt.Sprintf("petty:%d", r.ID), "Petty cash top-up", "petty_cash", "cash", r.I("amount"), c.UID)
			}
			return a.postSimple(tx, r.Branch, r.S("date"), fmt.Sprintf("petty:%d", r.ID), "Petty cash: "+r.S("description"), expenseFor(r.S("category")), "petty_cash", r.I("amount"), c.UID)
		}))
	def("cheques", "accounting", "Cheques", `no|Cheque no.:text*^~; kind:select(issued|received)*^; party*^~; amount:money*^; date:date*^; due:date^; bank; status:select(pending|cleared|bounced|cancelled)^=pending`, Scoped, Label("{no}"),
		ActFn("clear", "Mark cleared", LvManage, S("pending"), "ok", func(a *App, tx *sql.Tx, c *Claims, r *Rec, p map[string]any) error {
			r.Status = "cleared"
			if r.S("kind") == "issued" {
				return a.postSimple(tx, r.Branch, today(), fmt.Sprintf("cheque:%d", r.ID), "Cheque "+r.S("no")+" cleared", "ap", "bank", r.I("amount"), c.UID)
			}
			return a.postSimple(tx, r.Branch, today(), fmt.Sprintf("cheque:%d", r.ID), "Cheque "+r.S("no")+" cleared", "bank", "ar", r.I("amount"), c.UID)
		}),
		Act("bounce", "Bounced", LvManage, S("pending"), "bounced", "bad"), Act("cancel", "Cancel", LvManage, S("pending"), "cancelled", "bad"))

	// ---------------- purchasing accounting ----------------
	def("supplier_bills", "accounting", "Supplier bills", `no|Bill no.:text~^; supplier:ref(suppliers)*^; purchase:ref(purchases); date:date*^; due:date^; amount:money*^; paid:money.^=0; posted:bool.; note; status:select(open|partial|paid|void)^=open`, Scoped, Label("{no}"),
		Hint("What you owe suppliers. Receiving a purchase creates its bill automatically; add other bills (rent, repairs ...) by hand."),
		AfterSave(func(a *App, tx *sql.Tx, c *Claims, r *Rec, old *Rec) error {
			if old == nil && !r.B("posted") {
				r.Set("posted", true)
				if err := saveRec(tx, r); err != nil {
					return err
				}
				return a.postSimple(tx, r.Branch, r.S("date"), fmt.Sprintf("bill:%d", r.ID), "Supplier bill "+r.S("no"), "general_expense", "ap", r.I("amount"), c.UID)
			}
			return nil
		}),
		ActFn("void", "Void bill", LvApprove, S("open"), "bad", func(a *App, tx *sql.Tx, c *Claims, r *Rec, p map[string]any) error {
			if r.I("paid") > 0 {
				return uerr("this bill already has payments")
			}
			r.Status = "void"
			if r.I("purchase") == 0 {
				return a.postSimple(tx, r.Branch, today(), fmt.Sprintf("bill:%d", r.ID), "Void bill "+r.S("no"), "ap", "general_expense", r.I("amount"), c.UID)
			}
			return a.postSimple(tx, r.Branch, today(), fmt.Sprintf("bill:%d", r.ID), "Void bill "+r.S("no"), "ap", "inventory", r.I("amount"), c.UID)
		}))
	def("supplier_payments", "accounting", "Supplier payments", `supplier:ref(suppliers)*^; bill:ref(supplier_bills); date:date*^; amount:money*^; method:select(bank|cash|cheque|card)=bank; reference; status:select(posted|void)^=posted`, Scoped, Label("Payment #{id}"),
		Hint("Pay a supplier. Pick a bill, or leave it empty to pay the oldest open bills first."),
		AfterSave(func(a *App, tx *sql.Tx, c *Claims, r *Rec, old *Rec) error {
			if old != nil {
				return nil
			}
			remaining := r.I("amount")
			var bills []*Rec
			if b := r.I("bill"); b > 0 {
				if bill, err := loadRec(tx, b); err == nil {
					bills = append(bills, bill)
				}
			} else {
				for _, b := range recs(tx, "supplier_bills", r.Branch) {
					if b.I("supplier") == r.I("supplier") && (b.Status == "open" || b.Status == "partial") {
						bills = append(bills, b)
					}
				}
			}
			for _, b := range bills {
				if remaining <= 0 {
					break
				}
				due := b.I("amount") - b.I("paid")
				pay := int64(math.Min(float64(due), float64(remaining)))
				if pay <= 0 {
					continue
				}
				b.Set("paid", float64(b.I("paid")+pay))
				b.Status = "partial"
				if b.I("paid") >= b.I("amount") {
					b.Status = "paid"
				}
				saveRec(tx, b)
				if pid := b.I("purchase"); pid > 0 {
					tx.Exec(`UPDATE records SET data=json_set(data,'$.paid',COALESCE(json_extract(data,'$.paid'),0)+?) WHERE id=?`, pay, pid)
				}
				remaining -= pay
			}
			bumpLater("supplier_bills")
			return a.postSimple(tx, r.Branch, r.S("date"), fmt.Sprintf("supplier_payment:%d", r.ID), "Supplier payment", "ap", payAcct(r.S("method")), r.I("amount"), c.UID)
		}))
	def("credit_notes", "accounting", "Supplier credit notes", `no|Credit note no.:text~^; supplier:ref(suppliers)*^; bill:ref(supplier_bills); date:date*^; amount:money*^; reason; status:select(draft|applied)^=draft`, Scoped, Label("{no}"),
		ActFn("apply", "Apply", LvApprove, S("draft"), "ok", func(a *App, tx *sql.Tx, c *Claims, r *Rec, p map[string]any) error {
			r.Status = "applied"
			if b, err := loadRec(tx, r.I("bill")); err == nil && b.Res == "supplier_bills" {
				b.Set("paid", float64(b.I("paid")+r.I("amount")))
				if b.I("paid") >= b.I("amount") {
					b.Status = "paid"
				} else {
					b.Status = "partial"
				}
				saveRec(tx, b)
			}
			return a.postSimple(tx, r.Branch, r.S("date"), fmt.Sprintf("creditnote:%d", r.ID), "Supplier credit "+r.S("no"), "ap", "inventory", r.I("amount"), c.UID)
		}))

	// ---------------- banking & cash ----------------
	def("fin_accounts", "accounting", "Financial accounts", `name*^~; kind:select(cash|bank|card|wallet|petty)*^; ledger:select(cash|bank|petty_cash|card_clearing)=bank^; number|Account / IBAN no.:text; currency:text=INR; opening_balance:money=0; status:select(active|inactive)^=active`, Scoped,
		Hint("Your cash drawers and bank accounts. Each one maps to a ledger account."))
	def("cash_transfers", "accounting", "Cash transfers", `date:date*^; from:ref(fin_accounts)*^; to:ref(fin_accounts)*^; amount:money*^; reference; note; status:select(posted|void)^=posted`, Scoped, Label("Transfer #{id}"),
		Hint("Move money between your own accounts, e.g. depositing the day's cash at the bank."),
		BeforeSave(func(a *App, tx *sql.Tx, c *Claims, r *Rec, old *Rec) error {
			if r.I("from") == r.I("to") {
				return uerr("choose two different accounts")
			}
			return nil
		}),
		AfterSave(func(a *App, tx *sql.Tx, c *Claims, r *Rec, old *Rec) error {
			if old != nil {
				return nil
			}
			f, e1 := loadRec(tx, r.I("from"))
			t, e2 := loadRec(tx, r.I("to"))
			if e1 != nil || e2 != nil {
				return uerr("choose the accounts")
			}
			return a.postSimple(tx, r.Branch, r.S("date"), fmt.Sprintf("transfer_cash:%d", r.ID), "Transfer "+f.S("name")+" to "+t.S("name"), t.S("ledger"), f.S("ledger"), r.I("amount"), c.UID)
		}))
	def("bank_imports", "accounting", "Bank imports", `account:ref(fin_accounts)*^; date:date*^; description*^~; amount:money*^; direction:select(in|out)=in^; reference; status:select(unmatched|matched|ignored)^=unmatched; journal:int.`, Scoped, Label("Line #{id}"),
		Hint("Lines from your bank statement. Use 'Import statement' on the Bank imports page to paste a CSV (date,description,amount)."),
		ActFn("match", "Auto-match", LvManage, S("unmatched"), "ok", func(a *App, tx *sql.Tx, c *Claims, r *Rec, p map[string]any) error {
			acc, err := loadRec(tx, r.I("account"))
			if err != nil {
				return uerr("account not found")
			}
			aid, _ := a.acct(acc.S("ledger"))
			col := "debit"
			if r.S("direction") == "out" {
				col = "credit"
			}
			var jid int64
			err = tx.QueryRow(`SELECT journal_id FROM journal_lines WHERE account_id=? AND `+col+`=? AND date BETWEEN date(?,'-5 day') AND date(?,'+5 day') LIMIT 1`, aid, r.I("amount"), r.S("date"), r.S("date")).Scan(&jid)
			if err != nil {
				return uerr("no ledger entry of that amount near this date")
			}
			r.Set("journal", float64(jid))
			r.Status = "matched"
			return nil
		}),
		Act("ignore", "Ignore", LvManage, S("unmatched"), "ignored", "bad"))
	def("bank_recs", "accounting", "Bank reconciliations", `account:ref(fin_accounts)*^; statement_date:date*^; statement_balance:money*^; book_balance:money.^; difference:money.^; note; status:select(open|reconciled)^=open`, Scoped, Label("Rec #{id}"),
		Hint("Check that your books match the bank statement. A reconciliation closes only when the difference is zero."),
		BeforeSave(func(a *App, tx *sql.Tx, c *Claims, r *Rec, old *Rec) error {
			acc, err := loadRec(tx, r.I("account"))
			if err != nil {
				return uerr("choose an account")
			}
			id, _ := a.acct(acc.S("ledger"))
			var dr, cr int64
			tx.QueryRow(`SELECT COALESCE(SUM(l.debit),0),COALESCE(SUM(l.credit),0) FROM journal_lines l JOIN journals j ON j.id=l.journal_id AND j.status<>'draft' WHERE l.account_id=? AND l.date<=?`, id, r.S("statement_date")).Scan(&dr, &cr)
			book := dr - cr + acc.I("opening_balance")
			r.Set("book_balance", float64(book))
			r.Set("difference", float64(r.I("statement_balance")-book))
			return nil
		}),
		ActFn("reconcile", "Mark reconciled", LvApprove, S("open"), "ok", func(a *App, tx *sql.Tx, c *Claims, r *Rec, p map[string]any) error {
			if r.I("difference") != 0 {
				return uerr("the difference is %.2f - fix it before reconciling", float64(r.I("difference"))/100)
			}
			r.Status = "reconciled"
			return nil
		}))
	def("acc_exports", "accounting", "Accounting exports", `name*^~; type:select(trial_balance|general_ledger|journals|profit_loss|balance_sheet)^; from:date; to:date; rows:int^; user^`, NoCreate, Label("{name}"))

	// ---------------- calendar ----------------
	def("fiscal_years", "accounting", "Fiscal years", `name*^~; from:date*^; to:date*^; status:select(open|closed)^=open`, Asc,
		Hint("Your accounting year. Closing it moves the year's profit into retained earnings and locks every period."),
		ActFn("generate", "Create 12 periods", LvManage, S("open"), "", func(a *App, tx *sql.Tx, c *Claims, r *Rec, p map[string]any) error {
			f, ok := parseDate(r.S("from"))
			if !ok {
				return uerr("invalid dates")
			}
			for i := 0; i < 12; i++ {
				s := f.AddDate(0, i, 0)
				e := s.AddDate(0, 1, -1)
				name := s.Format("Jan 2006")
				var n int
				tx.QueryRow(`SELECT COUNT(*) FROM records WHERE res='periods' AND json_extract(data,'$.from')=?`, s.Format("2006-01-02")).Scan(&n)
				if n == 0 {
					mk(tx, "periods", 0, map[string]any{"name": name, "fiscal_year": float64(r.ID), "from": s.Format("2006-01-02"), "to": e.Format("2006-01-02"), "status": "open"})
				}
			}
			bumpLater("periods")
			return nil
		}),
		ActFn("close", "Close year", LvApprove, S("open"), "bad", func(a *App, tx *sql.Tx, c *Claims, r *Rec, p map[string]any) error {
			bal := a.balances(tx, 0, r.S("from"), r.S("to"))
			var lines []JLine
			var net int64
			for _, b := range bal {
				switch b.Type {
				case "revenue":
					if b.Bal != 0 {
						lines = append(lines, Dr(b.Code, b.Bal))
						net += b.Bal
					}
				case "cogs", "expense":
					if b.Bal != 0 {
						lines = append(lines, Cr(b.Code, b.Bal))
						net -= b.Bal
					}
				}
			}
			lines = append(lines, Cr("retained", net))
			if _, err := a.post(tx, 0, r.S("to"), fmt.Sprintf("yearclose:%d", r.ID), "Close fiscal year "+r.S("name"), c.UID, lines...); err != nil {
				return err
			}
			tx.Exec(`UPDATE records SET status='closed' WHERE res='periods' AND json_extract(data,'$.fiscal_year')=?`, r.ID)
			r.Status = "closed"
			bumpLater("periods")
			return nil
		}), Confirm("close", "Close this fiscal year? Every period will be locked."))
	def("periods", "accounting", "Periods", `name*^~; fiscal_year:ref(fiscal_years)^; from:date*^; to:date*^; status:select(open|soft_locked|closed)^=open`, Asc,
		Hint("Open = anyone can post. Soft-locked = only accounting managers. Closed = nobody."),
		Act("lock", "Soft lock", LvApprove, S("open"), "soft_locked", ""), Act("close", "Close", LvApprove, S("open", "soft_locked"), "closed", "bad"),
		Act("reopen", "Reopen", LvApprove, S("soft_locked", "closed"), "open", "ok"))

	// ---------------- tax ----------------
	def("tax_returns", "accounting", "Tax returns", `name*^~; from:date*^; to:date*^; output_tax:money.^; input_tax:money.^; payable:money.^; note; status:select(draft|filed|paid)^=draft`, Label("{name}"),
		Hint("Sales tax collected minus tax paid on purchases, calculated from the ledger."),
		BeforeSave(func(a *App, tx *sql.Tx, c *Claims, r *Rec, old *Rec) error {
			if old != nil && old.Status != "draft" {
				return uerr("a filed return cannot be changed")
			}
			var out, in int64
			if id, ok := a.acct("tax_payable"); ok {
				tx.QueryRow(`SELECT COALESCE(SUM(credit-debit),0) FROM journal_lines WHERE account_id=? AND date>=? AND date<=?`, id, r.S("from"), r.S("to")).Scan(&out)
			}
			if id, ok := a.acct("tax_input"); ok {
				tx.QueryRow(`SELECT COALESCE(SUM(debit-credit),0) FROM journal_lines WHERE account_id=? AND date>=? AND date<=?`, id, r.S("from"), r.S("to")).Scan(&in)
			}
			r.Set("output_tax", float64(out))
			r.Set("input_tax", float64(in))
			r.Set("payable", float64(out-in))
			return nil
		}),
		Act("file", "Mark filed", LvApprove, S("draft"), "filed", "pri"),
		ActFn("pay", "Record payment", LvApprove, S("filed"), "ok", func(a *App, tx *sql.Tx, c *Claims, r *Rec, p map[string]any) error {
			r.Status = "paid"
			if !a.autoPost() {
				return nil
			}
			_, err := a.post(tx, 0, today(), fmt.Sprintf("taxreturn:%d", r.ID), "Tax payment "+r.S("name"), c.UID, Dr("tax_payable", r.I("output_tax")), Cr("tax_input", r.I("input_tax")), JLine{Acct: "bank", Dr: max(0, r.I("input_tax")-r.I("output_tax")), Cr: max(0, r.I("output_tax")-r.I("input_tax"))})
			return err
		}))
	def("withholding_rules", "accounting", "Withholding rules", `name*^~; applies_to:select(supplier_payment|service|rent|professional)*^; rate|Rate %:number*^; threshold:money=0; status:select(active|inactive)^=active`)
	def("withholding_tax", "accounting", "Withholding tax", `supplier:ref(suppliers)*^; rule:ref(withholding_rules)*^; date:date*^; base:money*^; rate:number.^; amount:money.^; status:select(withheld|remitted)^=withheld`, Scoped, Label("WHT #{id}"),
		BeforeSave(func(a *App, tx *sql.Tx, c *Claims, r *Rec, old *Rec) error {
			rule, err := loadRec(tx, r.I("rule"))
			if err != nil || rule.Res != "withholding_rules" {
				return uerr("choose a withholding rule")
			}
			r.Set("rate", rule.N("rate"))
			r.Set("amount", float64(int64(float64(r.I("base"))*rule.N("rate")/100)))
			return nil
		}),
		AfterSave(func(a *App, tx *sql.Tx, c *Claims, r *Rec, old *Rec) error {
			if old == nil {
				return a.postSimple(tx, r.Branch, r.S("date"), fmt.Sprintf("wht:%d", r.ID), "Tax withheld from supplier", "ap", "wht_payable", r.I("amount"), c.UID)
			}
			return nil
		}),
		ActFn("remit", "Remit to authority", LvApprove, S("withheld"), "ok", func(a *App, tx *sql.Tx, c *Claims, r *Rec, p map[string]any) error {
			r.Status = "remitted"
			return a.postSimple(tx, r.Branch, today(), fmt.Sprintf("wht:%d", r.ID), "Withholding remitted", "wht_payable", "bank", r.I("amount"), c.UID)
		}))
	def("e_invoices", "accounting", "Electronic invoices", `no|Invoice no.:text#^; order|Order no.:text~^; customer:text^~; date:date*^; subtotal:money; tax:money; total:money^; uuid:text.; hash:text.; status:select(draft|issued|submitted|rejected|cancelled)^=draft`, Scoped, Label("{no}"),
		Hint("Tamper-evident invoices: each one carries a unique ID and a hash chained to the previous invoice. Connecting to a tax authority portal needs that country's credentials."),
		ActFn("issue", "Issue", LvManage, S("draft"), "ok", func(a *App, tx *sql.Tx, c *Claims, r *Rec, p map[string]any) error {
			var prev string
			tx.QueryRow(`SELECT COALESCE(json_extract(data,'$.hash'),'') FROM records WHERE res='e_invoices' AND status IN ('issued','submitted') ORDER BY id DESC LIMIT 1`).Scan(&prev)
			r.Set("uuid", randHex(8)+"-"+randHex(4)+"-"+randHex(4))
			r.Set("hash", chainHash(prev, r))
			r.Status = "issued"
			return nil
		}),
		Act("submit", "Mark submitted", LvManage, S("issued"), "submitted", "pri"), Act("cancel", "Cancel", LvApprove, S("draft", "issued"), "cancelled", "bad"))

	// ---------------- planning & assets ----------------
	def("forecasts", "accounting", "Plans & forecasts", `name*^~; from:date*^; to:date*^; basis:select(manual|last_year|trend)=manual; lines:lines(account:ref(accounts)*,amount:money*,note)*; total:money.^; status:select(draft|active|archived)^=draft`, Scoped,
		BeforeSave(func(a *App, tx *sql.Tx, c *Claims, r *Rec, old *Rec) error {
			var t int64
			for _, l := range r.L("lines") {
				t += toInt(l["amount"])
			}
			r.Set("total", float64(t))
			return nil
		}))
	def("budgets", "accounting", "Budgets", `name*^~; fiscal_year:ref(fiscal_years)^; from:date*; to:date*; lines:lines(account:ref(accounts)*,amount|Budget for the period:money*)*; total:money.^; status:select(draft|approved)^=draft`, Scoped,
		Hint("Planned spending and income per account. Approved budgets appear in Financial reports → Budget vs actual."),
		BeforeSave(func(a *App, tx *sql.Tx, c *Claims, r *Rec, old *Rec) error {
			var t int64
			for _, l := range r.L("lines") {
				t += toInt(l["amount"])
			}
			r.Set("total", float64(t))
			return nil
		}), Act("approve", "Approve", LvApprove, S("draft"), "approved", "ok"))
	def("allocations", "accounting", "Allocations", `name*^~; source:ref(accounts)*^; basis:select(equal|fixed_pct)=fixed_pct; targets:lines(branch:ref(branches)*,pct:number*)*; status:select(active|inactive)^=active`,
		Hint("Spread a head-office cost across branches. 'Run' for a date range posts the allocation."),
		ActFn("run", "Run for a period", LvApprove, S("active"), "pri", func(a *App, tx *sql.Tx, c *Claims, r *Rec, p map[string]any) error {
			from, to := toStr(p["from"]), toStr(p["to"])
			if from == "" || to == "" {
				return uerr("send from and to dates")
			}
			src := a.acctInfo(r.I("source"))
			var bal int64
			tx.QueryRow(`SELECT COALESCE(SUM(debit-credit),0) FROM journal_lines WHERE account_id=? AND date>=? AND date<=? AND branch_id=0`, src.ID, from, to).Scan(&bal)
			if bal <= 0 {
				return uerr("there is nothing unallocated on %s for that period", src.Name)
			}
			var lines []JLine
			var used int64
			tg := r.L("targets")
			for i, t := range tg {
				pct := toFloat(t["pct"])
				if r.S("basis") == "equal" {
					pct = 100 / float64(len(tg))
				}
				share := int64(float64(bal) * pct / 100)
				if i == len(tg)-1 {
					share = bal - used
				}
				used += share
				lines = append(lines, JLine{Acct: src.Code, Dr: share, Memo: fmt.Sprintf("branch %d", toInt(t["branch"]))})
			}
			lines = append(lines, JLine{Acct: src.Code, Cr: bal, Memo: "allocated out"})
			_, err := a.post(tx, 0, to, fmt.Sprintf("allocation:%d", r.ID), "Allocation "+r.S("name"), c.UID, lines...)
			return err
		}))
	def("asset_policies", "accounting", "Asset policies", `name*^~; method:select(straight_line|declining_balance)=straight_line^; life_years:int*=5^; salvage_pct|Salvage value %:number=0; rate|Declining rate %:number=20; status:select(active|inactive)^=active`)
	def("fixed_assets", "accounting", "Fixed assets", `name*^~; tag|Asset tag:text#^; policy:ref(asset_policies)*^; cost:money*^; purchase_date:date*^; funded_by:select(none|bank|cash|ap)=none; accumulated:money.^=0; book_value:money.^; last_depr:date.; status:select(active|fully_depreciated|disposed)^=active`, Scoped,
		Hint("Ovens, furniture, vehicles. Depreciation is posted month by month."),
		BeforeSave(func(a *App, tx *sql.Tx, c *Claims, r *Rec, old *Rec) error {
			if old == nil {
				r.Set("accumulated", float64(0))
			}
			r.Set("book_value", float64(r.I("cost")-r.I("accumulated")))
			return nil
		}),
		AfterSave(func(a *App, tx *sql.Tx, c *Claims, r *Rec, old *Rec) error {
			if old == nil && r.S("funded_by") != "none" && r.S("funded_by") != "" {
				return a.postSimple(tx, r.Branch, r.S("purchase_date"), fmt.Sprintf("asset:%d", r.ID), "Asset purchase "+r.S("name"), "fixed_assets", payAcctOrAP(r.S("funded_by")), r.I("cost"), c.UID)
			}
			return nil
		}),
		ActFn("depreciate", "Depreciate 1 month", LvManage, S("active"), "pri", func(a *App, tx *sql.Tx, c *Claims, r *Rec, p map[string]any) error {
			pol, err := loadRec(tx, r.I("policy"))
			if err != nil {
				return uerr("choose a depreciation policy")
			}
			cost, acc := r.I("cost"), r.I("accumulated")
			salvage := int64(float64(cost) * pol.N("salvage_pct") / 100)
			var amt int64
			if pol.S("method") == "declining_balance" {
				amt = int64(float64(cost-acc) * pol.N("rate") / 100 / 12)
			} else {
				amt = (cost - salvage) / (pol.I("life_years") * 12)
			}
			if acc+amt > cost-salvage {
				amt = cost - salvage - acc
			}
			if amt <= 0 {
				r.Status = "fully_depreciated"
				return nil
			}
			date := today()
			if ld := r.S("last_depr"); ld != "" {
				if t, ok := parseDate(ld); ok {
					date = t.AddDate(0, 1, 0).Format("2006-01-02")
				}
			} else if t, ok := parseDate(r.S("purchase_date")); ok {
				date = t.AddDate(0, 1, 0).Format("2006-01-02")
			}
			if date > today() {
				return uerr("depreciation for %s is not due yet", date[:7])
			}
			r.Set("accumulated", float64(acc+amt))
			r.Set("book_value", float64(cost-acc-amt))
			r.Set("last_depr", date)
			if acc+amt >= cost-salvage {
				r.Status = "fully_depreciated"
			}
			return a.postSimple(tx, r.Branch, date, fmt.Sprintf("depr:%d", r.ID), "Depreciation "+r.S("name"), "depreciation", "accum_dep", amt, c.UID)
		}),
		ActFn("dispose", "Dispose", LvApprove, S("active", "fully_depreciated"), "bad", func(a *App, tx *sql.Tx, c *Claims, r *Rec, p map[string]any) error {
			r.Status = "disposed"
			if !a.autoPost() {
				return nil
			}
			_, err := a.post(tx, r.Branch, today(), fmt.Sprintf("asset:%d", r.ID), "Dispose "+r.S("name"), c.UID, Dr("accum_dep", r.I("accumulated")), Dr("general_expense", r.I("cost")-r.I("accumulated")), Cr("fixed_assets", r.I("cost")))
			return err
		}), Confirm("dispose", "Write this asset off?"))
	def("costing_policies", "accounting", "Costing policies", `name*^~; method:select(average)=average^; applies_to:select(all|category)=all; include_waste:bool=true; status:select(active|inactive)^=active`,
		Hint("How ingredient cost is worked out. TableBill uses the weighted-average cost."))
	def("accruals", "accounting", "Accruals & prepayments", `kind:select(accrual|prepayment)*^; description*^~; amount:money*^; start:date*; months:int=1; expense:select(general_expense|rent|utilities|marketing|repairs)=general_expense; recognised:int.^=0; status:select(draft|active|completed)^=draft`, Scoped, Label("{description}"),
		Hint("Prepayment: paid now, expensed over several months. Accrual: expense known now, paid later."),
		ActFn("activate", "Activate", LvApprove, S("draft"), "ok", func(a *App, tx *sql.Tx, c *Claims, r *Rec, p map[string]any) error {
			r.Status = "active"
			if r.S("kind") == "prepayment" {
				return a.postSimple(tx, r.Branch, r.S("start"), fmt.Sprintf("accrual:%d", r.ID), "Prepaid "+r.S("description"), "prepaid", "bank", r.I("amount"), c.UID)
			}
			return a.postSimple(tx, r.Branch, r.S("start"), fmt.Sprintf("accrual:%d", r.ID), "Accrued "+r.S("description"), r.S("expense"), "accrued", r.I("amount"), c.UID)
		}),
		ActFn("recognize", "Recognise 1 month", LvManage, S("active"), "pri", func(a *App, tx *sql.Tx, c *Claims, r *Rec, p map[string]any) error {
			if r.S("kind") != "prepayment" {
				return uerr("only prepayments are recognised monthly")
			}
			n := r.I("recognised")
			if n >= r.I("months") {
				r.Status = "completed"
				return nil
			}
			per := r.I("amount") / max(r.I("months"), 1)
			if n+1 == r.I("months") {
				per = r.I("amount") - per*n
			}
			r.Set("recognised", float64(n+1))
			if n+1 >= r.I("months") {
				r.Status = "completed"
			}
			return a.postSimple(tx, r.Branch, today(), fmt.Sprintf("accrual:%d", r.ID), "Prepayment expense "+r.S("description"), r.S("expense"), "prepaid", per, c.UID)
		}),
		ActFn("settle", "Paid", LvManage, S("active"), "ok", func(a *App, tx *sql.Tx, c *Claims, r *Rec, p map[string]any) error {
			if r.S("kind") != "accrual" {
				return uerr("this applies to accruals only")
			}
			r.Status = "completed"
			return a.postSimple(tx, r.Branch, today(), fmt.Sprintf("accrual:%d", r.ID), "Paid accrual "+r.S("description"), "accrued", "bank", r.I("amount"), c.UID)
		}))
	def("landed_costs", "accounting", "Landed costs", `purchase:ref(purchases)*^; description*^~; amount:money*^; method:select(by_value|by_qty)=by_value; status:select(draft|allocated)^=draft`, Scoped, Label("{description}"),
		Hint("Freight, customs or handling that should be added to the cost of goods you bought."),
		ActFn("allocate", "Allocate to stock cost", LvApprove, S("draft"), "ok", func(a *App, tx *sql.Tx, c *Claims, r *Rec, p map[string]any) error {
			pu, err := loadRec(tx, r.I("purchase"))
			if err != nil || pu.Res != "purchases" {
				return uerr("purchase not found")
			}
			var totalW float64
			lines := pu.L("lines")
			for _, l := range lines {
				if r.S("method") == "by_qty" {
					totalW += toFloat(l["qty"])
				} else {
					totalW += toFloat(l["qty"]) * toFloat(l["cost"])
				}
			}
			if totalW == 0 {
				return uerr("the purchase has no value to allocate to")
			}
			for _, l := range lines {
				w := toFloat(l["qty"]) * toFloat(l["cost"])
				if r.S("method") == "by_qty" {
					w = toFloat(l["qty"])
				}
				share := float64(r.I("amount")) * w / totalW
				perUnit := share / math.Max(toFloat(l["qty"]), 1)
				tx.Exec(`UPDATE records SET data=json_set(data,'$.cost',CAST(json_extract(data,'$.cost')+? AS INTEGER)) WHERE id=? AND res='ingredients'`, perUnit, toInt(l["ingredient"]))
			}
			bumpLater("ingredients")
			r.Status = "allocated"
			return a.postSimple(tx, r.Branch, today(), fmt.Sprintf("landed:%d", r.ID), "Landed cost "+r.S("description"), "inventory", "ap", r.I("amount"), c.UID)
		}))
	def("inv_provisions", "accounting", "Inventory provisions", `name*^~; date:date*^; reason:select(obsolete|expiry|shrinkage|other)=expiry; amount:money*^; note; status:select(draft|posted|reversed)^=draft`, Scoped, Label("{name}"),
		Hint("Write down the value of stock that is likely to spoil or never sell."),
		ActFn("post", "Post", LvApprove, S("draft"), "ok", func(a *App, tx *sql.Tx, c *Claims, r *Rec, p map[string]any) error {
			r.Status = "posted"
			return a.postSimple(tx, r.Branch, r.S("date"), fmt.Sprintf("provision:%d", r.ID), "Provision "+r.S("name"), "inv_adjust", "inventory", r.I("amount"), c.UID)
		}),
		ActFn("reverse", "Reverse", LvApprove, S("posted"), "bad", func(a *App, tx *sql.Tx, c *Claims, r *Rec, p map[string]any) error {
			r.Status = "reversed"
			return a.postSimple(tx, r.Branch, today(), fmt.Sprintf("provision:%d", r.ID), "Reverse provision "+r.S("name"), "inventory", "inv_adjust", r.I("amount"), c.UID)
		}))
	def("fx_controls", "accounting", "Foreign exchange control", `currency:ref(currencies)*^; description*^~; foreign_amount:money*^; booked_rate:number*^; current_rate:number.^; gain_loss:money.^; date:date*; status:select(open|revalued)^=open`, Scoped, Label("{description}"),
		Hint("Foreign-currency balances (e.g. a USD bank account). Revaluing posts the gain or loss at today's rate."),
		BeforeSave(func(a *App, tx *sql.Tx, c *Claims, r *Rec, old *Rec) error {
			cur, err := loadRec(tx, r.I("currency"))
			if err != nil || cur.Res != "currencies" {
				return uerr("choose a currency")
			}
			r.Set("current_rate", cur.N("rate"))
			r.Set("gain_loss", float64(int64(float64(r.I("foreign_amount"))*(cur.N("rate")-r.N("booked_rate")))))
			return nil
		}),
		ActFn("revalue", "Post gain / loss", LvApprove, S("open"), "ok", func(a *App, tx *sql.Tx, c *Claims, r *Rec, p map[string]any) error {
			r.Status = "revalued"
			gl := r.I("gain_loss")
			if gl > 0 {
				return a.postSimple(tx, r.Branch, today(), fmt.Sprintf("fx:%d", r.ID), "FX gain "+r.S("description"), "bank", "other_income", gl, c.UID)
			}
			return a.postSimple(tx, r.Branch, today(), fmt.Sprintf("fx:%d", r.ID), "FX loss "+r.S("description"), "fx_diff", "bank", -gl, c.UID)
		}))
	def("legal_entities", "accounting", "Legal entities", `name*^~; code#^; tax_id; country; currency:text=INR; branches:multiref(branches); status:select(active|inactive)^=active`)
	def("intercompany", "accounting", "Intercompany reconciliation", `date:date*^; from_entity:ref(legal_entities)*^; to_entity:ref(legal_entities)*^; amount:money*^; description*^~; status:select(open|matched|eliminated)^=open`, Label("IC #{id}"),
		Hint("Money moved between your own companies. Matched entries are removed (eliminated) when consolidating."),
		Act("match", "Match", LvManage, S("open"), "matched", "ok"), Act("eliminate", "Eliminate", LvApprove, S("matched"), "eliminated", ""))
	def("posting_exceptions", "accounting", "Posting exceptions", `source*^; message*^; status:select(open|resolved)^=open`, NoCreate, Label("{source}"),
		Hint("Sales that could not be posted to the ledger automatically (for example a closed period). Fix the cause, then retry."),
		ActFn("retry", "Retry posting", LvManage, S("open"), "ok", func(a *App, tx *sql.Tx, c *Claims, r *Rec, p map[string]any) error {
			return a.retryPosting(tx, c, r)
		}),
		Act("resolve", "Mark resolved", LvManage, S("open"), "resolved", ""))

	// ---------------- configuration ----------------
	def("account_maps", "accounting", "Account mappings", `key:select(sales|delivery_income|service_income|tax_payable|tax_input|tips_payable|cash|bank|card_clearing|ar|ap|inventory|cogs|payroll_expense|payroll_payable|driver_expense|driver_payable|gift_liability|breakage|waste_expense|inv_adjust)*^; account:ref(accounts)*^; note`, Label("{key}"), Asc,
		Hint("Which ledger account each automatic posting uses. Change these only if you use your own chart of accounts."),
		AfterSave(func(a *App, tx *sql.Tx, c *Claims, r *Rec, old *Rec) error {
			time.AfterFunc(700*time.Millisecond, a.loadChart)
			return nil
		}))
	def("dimensions", "accounting", "Accounting dimensions", `name*^~; code#^; values:lines(code*,name*)*; status:select(active|inactive)^=active`, Hint("Extra tags for analysis, e.g. Cost centre or Project. Pick a value on any manual journal line."))
	def("dimension_rules", "accounting", "Dimension rules", `dimension:ref(dimensions)*^; applies_to:select(revenue|cogs|expense|asset|liability|equity)*^; required:bool=true^; status:select(active|inactive)^=active`, Label("Rule #{id}"),
		Hint("Make a dimension mandatory for certain account types on manual journals."))
	def("revenue_centers", "accounting", "Revenue centers", `name*^~; code#^; channel:select(all|dine_in|takeaway|delivery|pickup|drive_thru|pre_order|catering)=all^; branch:ref(branches); status:select(active|inactive)^=active`, Hint("Parts of the business you want to see revenue for."))
}

func payAcctOrAP(s string) string {
	if s == "ap" {
		return "ap"
	}
	return payAcct(s)
}

func chainHash(prev string, r *Rec) string {
	s := fmt.Sprintf("%s|%s|%s|%d|%s", prev, r.S("no"), r.S("order"), r.I("total"), r.S("uuid"))
	var h uint64 = 14695981039346656037
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return fmt.Sprintf("%016x", h)
}

// payBill records a payment against one bill.
func (a *App) payBill(tx *sql.Tx, c *Claims, bill *Rec, amt int64, method, date, ref string) error {
	due := bill.I("amount") - bill.I("paid")
	if amt > due {
		amt = due
	}
	if amt <= 0 {
		return nil
	}
	id := mk(tx, "supplier_payments", bill.Branch, map[string]any{"supplier": bill.Data["supplier"], "bill": float64(bill.ID), "date": date, "amount": float64(amt), "method": method, "reference": ref, "status": "posted"})
	_ = id
	bill2, _ := loadRec(tx, bill.ID)
	bill2.Set("paid", float64(bill2.I("paid")+amt))
	bill2.Status = "partial"
	if bill2.I("paid") >= bill2.I("amount") {
		bill2.Status = "paid"
	}
	saveRec(tx, bill2)
	if pid := bill2.I("purchase"); pid > 0 {
		tx.Exec(`UPDATE records SET data=json_set(data,'$.paid',COALESCE(json_extract(data,'$.paid'),0)+?) WHERE id=?`, amt, pid)
	}
	return a.postSimple(tx, bill.Branch, date, fmt.Sprintf("supplier_payment:%d", id), "Supplier payment "+ref, "ap", payAcct(method), amt, c.UID)
}

var _ = sort.Strings
var _ = strings.TrimSpace
