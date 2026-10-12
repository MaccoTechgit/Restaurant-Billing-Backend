package main

import (
	"database/sql"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// =====================================================================
//  HR: attendance (check-in/out, geofence), leave balances & approvals,
//  overtime, payroll calculation and posting.
// =====================================================================

func (a *App) notifyUser(uid int64, level, title, body, link string) {
	if uid > 0 {
		a.bgExec(`INSERT INTO notifications(ts,user_id,level,title,body,link) VALUES(?,?,?,?,?,?)`, nowUnix(), uid, level, trunc(title, 150), trunc(body, 400), link)
		a.hub.Publish("notify")
	}
}

func parseDate(s string) (time.Time, bool) {
	t, err := time.ParseInLocation("2006-01-02", s, time.Local)
	return t, err == nil
}

func yearsBetween(from, to string) float64 {
	f, ok1 := parseDate(from)
	t, ok2 := parseDate(to)
	if !ok1 || !ok2 || t.Before(f) {
		return 0
	}
	return math.Round(t.Sub(f).Hours()/24/365.25*100) / 100
}

func (a *App) weeklyOff() time.Weekday {
	names := map[string]time.Weekday{"Sunday": 0, "Monday": 1, "Tuesday": 2, "Wednesday": 3, "Thursday": 4, "Friday": 5, "Saturday": 6}
	return names[a.settingStr("hr", "weekly_off", "Friday")]
}

func (a *App) holidaySet(q Querier, branch int64) map[string]bool {
	m := map[string]bool{}
	for _, h := range recs(q, "holidays", branch) {
		m[h.S("date")] = true
	}
	return m
}

// leaveDays counts the working days between two dates (skips weekly off & holidays).
func (a *App) leaveDays(q Querier, branch int64, from, to string, half bool) float64 {
	f, ok1 := parseDate(from)
	t, ok2 := parseDate(to)
	if !ok1 || !ok2 || t.Before(f) {
		return 0
	}
	hol, off := a.holidaySet(q, branch), a.weeklyOff()
	var n float64
	for d := f; !d.After(t); d = d.AddDate(0, 0, 1) {
		if d.Weekday() == off || hol[d.Format("2006-01-02")] {
			continue
		}
		n++
	}
	if half && from == to && n > 0 {
		return 0.5
	}
	return n
}

func (a *App) employeeBasic(q Querier, emp *Rec) int64 {
	if b := emp.I("basic_salary"); b > 0 {
		return b
	}
	if s, err := loadRec(q, emp.I("salary_structure")); err == nil && s.Res == "salary_structures" {
		return s.I("basic")
	}
	return 0
}

// ---------- leave balances ----------

// entitlement = days this employee gets this year for this leave type.
func (a *App) entitlement(q Querier, emp *Rec, lt *Rec, year int) float64 {
	ent := lt.N("days_per_year")
	months := yearsBetween(emp.S("join_date"), fmt.Sprintf("%d-12-31", year)) * 12
	for _, p := range recs(q, "leave_policies", 0) {
		if p.Status == "active" && p.I("leave_type") == lt.ID && (p.I("department") == 0 || p.I("department") == emp.I("department")) && months >= p.N("min_service_months") {
			ent = p.N("entitlement")
		}
	}
	return ent
}

func (a *App) balanceOf(q Querier, emp *Rec, lt *Rec, year int) (entitled, used float64) {
	err := q.QueryRow(`SELECT entitled,used FROM leave_balances WHERE employee_id=? AND leave_type_id=? AND year=?`, emp.ID, lt.ID, year).Scan(&entitled, &used)
	if err != nil {
		entitled = a.entitlement(q, emp, lt, year)
		if lt.B("carry_forward") {
			var pe, pu float64
			if q.QueryRow(`SELECT entitled,used FROM leave_balances WHERE employee_id=? AND leave_type_id=? AND year=?`, emp.ID, lt.ID, year-1).Scan(&pe, &pu) == nil {
				entitled += math.Min(math.Max(pe-pu, 0), lt.N("max_carry"))
			}
		}
		used = 0
	}
	return
}

func (a *App) ensureBalance(tx Querier, emp *Rec, lt *Rec, year int) {
	e, u := a.balanceOf(tx, emp, lt, year)
	tx.Exec(`INSERT OR IGNORE INTO leave_balances(employee_id,leave_type_id,year,entitled,used) VALUES(?,?,?,?,?)`, emp.ID, lt.ID, year, e, u)
}

func leaveBefore(a *App, tx *sql.Tx, c *Claims, r *Rec, old *Rec) error {
	if old != nil && old.Status != "pending" {
		return uerr("only pending requests can be edited")
	}
	if r.S("from") > r.S("to") {
		return uerr("the end date is before the start date")
	}
	emp, err := loadRec(tx, r.I("employee"))
	if err != nil || emp.Res != "employees" {
		return uerr("choose an employee")
	}
	lt, err := loadRec(tx, r.I("leave_type"))
	if err != nil || lt.Res != "leave_types" {
		return uerr("choose a leave type")
	}
	if lt.B("requires_doc") && r.S("document") == "" && r.N("days") > 2 {
		return uerr("%s needs a supporting document", lt.S("name"))
	}
	days := a.leaveDays(tx, emp.Branch, r.S("from"), r.S("to"), r.B("half_day"))
	if days <= 0 {
		return uerr("those dates are all holidays or days off")
	}
	r.Set("days", days)
	r.Branch = emp.Branch
	var n int
	tx.QueryRow(`SELECT COUNT(*) FROM records WHERE res='leave_requests' AND status IN ('pending','approved') AND id<>? AND json_extract(data,'$.employee')=?
		AND json_extract(data,'$.from')<=? AND json_extract(data,'$.to')>=?`, r.ID, emp.ID, r.S("to"), r.S("from")).Scan(&n)
	if n > 0 {
		return uerr("this employee already has leave in those dates")
	}
	if lt.B("paid") {
		year, _ := strconv.Atoi(r.S("from")[:4])
		ent, used := a.balanceOf(tx, emp, lt, year)
		if used+days > ent+0.001 {
			return uerr("not enough %s balance: %.1f day(s) left, you asked for %.1f", lt.S("name"), ent-used, days)
		}
	}
	if old == nil { // auto-approval rules
		for _, ru := range recs(tx, "leave_rules", 0) {
			if ru.Status == "active" && (ru.I("leave_type") == 0 || ru.I("leave_type") == lt.ID) && ru.N("auto_approve_below") > 0 && days <= ru.N("auto_approve_below") {
				r.Status = "approved"
				r.Set("reviewed_by", "auto-approved")
				break
			}
		}
	}
	return nil
}

func leaveAfter(a *App, tx *sql.Tx, c *Claims, r *Rec, old *Rec) error {
	if old == nil && r.Status == "approved" {
		return a.leaveApply(tx, r)
	}
	if old == nil && r.Status == "pending" {
		a.notify(RoleHRManager, r.Branch, "info", "New leave request", fmt.Sprintf("%.1f day(s) from %s", r.N("days"), r.S("from")), "#/leave_requests")
	}
	return nil
}

// leaveApply deducts the balance and marks the days as leave in attendance.
func (a *App) leaveApply(tx Querier, r *Rec) error {
	emp, err := loadRec(tx, r.I("employee"))
	if err != nil {
		return err
	}
	lt, _ := loadRec(tx, r.I("leave_type"))
	paid := lt != nil && lt.B("paid")
	if paid {
		year, _ := strconv.Atoi(r.S("from")[:4])
		a.ensureBalance(tx, emp, lt, year)
		tx.Exec(`UPDATE leave_balances SET used=used+? WHERE employee_id=? AND leave_type_id=? AND year=?`, r.N("days"), emp.ID, lt.ID, year)
	}
	note := "paid leave"
	if !paid {
		note = "unpaid"
	}
	f, _ := parseDate(r.S("from"))
	t, _ := parseDate(r.S("to"))
	hol, off := a.holidaySet(tx, emp.Branch), a.weeklyOff()
	for d := f; !d.After(t); d = d.AddDate(0, 0, 1) {
		ds := d.Format("2006-01-02")
		if d.Weekday() == off || hol[ds] {
			continue
		}
		tx.Exec(`INSERT INTO attendance(employee_id,branch_id,date,status,source,note) VALUES(?,?,?,'leave','leave',?)
			ON CONFLICT(employee_id,date) DO UPDATE SET status='leave',note=excluded.note WHERE check_in=0`, emp.ID, emp.Branch, ds, note)
	}
	if u := emp.I("user"); u > 0 {
		a.notifyUser(u, "info", "Leave approved", fmt.Sprintf("%s to %s", r.S("from"), r.S("to")), "#/my_hr")
	}
	return nil
}

func leaveApprove(a *App, tx *sql.Tx, c *Claims, r *Rec, p map[string]any) error {
	r.Status = "approved"
	r.Set("reviewed_by", c.Name)
	r.Set("review_note", trunc(toStr(p["text"]), 200))
	return a.leaveApply(tx, r)
}

func leaveCancel(a *App, tx *sql.Tx, c *Claims, r *Rec, p map[string]any) error {
	if r.Status == "approved" {
		emp, _ := loadRec(tx, r.I("employee"))
		lt, _ := loadRec(tx, r.I("leave_type"))
		if emp != nil && lt != nil && lt.B("paid") {
			year, _ := strconv.Atoi(r.S("from")[:4])
			tx.Exec(`UPDATE leave_balances SET used=MAX(0,used-?) WHERE employee_id=? AND leave_type_id=? AND year=?`, r.N("days"), emp.ID, lt.ID, year)
		}
		tx.Exec(`DELETE FROM attendance WHERE employee_id=? AND date>=? AND date<=? AND status='leave' AND check_in=0`, r.I("employee"), r.S("from"), r.S("to"))
	}
	r.Status = "cancelled"
	return nil
}

// ---------- attendance ----------

func (a *App) myEmployee(q Querier, uid int64) *Rec {
	var id int64
	if q.QueryRow(`SELECT id FROM records WHERE res='employees' AND json_extract(data,'$.user')=? LIMIT 1`, uid).Scan(&id) != nil {
		return nil
	}
	r, _ := loadRec(q, id)
	return r
}

func haversineM(lat1, lng1, lat2, lng2 float64) float64 {
	const R = 6371000
	rad := math.Pi / 180
	dLat, dLng := (lat2-lat1)*rad, (lng2-lng1)*rad
	x := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLng/2)*math.Sin(dLng/2)
	return 2 * R * math.Asin(math.Sqrt(x))
}

func (a *App) pickEmployee(tx Querier, c *Claims, id int64) (*Rec, error) {
	if id > 0 && id != 0 {
		if a.level(c.Role, "attendance") < LvManage {
			if me := a.myEmployee(tx, c.UID); me == nil || me.ID != id {
				return nil, uerr("you can only record your own attendance")
			}
		}
		e, err := loadRec(tx, id)
		if err != nil || e.Res != "employees" || (c.Branch != 0 && e.Branch != 0 && e.Branch != c.Branch) {
			return nil, uerr("employee not found")
		}
		return e, nil
	}
	e := a.myEmployee(tx, c.UID)
	if e == nil {
		return nil, uerr("your login is not linked to an employee record - ask HR")
	}
	return e, nil
}

func hm(s string, base time.Time) (time.Time, bool) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return time.Time{}, false
	}
	return time.Date(base.Year(), base.Month(), base.Day(), t.Hour(), t.Minute(), 0, 0, base.Location()), true
}

func (a *App) handleCheckIn(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	var in struct {
		Employee int64   `json:"employee"`
		Lat      float64 `json:"lat"`
		Lng      float64 `json:"lng"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	var msg string
	err := a.tx(func(tx *sql.Tx) error {
		emp, err := a.pickEmployee(tx, c, in.Employee)
		if err != nil {
			return err
		}
		now := time.Now()
		date := now.Format("2006-01-02")
		var cin int64
		tx.QueryRow(`SELECT check_in FROM attendance WHERE employee_id=? AND date=?`, emp.ID, date).Scan(&cin)
		if cin > 0 {
			return uerr("already checked in today at %s", time.Unix(cin, 0).Format("15:04"))
		}
		if a.settingBool("hr", "geofence_required", false) {
			var fences []*Rec
			for _, g := range recs(tx, "geofences", emp.Branch) {
				if g.Status == "active" {
					fences = append(fences, g)
				}
			}
			if len(fences) > 0 {
				if in.Lat == 0 && in.Lng == 0 {
					return uerr("location is required - allow location access and try again")
				}
				ok := false
				for _, g := range fences {
					if haversineM(in.Lat, in.Lng, g.N("lat"), g.N("lng")) <= g.N("radius_m") {
						ok = true
					}
				}
				if !ok {
					return uerr("you are outside the allowed check-in area")
				}
			}
		}
		status, late := "present", 0
		if sh, err := loadRec(tx, emp.I("shift")); err == nil && sh.Res == "shifts" {
			if st, ok := hm(sh.S("start"), now); ok {
				grace := time.Duration(a.settingNum("hr", "late_grace_min", 10)) * time.Minute
				if g := sh.N("grace_min"); g > 0 {
					grace = time.Duration(g) * time.Minute
				}
				if now.After(st.Add(grace)) && !sh.B("night") {
					status, late = "late", int(now.Sub(st).Minutes())
				}
			}
		}
		_, err = tx.Exec(`INSERT INTO attendance(employee_id,branch_id,date,check_in,status,late_min,source,lat,lng) VALUES(?,?,?,?,?,?,?,?,?)
			ON CONFLICT(employee_id,date) DO UPDATE SET check_in=excluded.check_in,status=excluded.status,late_min=excluded.late_min,source=excluded.source,lat=excluded.lat,lng=excluded.lng`,
			emp.ID, emp.Branch, date, now.Unix(), status, late, "app", in.Lat, in.Lng)
		msg = fmt.Sprintf("%s checked in at %s", emp.S("name"), now.Format("15:04"))
		if late > 0 {
			msg += fmt.Sprintf(" (%d min late)", late)
		}
		return err
	})
	if err != nil {
		writeErr(w, 400, friendlyErr(err))
		return
	}
	a.hub.Publish("attendance")
	writeJSON(w, 200, map[string]string{"message": msg})
}

func (a *App) handleCheckOut(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	var in struct {
		Employee int64 `json:"employee"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	var msg string
	err := a.tx(func(tx *sql.Tx) error {
		emp, err := a.pickEmployee(tx, c, in.Employee)
		if err != nil {
			return err
		}
		now := time.Now()
		date := now.Format("2006-01-02")
		var cin, cout int64
		if tx.QueryRow(`SELECT check_in,check_out FROM attendance WHERE employee_id=? AND date=?`, emp.ID, date).Scan(&cin, &cout) != nil || cin == 0 {
			return uerr("you have not checked in today")
		}
		if cout > 0 {
			return uerr("already checked out today")
		}
		worked := int((now.Unix() - cin) / 60)
		if sh, err := loadRec(tx, emp.I("shift")); err == nil && sh.Res == "shifts" && worked > 300 {
			worked -= int(sh.N("break_min"))
		}
		ot := max(0, worked-int(a.settingNum("hr", "work_hours", 8)*60))
		status := ""
		if float64(worked)/60 < a.settingNum("hr", "half_day_hours", 4) {
			status = ",status='half_day'"
		}
		_, err = tx.Exec(`UPDATE attendance SET check_out=?,worked_min=?,ot_min=?`+status+` WHERE employee_id=? AND date=?`, now.Unix(), worked, ot, emp.ID, date)
		msg = fmt.Sprintf("Checked out. Worked %dh %02dm", worked/60, worked%60)
		return err
	})
	if err != nil {
		writeErr(w, 400, friendlyErr(err))
		return
	}
	a.hub.Publish("attendance")
	writeJSON(w, 200, map[string]string{"message": msg})
}

// GET /api/hr/attendance?from=&to=&employee=&status=&department=&page=&limit=
func (a *App) handleAttList(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	where, args := "1=1", []any{}
	if b := scopeBranch(c, r); b != 0 {
		where += " AND t.branch_id=?"
		args = append(args, b)
	}
	if v := qStr(r, "from"); v != "" {
		where += " AND t.date>=?"
		args = append(args, v)
	}
	if v := qStr(r, "to"); v != "" {
		where += " AND t.date<=?"
		args = append(args, v)
	}
	if v := qInt(r, "employee"); v > 0 {
		where += " AND t.employee_id=?"
		args = append(args, v)
	}
	if v := qStr(r, "status"); v != "" {
		where += " AND t.status=?"
		args = append(args, v)
	}
	if v := qInt(r, "department"); v > 0 {
		where += " AND t.employee_id IN (SELECT id FROM records WHERE res='employees' AND json_extract(data,'$.department')=?)"
		args = append(args, v)
	}
	limit := int(qInt(r, "limit"))
	if limit < 1 || limit > 1000 {
		limit = 30
	}
	page := int(max(qInt(r, "page"), 1))
	var total int64
	a.db.rdb.QueryRow(`SELECT COUNT(*) FROM attendance t WHERE `+where, args...).Scan(&total)
	rows := a.queryMaps(`SELECT t.id,t.employee_id,t.branch_id,t.date,t.check_in,t.check_out,t.status,t.late_min,t.worked_min,t.ot_min,t.source,t.note FROM attendance t WHERE `+where+
		` ORDER BY t.date DESC,t.id DESC LIMIT ? OFFSET ?`, append(args, limit, (page-1)*limit)...)
	names := a.labelMap("employees")
	for _, m := range rows {
		m["employee"] = names[toInt(m["employee_id"])]
	}
	writeJSON(w, 200, map[string]any{"rows": rows, "total": total, "page": page, "limit": limit})
}

func (a *App) handleAttSave(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	var in struct {
		Employee int64  `json:"employee"`
		Date     string `json:"date"`
		In       string `json:"in"`
		Out      string `json:"out"`
		Status   string `json:"status"`
		Note     string `json:"note"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	err := a.tx(func(tx *sql.Tx) error {
		emp, err := loadRec(tx, in.Employee)
		if err != nil || emp.Res != "employees" || (c.Branch != 0 && emp.Branch != c.Branch && emp.Branch != 0) {
			return uerr("employee not found")
		}
		d, ok := parseDate(in.Date)
		if !ok {
			return uerr("choose a date")
		}
		var cin, cout int64
		if t, ok := hm(in.In, d); ok {
			cin = t.Unix()
		}
		if t, ok := hm(in.Out, d); ok {
			cout = t.Unix()
		}
		worked, ot := 0, 0
		if cin > 0 && cout > cin {
			worked = int((cout - cin) / 60)
			ot = max(0, worked-int(a.settingNum("hr", "work_hours", 8)*60))
		}
		st := in.Status
		valid := map[string]bool{"present": true, "absent": true, "late": true, "leave": true, "holiday": true, "half_day": true}
		if !valid[st] {
			st = "present"
		}
		_, err = tx.Exec(`INSERT INTO attendance(employee_id,branch_id,date,check_in,check_out,status,worked_min,ot_min,source,note) VALUES(?,?,?,?,?,?,?,?,'manual',?)
			ON CONFLICT(employee_id,date) DO UPDATE SET check_in=excluded.check_in,check_out=excluded.check_out,status=excluded.status,worked_min=excluded.worked_min,ot_min=excluded.ot_min,note=excluded.note`,
			emp.ID, emp.Branch, in.Date, cin, cout, st, worked, ot, trunc(in.Note, 200))
		return err
	})
	if err != nil {
		writeErr(w, 400, friendlyErr(err))
		return
	}
	a.audit(r, "attendance", in.Employee, "save", in.Date)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *App) handleAttDelete(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if c := userFrom(r); c.Branch != 0 {
		var b int64
		if a.db.rdb.QueryRow(`SELECT branch_id FROM attendance WHERE id=?`, id).Scan(&b) != nil || b != c.Branch {
			writeErr(w, 404, "record not found")
			return
		}
	}
	a.db.wdb.Exec(`DELETE FROM attendance WHERE id=?`, id)
	a.audit(r, "attendance", id, "delete", "")
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// Creates "absent" rows for everyone who has no record on a working day.
func (a *App) handleAttGenerate(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	var in struct{ From, To string }
	if !readJSON(w, r, &in) {
		return
	}
	var created int
	err := a.tx(func(tx *sql.Tx) error {
		f, ok1 := parseDate(in.From)
		t, ok2 := parseDate(in.To)
		if !ok1 || !ok2 || t.Before(f) || t.Sub(f) > 93*24*time.Hour {
			return uerr("choose a valid date range (max 3 months)")
		}
		yesterday := time.Now().AddDate(0, 0, -1)
		if t.After(yesterday) {
			t = yesterday
		}
		off := a.weeklyOff()
		for _, e := range recs(tx, "employees", c.Branch) {
			if e.Status != "active" {
				continue
			}
			hol := a.holidaySet(tx, e.Branch)
			for d := f; !d.After(t); d = d.AddDate(0, 0, 1) {
				ds := d.Format("2006-01-02")
				if d.Weekday() == off || hol[ds] || ds < e.S("join_date") || (e.S("leave_date") != "" && ds > e.S("leave_date")) {
					continue
				}
				res, _ := tx.Exec(`INSERT OR IGNORE INTO attendance(employee_id,branch_id,date,status,source) VALUES(?,?,?,'absent','auto')`, e.ID, e.Branch, ds)
				if n, _ := res.RowsAffected(); n > 0 {
					created++
				}
			}
		}
		return nil
	})
	if err != nil {
		writeErr(w, 400, friendlyErr(err))
		return
	}
	a.audit(r, "attendance", 0, "generate", fmt.Sprintf("%d absences %s..%s", created, in.From, in.To))
	writeJSON(w, 200, map[string]int{"created": created})
}

// per-employee summary for a date range
func (a *App) handleAttSummary(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	from, to := qStr(r, "from"), qStr(r, "to")
	if from == "" || to == "" {
		to = today()
		from = time.Now().AddDate(0, 0, -29).Format("2006-01-02")
	}
	where, args := "date>=? AND date<=?", []any{from, to}
	if b := scopeBranch(c, r); b != 0 {
		where += " AND branch_id=?"
		args = append(args, b)
	}
	rows := a.queryMaps(`SELECT employee_id,SUM(status='present') present,SUM(status='late') late,SUM(status='absent') absent,SUM(status='leave') leave,SUM(status='half_day') half,
		SUM(worked_min) worked,SUM(ot_min) ot,SUM(late_min) late_min FROM attendance WHERE `+where+` GROUP BY employee_id`, args...)
	names := a.labelMap("employees")
	for _, m := range rows {
		m["employee"] = names[toInt(m["employee_id"])]
	}
	sort.Slice(rows, func(i, j int) bool { return toStr(rows[i]["employee"]) < toStr(rows[j]["employee"]) })
	writeJSON(w, 200, rows)
}

func (a *App) handleLeaveBalances(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	year, _ := strconv.Atoi(qStr(r, "year"))
	if year == 0 {
		year = time.Now().Year()
	}
	types := []*Rec{}
	for _, t := range recs(a.db.rdb, "leave_types", 0) {
		if t.Status == "active" {
			types = append(types, t)
		}
	}
	out := []map[string]any{}
	for _, e := range recs(a.db.rdb, "employees", scopeBranch(c, r)) {
		if e.Status != "active" && e.Status != "on_leave" {
			continue
		}
		if emp := qInt(r, "employee"); emp > 0 && emp != e.ID {
			continue
		}
		for _, t := range types {
			ent, used := a.balanceOf(a.db.rdb, e, t, year)
			out = append(out, map[string]any{"employee_id": e.ID, "employee": e.S("name"), "code": e.Ref, "leave_type_id": t.ID, "leave_type": t.S("name"),
				"entitled": ent, "used": used, "remaining": ent - used, "paid": t.B("paid")})
		}
	}
	writeJSON(w, 200, out)
}

func (a *App) handleLeaveReport(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	from, to := qStr(r, "from"), qStr(r, "to")
	if from == "" || to == "" {
		from, to = time.Now().Format("2006")+"-01-01", time.Now().Format("2006")+"-12-31"
	}
	types := a.labelMap("leave_types")
	emps := a.labelMap("employees")
	byType, byEmp := map[string]float64{}, map[string]float64{}
	cnt := map[string]int{}
	for _, l := range recs(a.db.rdb, "leave_requests", scopeBranch(c, r)) {
		if l.S("to") < from || l.S("from") > to {
			continue
		}
		cnt[l.Status]++
		if l.Status != "approved" {
			continue
		}
		byType[types[l.I("leave_type")]] += l.N("days")
		byEmp[emps[l.I("employee")]] += l.N("days")
	}
	writeJSON(w, 200, map[string]any{"by_type": byType, "by_employee": byEmp, "counts": cnt})
}

// ---------- my attendance & leave (self service) ----------

func (a *App) handleMyHR(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	emp := a.myEmployee(a.db.rdb, c.UID)
	if emp == nil {
		writeJSON(w, 200, map[string]any{"linked": false})
		return
	}
	var rec map[string]any
	if rows := a.queryMaps(`SELECT id,date,check_in,check_out,status,late_min,worked_min FROM attendance WHERE employee_id=? AND date=?`, emp.ID, today()); len(rows) > 0 {
		rec = rows[0]
	}
	history := a.queryMaps(`SELECT date,check_in,check_out,status,worked_min,late_min FROM attendance WHERE employee_id=? ORDER BY date DESC LIMIT 14`, emp.ID)
	if history == nil {
		history = []map[string]any{}
	}
	year := time.Now().Year()
	bal := []map[string]any{}
	for _, t := range recs(a.db.rdb, "leave_types", 0) {
		if t.Status == "active" {
			e, u := a.balanceOf(a.db.rdb, emp, t, year)
			bal = append(bal, map[string]any{"id": t.ID, "name": t.S("name"), "entitled": e, "used": u, "remaining": e - u})
		}
	}
	leaves := []map[string]any{}
	for _, l := range recs(a.db.rdb, "leave_requests", 0) {
		if l.I("employee") == emp.ID {
			leaves = append(leaves, l.JSON())
		}
	}
	sort.Slice(leaves, func(i, j int) bool { return toInt(leaves[i]["id"]) > toInt(leaves[j]["id"]) })
	if len(leaves) > 10 {
		leaves = leaves[:10]
	}
	writeJSON(w, 200, map[string]any{"linked": true, "employee": emp.JSON(), "today": rec, "history": history, "balances": bal, "leaves": leaves,
		"geofence": a.settingBool("hr", "geofence_required", false)})
}

func (a *App) handleMyLeave(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	var body map[string]any
	if !readJSON(w, r, &body) {
		return
	}
	var out *Rec
	err := a.tx(func(tx *sql.Tx) error {
		emp := a.myEmployee(tx, c.UID)
		if emp == nil {
			return uerr("your login is not linked to an employee record")
		}
		body["employee"] = float64(emp.ID)
		res := registry["leave_requests"]
		rec, err := a.buildRec(res, body, nil)
		if err != nil {
			return err
		}
		rec.CreatedBy = c.UID
		if err := leaveBefore(a, tx, c, rec, nil); err != nil {
			return err
		}
		if err := saveRec(tx, rec); err != nil {
			return err
		}
		out = rec
		return leaveAfter(a, tx, c, rec, nil)
	})
	if err != nil {
		writeErr(w, 400, friendlyErr(err))
		return
	}
	bumpRes("leave_requests")
	a.hub.Publish("res:leave_requests")
	writeJSON(w, 201, out.JSON())
}

// ---------- HR dashboard ----------

func (a *App) handleHRDash(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	b := scopeBranch(c, r)
	from, to := qStr(r, "from"), qStr(r, "to")
	if from == "" || to == "" {
		from, to = time.Now().Format("2006-01")+"-01", today()
	}
	dept, shift := qInt(r, "department"), qInt(r, "shift")
	a.serveCached(w, a.dashCache, fmt.Sprintf("hr|%d|%s|%s|%d|%d|%d", b, from, to, dept, shift, verOf("employees")), 5*time.Second, func() ([]byte, error) {
		emps := []*Rec{}
		ids := map[int64]bool{}
		for _, e := range recs(a.db.rdb, "employees", b) {
			if dept > 0 && e.I("department") != dept || shift > 0 && e.I("shift") != shift {
				continue
			}
			emps = append(emps, e)
			ids[e.ID] = true
		}
		out := map[string]any{}
		var active int
		for _, e := range emps {
			if e.Status == "active" {
				active++
			}
		}
		out["total"], out["active"] = len(emps), active
		td := today()
		type cnt struct{ Present, Absent, Late, Leave int }
		day := map[string]*cnt{}
		var todayPresent, todayAbsent, todayLate, todayLeave int
		status := map[string]int{}
		checked := map[int64]bool{}
		exceptions := []map[string]any{}
		names := a.labelMap("employees")
		for _, m := range a.queryMaps(`SELECT employee_id,date,status,late_min,check_in FROM attendance WHERE date>=? AND date<=?`+func() string {
			if b != 0 {
				return " AND branch_id=" + strconv.FormatInt(b, 10)
			}
			return ""
		}()+` ORDER BY date DESC`, from, to) {
			eid := toInt(m["employee_id"])
			if !ids[eid] {
				continue
			}
			d, st := toStr(m["date"]), toStr(m["status"])
			x := day[d]
			if x == nil {
				x = &cnt{}
				day[d] = x
			}
			status[st]++
			switch st {
			case "present", "half_day":
				x.Present++
			case "late":
				x.Late++
				x.Present++
			case "absent":
				x.Absent++
			case "leave":
				x.Leave++
			}
			if d == td {
				switch st {
				case "present", "half_day":
					todayPresent++
				case "late":
					todayLate++
					todayPresent++
				case "absent":
					todayAbsent++
				case "leave":
					todayLeave++
				}
				if toInt(m["check_in"]) > 0 {
					checked[eid] = true
				}
			}
			if (st == "late" || st == "absent") && len(exceptions) < 8 {
				exceptions = append(exceptions, map[string]any{"employee": names[eid], "date": d, "status": st, "severity": map[string]string{"late": "medium", "absent": "high"}[st]})
			}
		}
		days := []string{}
		for d := range day {
			days = append(days, d)
		}
		sort.Strings(days)
		trend := []map[string]any{}
		for _, d := range days {
			x := day[d]
			trend = append(trend, map[string]any{"label": d, "present": x.Present, "absent": x.Absent, "late": x.Late, "leave": x.Leave})
		}
		out["trend"], out["status_mix"], out["exceptions"] = trend, status, exceptions
		out["present"], out["absent"], out["late"], out["on_leave"] = todayPresent, todayAbsent, todayLate, todayLeave
		// shift coverage today
		shifts := a.labelMap("shifts")
		cov := map[string][2]int{}
		for _, e := range emps {
			if e.Status != "active" || e.I("shift") == 0 {
				continue
			}
			x := cov[shifts[e.I("shift")]]
			x[0]++
			if checked[e.ID] {
				x[1]++
			}
			cov[shifts[e.I("shift")]] = x
		}
		covList := []map[string]any{}
		for k, v := range cov {
			pct := 0
			if v[0] > 0 {
				pct = v[1] * 100 / v[0]
			}
			covList = append(covList, map[string]any{"shift": k, "scheduled": v[0], "covered": v[1], "pct": pct})
		}
		sort.Slice(covList, func(i, j int) bool { return toStr(covList[i]["shift"]) < toStr(covList[j]["shift"]) })
		out["coverage"] = covList
		// leave
		var pendingLeave int
		upcoming := []map[string]any{}
		types := a.labelMap("leave_types")
		typeDist := map[string]float64{}
		for _, l := range recs(a.db.rdb, "leave_requests", b) {
			if !ids[l.I("employee")] {
				continue
			}
			if l.Status == "pending" {
				pendingLeave++
			}
			if l.Status == "approved" {
				if l.S("from") >= td && len(upcoming) < 6 {
					upcoming = append(upcoming, map[string]any{"employee": names[l.I("employee")], "type": types[l.I("leave_type")], "from": l.S("from"), "to": l.S("to"), "days": l.N("days")})
				}
				if l.S("to") >= from && l.S("from") <= to {
					typeDist[types[l.I("leave_type")]] += l.N("days")
				}
			}
		}
		out["pending_leave"], out["upcoming_leave"], out["leave_types"] = pendingLeave, upcoming, typeDist
		var otHours float64
		var otPending int
		for _, o := range recs(a.db.rdb, "overtime", b) {
			if !ids[o.I("employee")] {
				continue
			}
			if o.Status == "approved" && o.S("date") >= from && o.S("date") <= to {
				otHours += o.N("hours")
			}
			if o.Status == "pending" {
				otPending++
			}
		}
		out["overtime_hours"], out["overtime_pending"] = otHours, otPending
		deptCount := map[string]int{}
		dn := a.labelMap("departments")
		for _, e := range emps {
			if e.Status == "active" {
				deptCount[dn[e.I("department")]]++
			}
		}
		out["by_department"] = deptCount
		return jsonBytes(out), nil
	})
}

// ---------- payroll ----------

func calcPayroll(a *App, tx *sql.Tx, c *Claims, r *Rec, p map[string]any) error {
	period, err := loadRec(tx, r.I("period"))
	if err != nil || period.Res != "payroll_periods" {
		return uerr("choose a payroll period")
	}
	if period.Status == "closed" {
		return uerr("this payroll period is closed")
	}
	from, to := period.S("from"), period.S("to")
	fd, _ := parseDate(from)
	td, _ := parseDate(to)
	periodDays := int(td.Sub(fd).Hours()/24) + 1
	divisor := int(a.settingNum("hr", "days_in_month", 30))
	if divisor <= 0 {
		divisor = periodDays
	}
	workHours := a.settingNum("hr", "work_hours", 8)
	otRate := a.settingNum("hr", "overtime_rate", 1.5)
	deductUnpaid := a.settingBool("hr", "unpaid_leave_deduct", true)

	comps := map[int64]*Rec{}
	for _, x := range recs(tx, "salary_components", 0) {
		comps[x.ID] = x
	}
	structs := map[int64]*Rec{}
	for _, x := range recs(tx, "salary_structures", 0) {
		structs[x.ID] = x
	}
	pick := func(res string, id int64) *Rec {
		list := recs(tx, res, 0)
		for _, x := range list {
			if id > 0 && x.ID == id {
				return x
			}
		}
		for _, x := range list {
			if id == 0 && x.B("default") && x.Status == "active" {
				return x
			}
		}
		return nil
	}
	tax, soc := pick("tax_policies", r.I("tax_policy")), pick("social_policies", r.I("social_policy"))

	tx.Exec(`DELETE FROM records WHERE res='payslips' AND json_extract(data,'$.run')=?`, r.ID)
	var nEmp int64
	var tGross, tDed, tNet, tCost int64
	for _, e := range recs(tx, "employees", r.Branch) {
		if (e.Status != "active" && e.Status != "on_leave") || e.S("join_date") > to || (e.S("leave_date") != "" && e.S("leave_date") < from) {
			continue
		}
		basic := a.employeeBasic(tx, e)
		if basic == 0 {
			continue
		}
		// pro-rate for people who joined / left during the period
		frac := 1.0
		start, end := from, to
		if e.S("join_date") > start {
			start = e.S("join_date")
		}
		if e.S("leave_date") != "" && e.S("leave_date") < end {
			end = e.S("leave_date")
		}
		if s, ok1 := parseDate(start); ok1 {
			if en, ok2 := parseDate(end); ok2 && (start != from || end != to) {
				frac = (en.Sub(s).Hours()/24 + 1) / float64(periodDays)
			}
		}
		basicPaid := int64(float64(basic) * frac)
		type pl struct {
			Name   string  `json:"name"`
			Kind   string  `json:"kind"`
			Amount float64 `json:"amount"`
		}
		var lines []any
		var earnings, taxableEarn, otherDed, erContrib int64
		if st := structs[e.I("salary_structure")]; st != nil {
			for _, l := range st.L("lines") {
				cp := comps[toInt(l["component"])]
				if cp == nil || cp.Status != "active" {
					continue
				}
				amt := toInt(l["amount"])
				if amt == 0 {
					amt = cp.I("amount")
				}
				pct := toFloat(l["percent"])
				if pct == 0 {
					pct = cp.N("percent")
				}
				switch cp.S("calc") {
				case "percent_of_basic":
					amt = int64(float64(basicPaid) * pct / 100)
				case "percent_of_gross":
					amt = int64(float64(basicPaid+earnings) * pct / 100)
				default:
					amt = int64(float64(amt) * frac)
				}
				switch cp.S("kind") {
				case "earning":
					earnings += amt
					if cp.B("taxable") {
						taxableEarn += amt
					}
				case "deduction":
					otherDed += amt
				case "employer_contribution":
					erContrib += amt
				}
				lines = append(lines, map[string]any{"name": cp.S("name"), "kind": cp.S("kind"), "amount": float64(amt)})
			}
		}
		gross := basicPaid + earnings
		// attendance: absences and unpaid leave
		var unpaidDays float64
		tx.QueryRow(`SELECT COUNT(*) FROM attendance WHERE employee_id=? AND date>=? AND date<=? AND (status='absent' OR (status='leave' AND note='unpaid'))`, e.ID, from, to).Scan(&unpaidDays)
		var halfDays float64
		tx.QueryRow(`SELECT COUNT(*) FROM attendance WHERE employee_id=? AND date>=? AND date<=? AND status='half_day'`, e.ID, from, to).Scan(&halfDays)
		unpaidDays += halfDays * 0.5
		daily := float64(gross) / float64(divisor)
		var unpaidDed int64
		if deductUnpaid {
			unpaidDed = int64(daily * unpaidDays)
		}
		// overtime
		var otHours float64
		for _, o := range recs(tx, "overtime", 0) {
			if o.Status == "approved" && o.I("employee") == e.ID && o.S("date") >= from && o.S("date") <= to {
				otHours += o.N("hours")
			}
		}
		otPay := int64(float64(gross) / float64(divisor) / workHours * otHours * otRate)
		// social security
		var empSoc, erSoc int64
		if soc != nil {
			base := float64(basicPaid)
			if cl := float64(soc.I("ceiling")); cl > 0 && base > cl {
				base = cl
			}
			empSoc = int64(base * soc.N("employee_pct") / 100)
			erSoc = int64(base * soc.N("employer_pct") / 100)
		}
		// income tax (progressive, monthly)
		var incTax int64
		if tax != nil {
			taxable := float64(basicPaid+taxableEarn+otPay-unpaidDed-empSoc) - float64(tax.I("standard_deduction"))
			lower := 0.0
			for _, br := range tax.L("brackets") {
				upper := toFloat(br["up_to"])
				if upper <= 0 {
					upper = math.MaxFloat64
				}
				if taxable > lower {
					incTax += int64((math.Min(taxable, upper) - lower) * toFloat(br["rate"]) / 100)
				}
				lower = upper
				if taxable <= upper {
					break
				}
			}
		}
		net := gross + otPay - unpaidDed - empSoc - incTax - otherDed
		mk(tx, "payslips", e.Branch, map[string]any{"run": float64(r.ID), "employee": float64(e.ID), "basic": float64(basicPaid), "earnings": float64(earnings), "overtime": float64(otPay),
			"unpaid_deduction": float64(unpaidDed), "social": float64(empSoc), "tax": float64(incTax), "other_deductions": float64(otherDed), "net": float64(net),
			"employer_social": float64(erSoc + erContrib), "unpaid_days": unpaidDays, "overtime_hours": otHours, "lines": lines, "status": "draft"})
		nEmp++
		tGross += gross + otPay - unpaidDed
		tDed += empSoc + incTax + otherDed
		tNet += net
		tCost += gross + otPay - unpaidDed + erSoc + erContrib
	}
	r.Set("employees", float64(nEmp))
	r.Set("gross", float64(tGross))
	r.Set("deductions", float64(tDed))
	r.Set("net", float64(tNet))
	r.Set("employer_cost", float64(tCost))
	r.Status = "calculated"
	bumpLater("payslips")
	return nil
}

func sumPayslips(tx Querier, run int64) (net, taxAmt, soc, other, erSoc int64) {
	tx.QueryRow(`SELECT COALESCE(SUM(json_extract(data,'$.net')),0),COALESCE(SUM(json_extract(data,'$.tax')),0),COALESCE(SUM(json_extract(data,'$.social')),0),
		COALESCE(SUM(json_extract(data,'$.other_deductions')),0),COALESCE(SUM(json_extract(data,'$.employer_social')),0) FROM records WHERE res='payslips' AND json_extract(data,'$.run')=?`, run).
		Scan(&net, &taxAmt, &soc, &other, &erSoc)
	return
}

func postPayroll(a *App, tx *sql.Tx, c *Claims, r *Rec, p map[string]any) error {
	net, taxAmt, soc, other, erSoc := sumPayslips(tx, r.ID)
	period, _ := loadRec(tx, r.I("period"))
	date := today()
	if period != nil {
		if period.S("pay_date") != "" {
			date = period.S("pay_date")
		} else if period.S("to") != "" {
			date = period.S("to")
		}
	}
	r.Status = "posted"
	if !a.autoPost() {
		return nil
	}
	_, err := a.post(tx, r.Branch, date, fmt.Sprintf("payroll:%d", r.ID), "Payroll "+r.S("name"), c.UID,
		Dr("payroll_expense", net+taxAmt+soc+other), Dr("social_expense", erSoc),
		Cr("payroll_payable", net), Cr("wht_payable", taxAmt), Cr("social_payable", soc+erSoc), Cr("accrued", other))
	return err
}

func payPayroll(a *App, tx *sql.Tx, c *Claims, r *Rec, p map[string]any) error {
	net, _, _, _, _ := sumPayslips(tx, r.ID)
	r.Status = "paid"
	tx.Exec(`UPDATE records SET status='paid' WHERE res='payslips' AND json_extract(data,'$.run')=?`, r.ID)
	acct := "bank"
	if toStr(p["method"]) == "cash" {
		acct = "cash"
	}
	bumpLater("payslips")
	return a.postSimple(tx, r.Branch, today(), fmt.Sprintf("payroll:%d", r.ID), "Salaries paid "+r.S("name"), "payroll_payable", acct, net, c.UID)
}

// ---------- payroll analytics ----------

func (a *App) handlePayrollAnalytics(w http.ResponseWriter, r *http.Request) {
	runs := recs(a.db.rdb, "payroll_runs", userFrom(r).Branch)
	sort.Slice(runs, func(i, j int) bool { return runs[i].ID < runs[j].ID })
	periods := a.labelMap("payroll_periods")
	trend := []map[string]any{}
	var gross, net, cost int64
	for _, ru := range runs {
		trend = append(trend, map[string]any{"label": periods[ru.I("period")], "gross": ru.I("gross"), "net": ru.I("net"), "cost": ru.I("employer_cost"), "status": ru.Status})
		if ru.Status != "draft" {
			gross, net, cost = ru.I("gross"), ru.I("net"), ru.I("employer_cost")
		}
	}
	depts := map[string]int64{}
	dn := a.labelMap("departments")
	emps := map[int64]*Rec{}
	for _, e := range recs(a.db.rdb, "employees", 0) {
		emps[e.ID] = e
	}
	var latest int64
	if len(runs) > 0 {
		latest = runs[len(runs)-1].ID
	}
	for _, ps := range recs(a.db.rdb, "payslips", 0) {
		if ps.I("run") == latest {
			if e := emps[ps.I("employee")]; e != nil {
				depts[dn[e.I("department")]] += ps.I("net")
			}
		}
	}
	writeJSON(w, 200, map[string]any{"trend": trend, "latest_gross": gross, "latest_net": net, "latest_cost": cost, "by_department": depts, "runs": len(runs)})
}

var _ = strings.TrimSpace
