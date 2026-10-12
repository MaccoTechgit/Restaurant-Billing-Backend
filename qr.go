package main

import (
	"database/sql"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/skip2/go-qrcode"
)

// =====================================================================
//  Table QR ordering: guests scan the code on their table (no login).
//  Everything here is public, so it is rate-limited and validated hard.
// =====================================================================

func (a *App) tableByToken(q Querier, token string) (*Rec, error) {
	if len(token) < 6 || len(token) > 40 {
		return nil, uerr("this QR code is not valid")
	}
	var id int64
	if q.QueryRow(`SELECT id FROM records WHERE res='tables' AND json_extract(data,'$.qr_token')=?`, token).Scan(&id) != nil {
		return nil, uerr("this QR code is not valid")
	}
	return loadRec(q, id)
}

func (a *App) handleQRMenu(w http.ResponseWriter, r *http.Request) {
	if !a.settingBool("qr", "enabled", true) {
		writeErr(w, 403, "QR ordering is turned off")
		return
	}
	t, err := a.tableByToken(a.db.rdb, qStr(r, "t"))
	if err != nil {
		writeErr(w, 404, friendlyErr(err))
		return
	}
	m := a.menu()
	branch, _ := loadRec(a.db.rdb, t.Branch)
	bname := ""
	if branch != nil {
		bname = branch.S("name")
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(jsonBytes(map[string]any{
		"restaurant": a.settingStr("application", "app_name", a.cfg.Restaurant), "branch": bname, "table": t.S("name"),
		"welcome": a.settingStr("qr", "welcome_text", ""), "tagline": a.settingStr("application", "tagline", ""), "phone": a.settingStr("application", "phone", ""), "require_phone": a.settingBool("qr", "require_phone", false),
		"call_waiter": a.settingBool("qr", "allow_call_waiter", true), "currency": a.publicCurrency(), "logo": a.settingGroup("appearance")["logo"], "color": a.settingStr("appearance", "primary_color", "#f97316"),
		"menu": jsonRaw(m.json),
	}))
}

type jsonRaw []byte

func (j jsonRaw) MarshalJSON() ([]byte, error) { return j, nil }

func (a *App) publicCurrency() map[string]any {
	return map[string]any{"symbol": a.settingStr("currency", "symbol", a.cfg.Currency), "position": a.settingStr("currency", "position", "before")}
}

var (
	callMu   sync.Mutex
	lastCall = map[string]time.Time{}
)

func (a *App) handleQROrder(w http.ResponseWriter, r *http.Request) {
	if !a.settingBool("qr", "enabled", true) {
		writeErr(w, 403, "QR ordering is turned off")
		return
	}
	var in struct {
		T     string   `json:"t"`
		Name  string   `json:"name"`
		Phone string   `json:"phone"`
		Note  string   `json:"note"`
		Items []ItemIn `json:"items"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if len(in.Items) > 40 {
		writeErr(w, 400, "too many items in one order")
		return
	}
	if a.settingBool("qr", "require_phone", false) && len(in.Phone) < 7 {
		writeErr(w, 400, "please enter your phone number")
		return
	}
	var out *Order
	err := a.tx(func(tx *sql.Tx) error {
		t, err := a.tableByToken(tx, in.T)
		if err != nil {
			return err
		}
		if t.Status == "blocked" {
			return uerr("this table is not available")
		}
		if !qrAllow(t.ID) {
			return uerr("too many orders from this table - please call the waiter")
		}
		guest := &Claims{UID: 0, User: "guest", Name: "QR guest", Role: "guest", Branch: t.Branch}
		oi := &OrderIn{Channel: "dine_in", TableID: t.ID, Customer: in.Name, Phone: in.Phone, Note: in.Note, Items: in.Items, Source: "qr", BranchID: t.Branch,
			Hold: !a.settingBool("qr", "auto_accept", true)}
		out, err = a.placeOrder(tx, guest, oi)
		return err
	})
	if err != nil {
		writeErr(w, 400, friendlyErr(err))
		return
	}
	a.hub.PublishBranch(out.Branch, "order")
	if out.Status == "held" {
		a.notify(RoleCashier, out.Branch, "info", "QR order waiting: "+out.TableName, out.No+" needs staff approval", "#/pos")
	}
	writeJSON(w, 201, map[string]any{"id": out.ID, "no": out.No, "status": out.Status, "total": out.Total, "table": out.TableName})
}

// guests follow their order
func (a *App) handleQROrderStatus(w http.ResponseWriter, r *http.Request) {
	t, err := a.tableByToken(a.db.rdb, qStr(r, "t"))
	if err != nil {
		writeErr(w, 404, "invalid QR code")
		return
	}
	id, _ := strconv.ParseInt(qStr(r, "id"), 10, 64)
	o, err := loadOrder(a.db.rdb, id, false)
	if err != nil || o.TableID != t.ID || time.Now().Unix()-o.CreatedAt > 6*3600 {
		writeErr(w, 404, "order not found")
		return
	}
	items := make([]map[string]any, 0, len(o.Items))
	for _, it := range o.Items {
		if it.Status != "void" {
			items = append(items, map[string]any{"name": it.Name, "qty": it.Qty, "status": it.Status})
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{"no": o.No, "status": o.Status, "total": o.Total, "paid": o.PayStatus == "paid", "items": items})
}

func (a *App) handleQRCall(w http.ResponseWriter, r *http.Request) {
	var in struct{ T string }
	if !readJSON(w, r, &in) {
		return
	}
	t, err := a.tableByToken(a.db.rdb, in.T)
	if err != nil || !a.settingBool("qr", "allow_call_waiter", true) {
		writeErr(w, 404, "not available")
		return
	}
	callMu.Lock()
	if time.Since(lastCall[in.T]) < 30*time.Second {
		callMu.Unlock()
		writeErr(w, 429, "the waiter has already been called")
		return
	}
	if len(lastCall) > 5000 {
		lastCall = map[string]time.Time{}
	}
	lastCall[in.T] = time.Now()
	callMu.Unlock()
	a.notify(RoleWaiter, t.Branch, "warn", "Table "+t.S("name")+" is calling", "A guest asked for a waiter", "#/pos")
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// GET /api/qrcode?data=...&size=256  -> PNG (used to print table QR codes and 2FA setup codes)
func (a *App) handleQRImage(w http.ResponseWriter, r *http.Request) {
	data := qStr(r, "data")
	if data == "" || len(data) > 600 {
		writeErr(w, 400, "nothing to encode")
		return
	}
	size := int(qInt(r, "size"))
	if size < 96 || size > 1024 {
		size = 256
	}
	png, err := qrcode.Encode(data, qrcode.Medium, size)
	if err != nil {
		writeErr(w, 500, "could not make the code")
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Write(png)
}

// qrAllow lets one table send at most 8 QR orders per 10 minutes (guests append to the
// table's open bill, so counting database orders would not catch a flood).
var (
	qrMu    sync.Mutex
	qrCount = map[int64][]time.Time{}
)

func qrAllow(table int64) bool {
	qrMu.Lock()
	defer qrMu.Unlock()
	if len(qrCount) > 5000 {
		qrCount = map[int64][]time.Time{}
	}
	cut := time.Now().Add(-10 * time.Minute)
	keep := qrCount[table][:0]
	for _, t := range qrCount[table] {
		if t.After(cut) {
			keep = append(keep, t)
		}
	}
	if len(keep) >= 8 {
		qrCount[table] = keep
		return false
	}
	qrCount[table] = append(keep, time.Now())
	return true
}
