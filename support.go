package main

import (
	"database/sql"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// =====================================================================
//  Support: any signed-in user can report a problem to the super admin,
//  follow the replies, and the super admin works the queue.
//  Tickets are plain records (res "support_tickets"); status lives in the
//  status column: open -> in_progress -> resolved -> closed.
// =====================================================================

const ticketRes = "support_tickets"

var ticketStatuses = map[string]bool{"open": true, "in_progress": true, "resolved": true, "closed": true}

func ticketJSON(r *Rec, full bool) map[string]any {
	m := map[string]any{"id": r.ID, "no": r.Ref, "status": r.Status, "subject": r.S("subject"), "category": r.S("category"), "priority": r.S("priority"),
		"reporter": r.S("reporter_name"), "reporter_id": r.I("reporter"), "role": r.S("role"), "branch_id": r.Branch, "created_at": r.CreatedAt, "updated_at": r.UpdatedAt,
		"replies_n": len(r.L("replies"))}
	if full {
		m["message"], m["page"], m["agent"], m["replies"] = r.S("message"), r.S("page"), r.S("agent"), r.L("replies")
	}
	return m
}

func (a *App) canSeeTicket(c *Claims, r *Rec) bool {
	return r.Res == ticketRes && (a.level(c.Role, "support") >= LvView || r.I("reporter") == c.UID)
}

// POST /api/support {subject, message, category, priority, page}
func (a *App) handleSupportCreate(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	var in struct {
		Subject, Message, Category, Priority, Page string
	}
	if !readJSON(w, r, &in) {
		return
	}
	in.Subject, in.Message = strings.TrimSpace(in.Subject), strings.TrimSpace(in.Message)
	if in.Subject == "" || len(in.Message) < 5 {
		writeErr(w, 400, "please write a short title and describe the problem")
		return
	}
	if !map[string]bool{"bug": true, "billing": true, "kitchen": true, "printing": true, "login": true, "report": true, "other": true}[in.Category] {
		in.Category = "other"
	}
	if !map[string]bool{"low": true, "normal": true, "high": true, "urgent": true}[in.Priority] {
		in.Priority = "normal"
	}
	var open int
	a.db.rdb.QueryRow(`SELECT COUNT(*) FROM records WHERE res=? AND created_by=? AND status IN ('open','in_progress')`, ticketRes, c.UID).Scan(&open)
	if open >= 15 {
		writeErr(w, 429, "you already have many open tickets - please wait for a reply")
		return
	}
	var out *Rec
	err := a.tx(func(tx *sql.Tx) error {
		rec := &Rec{Res: ticketRes, Branch: c.Branch, Status: "open", CreatedBy: c.UID, Data: map[string]any{
			"subject": trunc(in.Subject, 120), "message": trunc(in.Message, 4000), "category": in.Category, "priority": in.Priority,
			"page": trunc(in.Page, 200), "agent": trunc(r.UserAgent(), 200), "reporter": float64(c.UID), "reporter_name": c.Name, "role": c.Role, "replies": []any{}}}
		rec.Ref = docNo(tx, ticketRes, "TKT")
		if err := saveRec(tx, rec); err != nil {
			return err
		}
		out = rec
		return nil
	})
	if err != nil {
		writeErr(w, 500, "could not save the ticket")
		return
	}
	lvl := "info"
	if in.Priority == "high" || in.Priority == "urgent" {
		lvl = "warn"
	}
	a.notify(RoleSuperAdmin, 0, lvl, "Support: "+out.S("subject"), c.Name+" ("+c.Role+") · "+in.Priority, "#/support_admin")
	a.hub.Publish("res:" + ticketRes)
	a.audit(r, ticketRes, out.ID, "create", out.Ref)
	writeJSON(w, 201, ticketJSON(out, true))
}

func (a *App) handleSupportMine(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	out := []map[string]any{}
	for _, t := range recsDesc(a.db.rdb, ticketRes, `json_extract(data,'$.reporter')=?`, c.UID) {
		out = append(out, ticketJSON(t, false))
	}
	if out == nil {
		out = []map[string]any{}
	}
	writeJSON(w, 200, out)
}

// recsDesc loads the 100 newest records of a resource that match an extra condition.
func recsDesc(q Querier, res, cond string, args ...any) []*Rec {
	rows, err := q.Query(`SELECT `+recCols+` FROM records WHERE res=? AND `+cond+` ORDER BY id DESC LIMIT 100`, append([]any{res}, args...)...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []*Rec
	for rows.Next() {
		if r, err := scanRec(rows); err == nil {
			out = append(out, r)
		}
	}
	return out
}

// GET /api/support?status=&q=&page=   (super admin)
func (a *App) handleSupportList(w http.ResponseWriter, r *http.Request) {
	where, args := "res=?", []any{ticketRes}
	if s := qStr(r, "status"); s != "" {
		where += " AND status=?"
		args = append(args, s)
	}
	if q := strings.ToLower(qStr(r, "q")); q != "" {
		where += " AND (lower(ref) LIKE ? OR lower(json_extract(data,'$.subject')) LIKE ? OR lower(json_extract(data,'$.reporter_name')) LIKE ?)"
		args = append(args, "%"+q+"%", "%"+q+"%", "%"+q+"%")
	}
	limit, page := 25, int(max(qInt(r, "page"), 1))
	var total int64
	a.db.rdb.QueryRow(`SELECT COUNT(*) FROM records WHERE `+where, args...).Scan(&total)
	rows, err := a.db.rdb.Query(`SELECT `+recCols+` FROM records WHERE `+where+` ORDER BY CASE status WHEN 'open' THEN 0 WHEN 'in_progress' THEN 1 ELSE 2 END, id DESC LIMIT ? OFFSET ?`, append(args, limit, (page-1)*limit)...)
	if err != nil {
		writeErr(w, 500, "database error")
		return
	}
	defer rows.Close()
	list := []map[string]any{}
	for rows.Next() {
		if t, err := scanRec(rows); err == nil {
			list = append(list, ticketJSON(t, false))
		}
	}
	counts := map[string]int64{}
	for _, m := range a.queryMaps(`SELECT status,COUNT(*) n FROM records WHERE res=? GROUP BY status`, ticketRes) {
		counts[toStr(m["status"])] = toInt(m["n"])
	}
	writeJSON(w, 200, map[string]any{"rows": list, "total": total, "page": page, "limit": limit, "counts": counts})
}

func (a *App) handleSupportGet(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	t, err := loadRec(a.db.rdb, id)
	if err != nil || !a.canSeeTicket(userFrom(r), t) {
		writeErr(w, 404, "ticket not found")
		return
	}
	writeJSON(w, 200, ticketJSON(t, true))
}

// POST /api/support/{id}/reply {text}
func (a *App) handleSupportReply(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var in struct{ Text string }
	if !readJSON(w, r, &in) {
		return
	}
	in.Text = trunc(in.Text, 4000)
	if in.Text == "" {
		writeErr(w, 400, "write a message first")
		return
	}
	var out *Rec
	staff := a.level(c.Role, "support") >= LvManage
	err := a.tx(func(tx *sql.Tx) error {
		t, err := loadRec(tx, id)
		if err != nil || !a.canSeeTicket(c, t) {
			return uerr("ticket not found")
		}
		if t.Status == "closed" {
			return uerr("this ticket is closed - open a new one")
		}
		reps := t.L("replies")
		arr := make([]any, 0, len(reps)+1)
		for _, x := range reps {
			arr = append(arr, x)
		}
		arr = append(arr, map[string]any{"by": float64(c.UID), "name": c.Name, "role": c.Role, "staff": staff, "text": in.Text, "at": float64(time.Now().Unix())})
		t.Set("replies", arr)
		if staff && t.Status == "open" {
			t.Status = "in_progress"
		}
		if !staff && t.Status == "resolved" {
			t.Status = "open" // the reporter says it is not fixed
		}
		out = t
		return saveRec(tx, t)
	})
	if err != nil {
		writeErr(w, 400, friendlyErr(err))
		return
	}
	if staff {
		a.notifyUser(out.I("reporter"), "info", "Reply on "+out.Ref, trunc(in.Text, 120), "#/support")
	} else {
		a.notify(RoleSuperAdmin, 0, "info", "New reply on "+out.Ref, c.Name+": "+trunc(in.Text, 100), "#/support_admin")
	}
	a.hub.Publish("res:" + ticketRes)
	writeJSON(w, 200, ticketJSON(out, true))
}

// POST /api/support/{id}/status {status}   (super admin)
func (a *App) handleSupportStatus(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var in struct{ Status string }
	if !readJSON(w, r, &in) || !ticketStatuses[in.Status] {
		writeErr(w, 400, "invalid status")
		return
	}
	var out *Rec
	err := a.tx(func(tx *sql.Tx) error {
		t, err := loadRec(tx, id)
		if err != nil || t.Res != ticketRes {
			return uerr("ticket not found")
		}
		t.Status = in.Status
		out = t
		return saveRec(tx, t)
	})
	if err != nil {
		writeErr(w, 400, friendlyErr(err))
		return
	}
	if in.Status == "resolved" || in.Status == "closed" {
		a.notifyUser(out.I("reporter"), "info", out.Ref+" is "+in.Status, out.S("subject"), "#/support")
	}
	a.audit(r, ticketRes, id, "status", in.Status)
	a.hub.Publish("res:" + ticketRes)
	writeJSON(w, 200, ticketJSON(out, true))
}
