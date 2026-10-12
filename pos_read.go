package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

func jsonBytes(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("null")
	}
	return b
}

func (a *App) serveCached(w http.ResponseWriter, cache *flightCache, key string, ttl time.Duration, load func() ([]byte, error)) {
	b, err := cache.get(key, ttl, load)
	if err != nil {
		writeErr(w, 503, "please retry")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

// ---------- orders list ----------

// GET /api/pos/orders?status=active|all|a,b&channel=&pay=&q=&from=&to=&table=&items=1&page=&limit=
func (a *App) handleOrders(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	b := scopeBranch(c, r)
	own := a.ownOrdersOnly(c)
	a.serveCached(w, a.ordersCache, fmt.Sprintf("%d|%s|%d|%v|%d", b, r.URL.RawQuery, verOf("orders"), own, c.UID), time.Second, func() ([]byte, error) {
		q := r.URL.Query()
		where := []string{"1=1"}
		args := []any{}
		if own {
			where = append(where, "o.created_by=?")
			args = append(args, c.UID)
		}
		if b != 0 {
			where = append(where, "o.branch_id=?")
			args = append(args, b)
		}
		switch s := q.Get("status"); s {
		case "", "all":
		case "active":
			where = append(where, "o.status IN ('pending','preparing','ready','served')")
		default:
			parts := strings.Split(s, ",")
			ph := make([]string, len(parts))
			for i, p := range parts {
				ph[i] = "?"
				args = append(args, p)
			}
			where = append(where, "o.status IN ("+strings.Join(ph, ",")+")")
		}
		if v := q.Get("channel"); v != "" {
			where = append(where, "o.channel=?")
			args = append(args, v)
		}
		if v := q.Get("pay"); v != "" {
			where = append(where, "o.pay_status=?")
			args = append(args, v)
		}
		if v, _ := strconv.ParseInt(q.Get("table"), 10, 64); v > 0 {
			where = append(where, "o.table_id=?")
			args = append(args, v)
		}
		if v, _ := strconv.ParseInt(q.Get("from"), 10, 64); v > 0 {
			where = append(where, "o.created_at>=?")
			args = append(args, v)
		}
		if v, _ := strconv.ParseInt(q.Get("to"), 10, 64); v > 0 {
			where = append(where, "o.created_at<?")
			args = append(args, v)
		}
		if s := strings.TrimSpace(q.Get("q")); s != "" {
			if id, err := strconv.ParseInt(strings.TrimPrefix(s, "#"), 10, 64); err == nil {
				where = append(where, "(o.id=? OR o.no LIKE ? OR o.phone LIKE ?)")
				args = append(args, id, "%"+s+"%", "%"+s+"%")
			} else {
				where = append(where, "(o.no LIKE ? OR o.customer LIKE ? OR o.phone LIKE ?)")
				args = append(args, "%"+s+"%", "%"+s+"%", "%"+s+"%")
			}
		}
		limit, _ := strconv.Atoi(q.Get("limit"))
		if limit < 1 || limit > 300 {
			limit = 30
		}
		page, _ := strconv.Atoi(q.Get("page"))
		if page < 1 {
			page = 1
		}
		var total int64
		a.db.rdb.QueryRow(`SELECT COUNT(*) FROM orders o WHERE `+strings.Join(where, " AND "), args...).Scan(&total)
		rows, err := a.db.rdb.Query(`SELECT `+orderCols+orderFrom+` WHERE `+strings.Join(where, " AND ")+` ORDER BY o.id DESC LIMIT ? OFFSET ?`,
			append(args, limit, (page-1)*limit)...)
		if err != nil {
			return nil, err
		}
		out := []*Order{}
		for rows.Next() {
			if o, err := scanOrder(rows); err == nil {
				out = append(out, o)
			}
		}
		rows.Close()
		if q.Get("items") == "1" || q.Get("status") == "active" {
			loadItemsFor(a.db.rdb, out)
		}
		return jsonBytes(map[string]any{"rows": out, "total": total, "page": page, "limit": limit}), nil
	})
}

// ---------- kitchen display ----------

func (a *App) handleKDS(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	b := scopeBranch(c, r)
	station := qStr(r, "station")
	a.serveCached(w, a.ordersCache, fmt.Sprintf("kds|%d|%s", b, station), time.Second, func() ([]byte, error) {
		where := "o.status IN ('pending','preparing','ready')"
		args := []any{}
		if b != 0 {
			where += " AND o.branch_id=?"
			args = append(args, b)
		}
		rows, err := a.db.rdb.Query(`SELECT `+orderCols+orderFrom+` WHERE `+where+` ORDER BY o.id LIMIT 200`, args...)
		if err != nil {
			return nil, err
		}
		list := []*Order{}
		for rows.Next() {
			if o, err := scanOrder(rows); err == nil {
				list = append(list, o)
			}
		}
		rows.Close()
		loadItemsFor(a.db.rdb, list)
		out := []*Order{}
		for _, o := range list {
			if station != "" {
				var keep []Item
				for _, it := range o.Items {
					if it.Station == station {
						keep = append(keep, it)
					}
				}
				if len(keep) == 0 {
					continue
				}
				o.Items = keep
			}
			// hide fully served items
			out = append(out, o)
		}
		return jsonBytes(out), nil
	})
}

// ---------- table board (POS floor view) ----------

func (a *App) handleTableBoard(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	b := scopeBranch(c, r)
	a.serveCached(w, a.ordersCache, fmt.Sprintf("tboard|%d|%d|%d", b, verOf("tables"), verOf("reservations")), time.Second, func() ([]byte, error) {
		zones, floors := a.labelMap("zones"), a.labelMap("floors")
		type open struct {
			ID     int64  `json:"id"`
			No     string `json:"no"`
			Total  int64  `json:"total"`
			Status string `json:"status"`
			Paid   int64  `json:"paid"`
			At     int64  `json:"created_at"`
		}
		opens := map[int64]open{}
		q := `SELECT table_id,id,no,total,status,paid,created_at FROM orders WHERE table_id>0 AND status IN ('pending','preparing','ready','served')`
		args := []any{}
		if b != 0 {
			q += ` AND branch_id=?`
			args = append(args, b)
		}
		if rows, err := a.db.rdb.Query(q, args...); err == nil {
			for rows.Next() {
				var tid int64
				var o open
				rows.Scan(&tid, &o.ID, &o.No, &o.Total, &o.Status, &o.Paid, &o.At)
				opens[tid] = o
			}
			rows.Close()
		}
		next := map[int64]string{}
		for _, rv := range recs(a.db.rdb, "reservations", b) {
			if rv.S("date") == today() && (rv.Status == "pending" || rv.Status == "confirmed") && rv.I("table") > 0 {
				if cur, ok := next[rv.I("table")]; !ok || rv.S("time") < cur {
					next[rv.I("table")] = rv.S("time") + " " + rv.S("name")
				}
			}
		}
		out := []map[string]any{}
		for _, t := range recs(a.db.rdb, "tables", b) {
			m := map[string]any{"id": t.ID, "name": t.S("name"), "seats": t.I("seats"), "status": t.Status, "zone": zones[t.I("zone")], "floor": floors[t.I("floor")],
				"shape": t.S("shape"), "waiter": t.I("waiter"), "branch_id": t.Branch}
			if o, ok := opens[t.ID]; ok {
				m["order"] = o
			}
			if n, ok := next[t.ID]; ok {
				m["reserved_next"] = n
			}
			out = append(out, m)
		}
		return jsonBytes(out), nil
	})
}

func (a *App) handleTableStatus(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var in struct {
		Status string `json:"status"`
		Waiter *int64 `json:"waiter"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	err := a.tx(func(tx *sql.Tx) error {
		t, err := loadRec(tx, id)
		if err != nil || t.Res != "tables" || (c.Branch != 0 && t.Branch != c.Branch) {
			return uerr("table not found")
		}
		if in.Status != "" {
			ok := false
			for _, s := range registry["tables"].byKey["status"].Opts {
				ok = ok || s == in.Status
			}
			if !ok {
				return uerr("invalid table status")
			}
			if in.Status == "available" {
				var n int
				tx.QueryRow(`SELECT COUNT(*) FROM orders WHERE table_id=? AND status IN ('pending','preparing','ready','served')`, id).Scan(&n)
				if n > 0 {
					return uerr("this table still has an open order")
				}
			}
			t.Status = in.Status
		}
		if in.Waiter != nil {
			t.Set("waiter", float64(*in.Waiter))
		}
		return saveRec(tx, t)
	})
	if err != nil {
		writeErr(w, 400, friendlyErr(err))
		return
	}
	bumpRes("tables")
	a.hub.PublishBranch(c.Branch, "tables")
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---------- sessions & cash ----------

func init() {
	def("pos_sessions", "pos", "POS sessions",
		`register:ref(registers)*^; user:ref(users)*^; opening_cash:money*^; closing_cash:money^; expected:money.^; difference:money.^; opened_at:datetime^; closed_at:datetime; note; status:select(open|closed)^=open`,
		Scoped, NoCreate, NoDelete, Label("Session #{id}"), Hint("A cashier's shift on a register. Close it to count the cash drawer."))
	def("cash_moves", "pos", "Cash movements",
		`session:ref(pos_sessions); kind:select(in|out)*^; amount:money*^; reason:text*^; date:date; note`, Scoped, Label("Cash {kind} #{id}"),
		Hint("Pay-ins and pay-outs from the drawer (float, supplier cash payments, tips paid out ...)."),
		BeforeSave(func(a *App, tx *sql.Tx, c *Claims, r *Rec, old *Rec) error {
			if old == nil {
				if r.I("session") == 0 {
					r.Set("session", float64(a.openSession(tx, c.UID, firstNonZero(r.Branch, c.Branch))))
				}
				if r.S("date") == "" {
					r.Set("date", today())
				}
			}
			return nil
		}),
		AfterSave(func(a *App, tx *sql.Tx, c *Claims, r *Rec, old *Rec) error {
			if old != nil {
				return nil
			}
			amt := r.I("amount")
			if r.S("kind") == "in" {
				return a.postSimple(tx, r.Branch, r.S("date"), fmt.Sprintf("cashmove:%d", r.ID), "Cash in: "+r.S("reason"), "cash", "other_income", amt, c.UID)
			}
			return a.postSimple(tx, r.Branch, r.S("date"), fmt.Sprintf("cashmove:%d", r.ID), "Cash out: "+r.S("reason"), "general_expense", "cash", amt, c.UID)
		}))
}

func (a *App) handleSessionGet(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	var id int64
	a.db.rdb.QueryRow(`SELECT id FROM records WHERE res='pos_sessions' AND status='open' AND json_extract(data,'$.user')=? ORDER BY id DESC LIMIT 1`, c.UID).Scan(&id)
	if id == 0 {
		writeJSON(w, 200, nil)
		return
	}
	rec, _ := loadRec(a.db.rdb, id)
	m := rec.JSON()
	m["cash_sales"], m["cash_refunds"], m["moves_in"], m["moves_out"] = a.sessionFigures(a.db.rdb, id)
	writeJSON(w, 200, m)
}

func (a *App) sessionFigures(q Querier, id int64) (sales, refunds, in, out int64) {
	q.QueryRow(`SELECT COALESCE(SUM(CASE WHEN kind='payment' THEN amount END),0),COALESCE(SUM(CASE WHEN kind='refund' THEN amount END),0) FROM payments WHERE session_id=? AND method='cash'`, id).Scan(&sales, &refunds)
	q.QueryRow(`SELECT COALESCE(SUM(CASE WHEN json_extract(data,'$.kind')='in' THEN json_extract(data,'$.amount') END),0),
		COALESCE(SUM(CASE WHEN json_extract(data,'$.kind')='out' THEN json_extract(data,'$.amount') END),0) FROM records WHERE res='cash_moves' AND json_extract(data,'$.session')=?`, id).Scan(&in, &out)
	return
}

func (a *App) handleSessionOpen(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	var in struct {
		Register int64 `json:"register"`
		Cash     int64 `json:"opening_cash"`
		Branch   int64 `json:"branch_id"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	var out *Rec
	err := a.tx(func(tx *sql.Tx) error {
		branch := firstNonZero(c.Branch, in.Branch, a.defaultBranch(tx))
		if a.openSession(tx, c.UID, branch) > 0 {
			return uerr("you already have an open session")
		}
		id := mk(tx, "pos_sessions", branch, map[string]any{"register": float64(in.Register), "user": float64(c.UID), "opening_cash": float64(in.Cash),
			"opened_at": time.Now().Format("2006-01-02T15:04"), "status": "open"})
		out, _ = loadRec(tx, id)
		return nil
	})
	if err != nil {
		writeErr(w, 400, friendlyErr(err))
		return
	}
	bumpRes("pos_sessions")
	a.audit(r, "pos_sessions", out.ID, "open", "")
	writeJSON(w, 200, out.JSON())
}

func (a *App) handleSessionClose(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	var in struct {
		Cash int64  `json:"closing_cash"`
		Note string `json:"note"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	var out *Rec
	err := a.tx(func(tx *sql.Tx) error {
		var id int64
		if tx.QueryRow(`SELECT id FROM records WHERE res='pos_sessions' AND status='open' AND json_extract(data,'$.user')=? ORDER BY id DESC LIMIT 1`, c.UID).Scan(&id) != nil {
			return uerr("you have no open session")
		}
		s, _ := loadRec(tx, id)
		sales, refunds, mi, mo := a.sessionFigures(tx, id)
		expected := s.I("opening_cash") + sales - refunds + mi - mo
		diff := in.Cash - expected
		s.Set("closing_cash", float64(in.Cash))
		s.Set("expected", float64(expected))
		s.Set("difference", float64(diff))
		s.Set("closed_at", time.Now().Format("2006-01-02T15:04"))
		s.Set("note", trunc(in.Note, 300))
		s.Status = "closed"
		if diff != 0 {
			if diff < 0 {
				a.postSimple(tx, s.Branch, today(), fmt.Sprintf("session:%d", id), "Cash short", "cash_over_short", "cash", -diff, c.UID)
			} else {
				a.postSimple(tx, s.Branch, today(), fmt.Sprintf("session:%d", id), "Cash over", "cash", "cash_over_short", diff, c.UID)
			}
		}
		out = s
		return saveRec(tx, s)
	})
	if err != nil {
		writeErr(w, 400, friendlyErr(err))
		return
	}
	bumpRes("pos_sessions")
	a.audit(r, "pos_sessions", out.ID, "close", "")
	writeJSON(w, 200, out.JSON())
}

// ---------- payments list ----------

func (a *App) handlePayments(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	where, args := "1=1", []any{}
	if b := scopeBranch(c, r); b != 0 {
		where += " AND p.branch_id=?"
		args = append(args, b)
	}
	if v := qStr(r, "method"); v != "" {
		where += " AND p.method=?"
		args = append(args, v)
	}
	if v := qStr(r, "kind"); v != "" {
		where += " AND p.kind=?"
		args = append(args, v)
	}
	if v := qInt(r, "from"); v > 0 {
		where += " AND p.created_at>=?"
		args = append(args, v)
	}
	if v := qInt(r, "to"); v > 0 {
		where += " AND p.created_at<?"
		args = append(args, v)
	}
	limit := int(qInt(r, "limit"))
	if limit < 1 || limit > 500 {
		limit = 30
	}
	page := int(max(qInt(r, "page"), 1))
	var total int64
	a.db.rdb.QueryRow(`SELECT COUNT(*) FROM payments p WHERE `+where, args...).Scan(&total)
	rows := a.queryMaps(`SELECT p.id,p.order_id,o.no,p.method,p.kind,p.amount,p.ref,p.created_at,COALESCE(u.name,'') staff FROM payments p
		LEFT JOIN orders o ON o.id=p.order_id LEFT JOIN users u ON u.id=p.created_by WHERE `+where+` ORDER BY p.id DESC LIMIT ? OFFSET ?`, append(args, limit, (page-1)*limit)...)
	sums := a.queryMaps(`SELECT p.method,SUM(CASE WHEN p.kind='payment' THEN p.amount ELSE -p.amount END) amount,COUNT(*) n FROM payments p WHERE `+where+` GROUP BY p.method`, args...)
	writeJSON(w, 200, map[string]any{"rows": rows, "total": total, "page": page, "limit": limit, "by_method": sums})
}

// ---------- customer display (second screen) ----------

var (
	displayMu sync.RWMutex
	displays  = map[string]displayState{}
)

type displayState struct {
	At   int64           `json:"at"`
	Cart json.RawMessage `json:"cart"`
}

func (a *App) displayKey(register int64) string {
	m := hmac.New(sha256.New, a.secret)
	m.Write([]byte(fmt.Sprintf("display:%d", register)))
	return hex.EncodeToString(m.Sum(nil))[:16]
}

func (a *App) handleDisplayKey(w http.ResponseWriter, r *http.Request) {
	reg := qInt(r, "register")
	writeJSON(w, 200, map[string]any{"url": fmt.Sprintf("/display?r=%d&k=%s", reg, a.displayKey(reg))})
}

func (a *App) handleDisplayPush(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Register int64           `json:"register"`
		Cart     json.RawMessage `json:"cart"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	displayMu.Lock()
	if len(displays) > 500 {
		displays = map[string]displayState{}
	}
	displays[strconv.FormatInt(in.Register, 10)] = displayState{nowUnix(), in.Cart}
	displayMu.Unlock()
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *App) handleDisplayGet(w http.ResponseWriter, r *http.Request) {
	reg := qInt(r, "r")
	if !hmac.Equal([]byte(qStr(r, "k")), []byte(a.displayKey(reg))) {
		writeErr(w, 403, "invalid display link")
		return
	}
	displayMu.RLock()
	st := displays[strconv.FormatInt(reg, 10)]
	displayMu.RUnlock()
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{"state": st, "name": a.settingStr("application", "app_name", a.cfg.Restaurant), "logo": a.settingGroup("appearance")["logo"]})
}

// ---------- brief user list & per-user prefs ----------

func (a *App) handleUsersBrief(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	a.serveCached(w, a.lookups, fmt.Sprintf("ubrief|%d", c.Branch), 10*time.Second, func() ([]byte, error) {
		where, args := "active=1", []any{}
		if c.Branch != 0 {
			where += " AND (branch_id=? OR branch_id=0)"
			args = append(args, c.Branch)
		}
		return jsonBytes(a.queryMaps(`SELECT id,name,role,branch_id FROM users WHERE `+where+` ORDER BY name`, args...)), nil
	})
}

func (a *App) handlePrefGet(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	var v string
	if a.db.rdb.QueryRow(`SELECT value FROM settings WHERE key=?`, fmt.Sprintf("pref:%d:%s", c.UID, r.PathValue("key"))).Scan(&v) != nil {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("null"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(v))
}

func (a *App) handlePrefPut(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	var raw json.RawMessage
	if !readJSON(w, r, &raw) {
		return
	}
	if len(raw) > 20000 {
		writeErr(w, 400, "too large")
		return
	}
	a.db.wdb.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, fmt.Sprintf("pref:%d:%s", c.UID, r.PathValue("key")), string(raw))
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---------- hub dashboard ----------

func (a *App) tzOffsetMin() int {
	loc, err := time.LoadLocation(a.settingStr("application", "timezone", "Asia/Kolkata"))
	if err != nil {
		return 0
	}
	_, off := time.Now().In(loc).Zone()
	return off / 60
}

// GET /api/dash/hub?from=&to=&branch=&tz=
func (a *App) handleHubDash(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	b := scopeBranch(c, r)
	to, from := qInt(r, "to"), qInt(r, "from")
	tz := int(qInt(r, "tz"))
	if to == 0 {
		to = time.Now().Unix() + 1
	}
	if from == 0 || from >= to {
		from = to - 7*86400
	}
	key := fmt.Sprintf("hub|%d|%d|%d|%d", b, from/60, to/60, tz)
	a.serveCached(w, a.dashCache, key, 5*time.Second, func() ([]byte, error) {
		return jsonBytes(a.computeHubDash(b, from, to, tz)), nil
	})
}

func (a *App) computeHubDash(b, from, to int64, tz int) map[string]any {
	db := a.db.rdb
	bw, bargs := "", []any{}
	if b != 0 {
		bw = " AND branch_id=?"
		bargs = []any{b}
	}
	period := func(f, t int64) (sales, orders, paidN int64) {
		args := append([]any{f, t}, bargs...)
		db.QueryRow(`SELECT COALESCE(SUM(CASE WHEN pay_status='paid' THEN total END),0),COUNT(CASE WHEN status NOT IN ('cancelled','held') THEN 1 END),
			COUNT(CASE WHEN pay_status='paid' THEN 1 END) FROM orders WHERE created_at>=? AND created_at<?`+bw, args...).Scan(&sales, &orders, &paidN)
		return
	}
	sales, orders, paidN := period(from, to)
	span := to - from
	psales, porders, ppaid := period(from-span, from)
	avg, pavg := int64(0), int64(0)
	if paidN > 0 {
		avg = sales / paidN
	}
	if ppaid > 0 {
		pavg = psales / ppaid
	}
	out := map[string]any{"sales": sales, "orders": orders, "avg": avg, "prev_sales": psales, "prev_orders": porders, "prev_avg": pavg}
	var active int64
	db.QueryRow(`SELECT COUNT(*) FROM orders WHERE status IN ('pending','preparing','ready','served')`+bw, bargs...).Scan(&active)
	out["active"] = active

	args := append([]any{from, to}, bargs...)
	byStatus := map[string]int64{}
	for _, m := range a.queryMaps(`SELECT status,COUNT(*) n FROM orders WHERE created_at>=? AND created_at<?`+bw+` GROUP BY status`, args...) {
		byStatus[toStr(m["status"])] = toInt(m["n"])
	}
	out["by_status"] = byStatus

	format, step := "%Y-%m-%d", "day"
	if to-from <= 2*86400 {
		format, step = "%H:00", "hour"
	}
	series := func(f, t int64) []map[string]any {
		return a.queryMaps(`SELECT strftime('`+format+`',created_at+?,'unixepoch') label,COALESCE(SUM(CASE WHEN pay_status='paid' THEN total END),0) sales,COUNT(*) orders
			FROM orders WHERE status NOT IN ('cancelled','held') AND created_at>=? AND created_at<?`+bw+` GROUP BY label ORDER BY label`, append([]any{tz * 60, f, t}, bargs...)...)
	}
	out["series"], out["prev_series"], out["step"] = series(from, to), series(from-span, from), step

	out["top"] = a.queryMaps(`SELECT i.name,SUM(i.qty) qty,SUM(i.qty*i.price) revenue FROM order_items i JOIN orders o ON o.id=i.order_id
		WHERE o.status NOT IN ('cancelled','held') AND i.status<>'void' AND o.created_at>=? AND o.created_at<?`+strings.ReplaceAll(bw, "branch_id", "o.branch_id")+` GROUP BY i.product_id ORDER BY qty DESC LIMIT 8`, args...)
	out["top_categories"] = a.queryMaps(`SELECT i.category name,SUM(i.qty) qty,SUM(i.qty*i.price) revenue FROM order_items i JOIN orders o ON o.id=i.order_id
		WHERE o.status NOT IN ('cancelled','held') AND i.status<>'void' AND o.created_at>=? AND o.created_at<?`+strings.ReplaceAll(bw, "branch_id", "o.branch_id")+` GROUP BY i.category ORDER BY revenue DESC LIMIT 6`, args...)
	out["by_channel"] = a.queryMaps(`SELECT channel name,COUNT(*) orders,COALESCE(SUM(total),0) sales FROM orders WHERE status NOT IN ('cancelled','held') AND created_at>=? AND created_at<?`+bw+` GROUP BY channel`, args...)
	out["by_payment"] = a.queryMaps(`SELECT p.method name,SUM(CASE WHEN p.kind='payment' THEN p.amount ELSE -p.amount END) sales FROM payments p WHERE p.created_at>=? AND p.created_at<?`+strings.ReplaceAll(bw, "branch_id", "p.branch_id")+` GROUP BY p.method`, args...)
	out["hourly"] = a.queryMaps(`SELECT CAST(strftime('%H',created_at+?,'unixepoch') AS INTEGER) hour,COUNT(*) orders FROM orders WHERE status NOT IN ('cancelled','held') AND created_at>=? AND created_at<?`+bw+` GROUP BY hour ORDER BY hour`,
		append([]any{tz * 60, from, to}, bargs...)...)
	out["staff"] = a.queryMaps(`SELECT COALESCE(u.name,'?') name,COUNT(*) orders,COALESCE(SUM(CASE WHEN o.pay_status='paid' THEN o.total END),0) sales FROM orders o LEFT JOIN users u ON u.id=o.created_by
		WHERE o.status NOT IN ('cancelled','held') AND o.created_at>=? AND o.created_at<?`+strings.ReplaceAll(bw, "branch_id", "o.branch_id")+` GROUP BY o.created_by ORDER BY sales DESC LIMIT 6`, args...)
	var disc, tips, tax, svc int64
	var cancelled, refunded, refundedAmt int64
	db.QueryRow(`SELECT COALESCE(SUM(discount),0),COALESCE(SUM(tip),0),COALESCE(SUM(tax),0),COALESCE(SUM(service),0),
		COUNT(CASE WHEN status='cancelled' THEN 1 END),COUNT(CASE WHEN status='refunded' THEN 1 END),COALESCE(SUM(CASE WHEN status='refunded' THEN total END),0)
		FROM orders WHERE status<>'held' AND created_at>=? AND created_at<?`+bw, args...).Scan(&disc, &tips, &tax, &svc, &cancelled, &refunded, &refundedAmt)
	out["discounts"], out["tips"], out["tax"], out["service"], out["cancelled"], out["refunded"], out["refunded_amount"] = disc, tips, tax, svc, cancelled, refunded, refundedAmt
	var avgMin float64
	db.QueryRow(`SELECT COALESCE(AVG(updated_at-created_at),0)/60.0 FROM orders WHERE status='completed' AND created_at>=? AND created_at<?`+bw, args...).Scan(&avgMin)
	out["avg_service_min"] = avgMin
	// tables
	var tot, occ int64
	for _, t := range recs(db, "tables", b) {
		tot++
		if t.Status == "occupied" {
			occ++
		}
	}
	out["tables_total"], out["tables_occupied"] = tot, occ
	// customers
	var newC, allC int64
	db.QueryRow(`SELECT COUNT(CASE WHEN created_at>=? AND created_at<? THEN 1 END),COUNT(*) FROM records WHERE res='customers'`, from, to).Scan(&newC, &allC)
	out["new_customers"], out["customers"] = newC, allC
	// low stock
	var low []StockRow
	for _, s := range a.stockRows(b) {
		if s.Low {
			low = append(low, s)
		}
	}
	sort.Slice(low, func(i, j int) bool { return low[i].Qty-low[i].Reorder < low[j].Qty-low[j].Reorder })
	if len(low) > 8 {
		low = low[:8]
	}
	out["low_stock"] = low
	return out
}

// ---------- customer analytics ----------

func (a *App) handleCustomerAnalytics(w http.ResponseWriter, r *http.Request) {
	list := recs(a.db.rdb, "customers", 0)
	sort.Slice(list, func(i, j int) bool { return list[i].I("spent") > list[j].I("spent") })
	top := []map[string]any{}
	var points, spent int64
	tiers := map[string]int{}
	var repeat, one int
	month := time.Now().Format("-01-")
	bdays := []map[string]any{}
	for i, cu := range list {
		points += cu.I("points")
		spent += cu.I("spent")
		t := cu.S("tier")
		if t == "" {
			t = "No tier"
		}
		tiers[t]++
		if cu.I("visits") > 1 {
			repeat++
		} else if cu.I("visits") == 1 {
			one++
		}
		if i < 10 {
			top = append(top, map[string]any{"id": cu.ID, "name": cu.S("name"), "phone": cu.S("phone"), "spent": cu.I("spent"), "visits": cu.I("visits"), "points": cu.I("points"), "tier": cu.S("tier")})
		}
		if bd := cu.S("birthday"); len(bd) == 10 && strings.Contains(bd[4:], month) {
			bdays = append(bdays, map[string]any{"name": cu.S("name"), "date": bd, "phone": cu.S("phone")})
		}
	}
	writeJSON(w, 200, map[string]any{"count": len(list), "points_outstanding": points, "lifetime_spend": spent, "repeat": repeat, "one_time": one,
		"tiers": tiers, "top": top, "birthdays": bdays})
}
