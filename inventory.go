package main

import (
	"database/sql"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"
)

// =====================================================================
//  Stock: every change goes through stockMove so there is always a ledger
//  (stock_moves) behind the on-hand number (ingredients_stock).
// =====================================================================

type ingInfo struct {
	Name    string
	Reorder float64
	Cost    int64
	Unit    int64
}

var (
	ingMu  sync.RWMutex
	ingMap       = map[int64]ingInfo{}
	ingVer int64 = -1
)

func (a *App) ing(q Querier, id int64) ingInfo {
	v := verOf("ingredients")
	ingMu.Lock()
	if v != ingVer {
		ingMap, ingVer = map[int64]ingInfo{}, v
	}
	info, ok := ingMap[id]
	ingMu.Unlock()
	if ok {
		return info
	}
	rec, err := loadRec(q, id)
	if err != nil || rec.Res != "ingredients" {
		return ingInfo{Name: "#" + strconv.FormatInt(id, 10)}
	}
	info = ingInfo{Name: rec.S("name"), Reorder: rec.N("reorder_level"), Cost: rec.I("cost"), Unit: rec.I("unit")}
	ingMu.Lock()
	ingMap[id] = info
	ingMu.Unlock()
	return info
}

func (a *App) ingCost(q Querier, id int64) int64 { return a.ing(q, id).Cost }

// bumpLater invalidates caches now and again shortly after commit.
func bumpLater(keys ...string) {
	for _, k := range keys {
		bumpRes(k)
	}
	time.AfterFunc(600*time.Millisecond, func() {
		for _, k := range keys {
			bumpRes(k)
		}
	})
}

func (a *App) defaultBranch(q Querier) int64 {
	var id int64
	q.QueryRow(`SELECT id FROM records WHERE res='branches' ORDER BY id LIMIT 1`).Scan(&id)
	return id
}

func (a *App) stockMove(q Querier, branch, ing int64, qty float64, unitCost int64, kind, ref, note string, uid int64) error {
	if qty == 0 || ing == 0 {
		return nil
	}
	var after float64
	if err := q.QueryRow(`INSERT INTO ingredients_stock(branch_id,ingredient_id,qty) VALUES(?,?,?)
		ON CONFLICT(branch_id,ingredient_id) DO UPDATE SET qty=qty+excluded.qty RETURNING qty`, branch, ing, qty).Scan(&after); err != nil {
		return err
	}
	before := after - qty
	if _, err := q.Exec(`INSERT INTO stock_moves(branch_id,ingredient_id,qty,unit_cost,kind,ref,note,created_by,created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		branch, ing, qty, unitCost, kind, ref, trunc(note, 200), uid, nowUnix()); err != nil {
		return err
	}
	if qty < 0 && a.settingBool("notifications", "low_stock", true) {
		info := a.ing(q, ing)
		if info.Reorder > 0 && before > info.Reorder && before+qty <= info.Reorder {
			a.notify(RoleManager, branch, "warn", "Low stock: "+info.Name,
				fmt.Sprintf("%.1f left (reorder level %.1f)", before+qty, info.Reorder), "#/inventory")
		}
	}
	return nil
}

// ---------- document actions ----------

func receivePurchase(a *App, tx *sql.Tx, c *Claims, r *Rec, p map[string]any) error {
	branch := r.Branch
	if branch == 0 {
		branch = c.Branch
	}
	if branch == 0 {
		branch = a.defaultBranch(tx)
		r.Branch = branch
	}
	ref := fmt.Sprintf("purchase:%d", r.ID)
	for _, l := range r.L("lines") {
		ing, qty, cost := toInt(l["ingredient"]), toFloat(l["qty"]), toInt(l["cost"])
		var onhand float64
		tx.QueryRow(`SELECT COALESCE(SUM(qty),0) FROM ingredients_stock WHERE ingredient_id=?`, ing).Scan(&onhand)
		old := a.ingCost(tx, ing)
		newCost := cost
		if onhand > 0 && onhand+qty > 0 {
			newCost = int64((float64(old)*onhand + float64(cost)*qty) / (onhand + qty))
		}
		tx.Exec(`UPDATE records SET data=json_set(data,'$.cost',?) WHERE id=? AND res='ingredients'`, newCost, ing)
		if err := a.stockMove(tx, branch, ing, qty, cost, "purchase", ref, r.S("no"), c.UID); err != nil {
			return err
		}
	}
	bumpLater("ingredients")
	r.Status = "received"
	date := r.S("date")
	if date == "" {
		date = today()
	}
	total, tax := r.I("total"), r.I("tax")
	if a.autoPost() {
		if _, err := a.post(tx, branch, date, ref, "Goods received "+r.S("no"), c.UID,
			Dr("inventory", total-tax), Dr("tax_input", tax), Cr("ap", total)); err != nil {
			return err
		}
	}
	// open a supplier bill so it can be paid from Accounting
	terms := map[string]int{"cash": 0, "net7": 7, "net15": 15, "net30": 30, "net60": 60}
	days := 30
	if sup, err := loadRec(tx, r.I("supplier")); err == nil {
		if d, ok := terms[sup.S("payment_terms")]; ok {
			days = d
		}
	}
	due := time.Now().AddDate(0, 0, days).Format("2006-01-02")
	mk(tx, "supplier_bills", branch, map[string]any{"no": r.S("no"), "supplier": r.Data["supplier"], "purchase": float64(r.ID), "date": date, "due": due,
		"amount": float64(total), "paid": float64(0), "posted": true, "status": "open", "note": "From purchase " + r.S("no")})
	bumpLater("supplier_bills")
	return nil
}

func sendTransfer(a *App, tx *sql.Tx, c *Claims, r *Rec, p map[string]any) error {
	ref := fmt.Sprintf("transfer:%d", r.ID)
	for _, l := range r.L("lines") {
		ing := toInt(l["ingredient"])
		if err := a.stockMove(tx, r.I("from_branch"), ing, -toFloat(l["qty"]), a.ingCost(tx, ing), "transfer_out", ref, r.S("no"), c.UID); err != nil {
			return err
		}
	}
	r.Status = "in_transit"
	return nil
}

func receiveTransfer(a *App, tx *sql.Tx, c *Claims, r *Rec, p map[string]any) error {
	ref := fmt.Sprintf("transfer:%d", r.ID)
	for _, l := range r.L("lines") {
		ing := toInt(l["ingredient"])
		if err := a.stockMove(tx, r.I("to_branch"), ing, toFloat(l["qty"]), a.ingCost(tx, ing), "transfer_in", ref, r.S("no"), c.UID); err != nil {
			return err
		}
	}
	r.Status = "received"
	return nil
}

func applyCount(a *App, tx *sql.Tx, c *Claims, r *Rec, p map[string]any) error {
	branch := r.Branch
	if branch == 0 {
		branch = c.Branch
	}
	if branch == 0 {
		return uerr("choose a branch on this count first")
	}
	ref := fmt.Sprintf("count:%d", r.ID)
	var gain, loss int64
	lines := r.L("lines")
	for _, l := range lines {
		ing := toInt(l["ingredient"])
		var sys float64
		tx.QueryRow(`SELECT qty FROM ingredients_stock WHERE branch_id=? AND ingredient_id=?`, branch, ing).Scan(&sys)
		diff := toFloat(l["counted"]) - sys
		l["system"], l["variance"] = sys, diff
		if diff == 0 {
			continue
		}
		cost := a.ingCost(tx, ing)
		if err := a.stockMove(tx, branch, ing, diff, cost, "count", ref, r.S("name"), c.UID); err != nil {
			return err
		}
		if diff > 0 {
			gain += int64(diff * float64(cost))
		} else {
			loss += int64(-diff * float64(cost))
		}
	}
	arr := make([]any, len(lines))
	for i, l := range lines {
		arr[i] = l
	}
	r.Set("lines", arr)
	r.Set("variance_value", float64(gain-loss))
	r.Status = "applied"
	if a.autoPost() {
		_, err := a.post(tx, branch, today(), ref, "Inventory count "+r.S("name"), c.UID,
			Dr("inventory", gain), Cr("inv_adjust", gain), Dr("inv_adjust", loss), Cr("inventory", loss))
		return err
	}
	return nil
}

func postBreakage(a *App, tx *sql.Tx, c *Claims, r *Rec, p map[string]any) error {
	var total int64
	var n int64
	for _, g := range recs(tx, "gift_cards", 0) {
		if g.Status == "active" && g.S("expires") != "" && g.S("expires") < today() && g.I("balance") > 0 {
			total += g.I("balance")
			n++
			mk(tx, "gift_txns", g.Branch, map[string]any{"card": float64(g.ID), "kind": "expire", "amount": float64(g.I("balance")), "note": "Expired - breakage"})
			g.Set("balance", float64(0))
			g.Status = "expired"
			saveRec(tx, g)
		}
	}
	r.Set("expired_cards", float64(n))
	r.Set("amount", float64(total))
	r.Status = "posted"
	return a.postSimple(tx, r.Branch, r.S("date"), fmt.Sprintf("breakage:%d", r.ID), "Gift card breakage "+r.S("name"), "gift_liability", "breakage", total, c.UID)
}

// ---------- read endpoints ----------

type StockRow struct {
	ID      int64   `json:"id"`
	Name    string  `json:"name"`
	SKU     string  `json:"sku"`
	Cat     string  `json:"category"`
	Unit    string  `json:"unit"`
	Reorder float64 `json:"reorder"`
	Cost    int64   `json:"cost"`
	Qty     float64 `json:"qty"`
	Value   int64   `json:"value"`
	Low     bool    `json:"low"`
}

func (a *App) stockRows(branch int64) []StockRow {
	qty := map[int64]float64{}
	q := `SELECT ingredient_id,SUM(qty) FROM ingredients_stock`
	args := []any{}
	if branch != 0 {
		q += ` WHERE branch_id=?`
		args = append(args, branch)
	}
	if rows, err := a.db.rdb.Query(q+` GROUP BY ingredient_id`, args...); err == nil {
		for rows.Next() {
			var id int64
			var s float64
			rows.Scan(&id, &s)
			qty[id] = s
		}
		rows.Close()
	}
	units := a.labelMap("units")
	var out []StockRow
	for _, rec := range recs(a.db.rdb, "ingredients", 0) {
		if rec.Status == "inactive" {
			continue
		}
		s := StockRow{ID: rec.ID, Name: rec.S("name"), SKU: rec.Ref, Cat: rec.S("category"), Unit: units[rec.I("unit")],
			Reorder: rec.N("reorder_level"), Cost: rec.I("cost"), Qty: qty[rec.ID]}
		s.Value = int64(s.Qty * float64(s.Cost))
		s.Low = s.Reorder > 0 && s.Qty <= s.Reorder
		out = append(out, s)
	}
	return out
}

func (a *App) handleStock(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	b := scopeBranch(c, r)
	data, err := a.lookups.get("stock|"+strconv.FormatInt(b, 10)+"|"+strconv.FormatInt(verOf("ingredients"), 10), 3*time.Second, func() ([]byte, error) {
		return jsonBytes(a.stockRows(b)), nil
	})
	if err != nil {
		writeErr(w, 503, "please retry")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func (a *App) handleStockMoves(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	where, args := "1=1", []any{}
	if b := scopeBranch(c, r); b != 0 {
		where += " AND m.branch_id=?"
		args = append(args, b)
	}
	if n := qInt(r, "ingredient"); n > 0 {
		where += " AND m.ingredient_id=?"
		args = append(args, n)
	}
	if k := qStr(r, "kind"); k != "" {
		where += " AND m.kind=?"
		args = append(args, k)
	}
	limit := int(qInt(r, "limit"))
	if limit < 1 || limit > 500 {
		limit = 50
	}
	page := int(max(qInt(r, "page"), 1))
	var total int64
	a.db.rdb.QueryRow(`SELECT COUNT(*) FROM stock_moves m WHERE `+where, args...).Scan(&total)
	rows := a.queryMaps(`SELECT m.id,m.branch_id,m.ingredient_id,m.qty,m.unit_cost,m.kind,m.ref,m.note,m.created_at FROM stock_moves m WHERE `+where+
		` ORDER BY m.id DESC LIMIT ? OFFSET ?`, append(args, limit, (page-1)*limit)...)
	names := map[int64]ingInfo{}
	for _, m := range rows {
		id := toInt(m["ingredient_id"])
		if _, ok := names[id]; !ok {
			names[id] = a.ing(a.db.rdb, id)
		}
		m["ingredient"] = names[id].Name
	}
	writeJSON(w, 200, map[string]any{"rows": rows, "total": total, "page": page, "limit": limit})
}

func (a *App) handleInventoryOverview(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	b := scopeBranch(c, r)
	items := a.stockRows(b)
	var value int64
	low := []StockRow{}
	for _, s := range items {
		value += s.Value
		if s.Low {
			low = append(low, s)
		}
	}
	sort.Slice(low, func(i, j int) bool { return low[i].Qty < low[j].Qty })
	if len(low) > 10 {
		low = low[:10]
	}
	since := time.Now().AddDate(0, 0, -30).Unix()
	q := `SELECT ingredient_id,-SUM(qty) FROM stock_moves WHERE kind='sale' AND created_at>=?`
	args := []any{since}
	if b != 0 {
		q += ` AND branch_id=?`
		args = append(args, b)
	}
	used := []map[string]any{}
	if rows, err := a.db.rdb.Query(q+` GROUP BY ingredient_id ORDER BY 2 DESC LIMIT 5`, args...); err == nil {
		for rows.Next() {
			var id int64
			var qty float64
			rows.Scan(&id, &qty)
			used = append(used, map[string]any{"name": a.ing(a.db.rdb, id).Name, "qty": qty})
		}
		rows.Close()
	}
	mq := `SELECT m.id,m.ingredient_id,m.qty,m.kind,m.created_at FROM stock_moves m`
	margs := []any{}
	if b != 0 {
		mq += ` WHERE m.branch_id=?`
		margs = append(margs, b)
	}
	recent := a.queryMaps(mq+` ORDER BY m.id DESC LIMIT 8`, margs...)
	for _, m := range recent {
		m["ingredient"] = a.ing(a.db.rdb, toInt(m["ingredient_id"])).Name
	}
	writeJSON(w, 200, map[string]any{"items": len(items), "value": value, "low_count": len(low), "low": low, "top_used": used, "recent": recent})
}

// POST /api/customers/{id}/points {delta, reason}
func (a *App) handlePointsAdjust(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var in struct {
		Delta  int64  `json:"delta"`
		Reason string `json:"reason"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	err := a.tx(func(tx *sql.Tx) error {
		cu, err := loadRec(tx, id)
		if err != nil || cu.Res != "customers" {
			return uerr("customer not found")
		}
		n := cu.I("points") + in.Delta
		if n < 0 {
			return uerr("a customer cannot have fewer than 0 points")
		}
		cu.Set("points", float64(n))
		return saveRec(tx, cu)
	})
	if err != nil {
		writeErr(w, 400, friendlyErr(err))
		return
	}
	bumpRes("customers")
	a.audit(r, "customers", id, "points", strconv.FormatInt(in.Delta, 10)+" "+in.Reason)
	writeJSON(w, 200, map[string]bool{"ok": true})
}
