package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"sort"
	"strconv"
	"time"
)

func toDay(unix int64) string { return time.Unix(unix, 0).Format("2006-01-02") }

// deliveryFee: pricing rule by order value -> zone fee -> base fee, plus peak surcharge.
func (a *App) deliveryFee(q Querier, branch int64, net int64, zone int64) int64 {
	d := a.settingGroup("delivery")
	fee := toInt(d["base_fee"])
	var zrec *Rec
	if zone > 0 {
		if z, err := loadRec(q, zone); err == nil && z.Res == "delivery_zones" {
			zrec, fee = z, z.I("fee")
		}
	}
	for _, ru := range recs(q, "pricing_rules", 0) {
		if ru.Status != "active" || (ru.I("zone") != 0 && ru.I("zone") != zone) {
			continue
		}
		if net >= ru.I("min_order") && (ru.I("max_order") == 0 || net <= ru.I("max_order")) {
			fee = ru.I("fee")
			break
		}
	}
	if fa := toInt(d["free_above"]); fa > 0 && net >= fa {
		return 0
	}
	now := time.Now().Format("15:04")
	for _, p := range recs(q, "peak_surcharges", 0) {
		if p.Status != "active" {
			continue
		}
		in := p.S("from") <= p.S("to") && now >= p.S("from") && now <= p.S("to") || p.S("from") > p.S("to") && (now >= p.S("from") || now <= p.S("to"))
		if in {
			if p.S("kind") == "percent" {
				fee += int64(float64(fee) * p.N("value") / 100)
			} else {
				fee += int64(p.N("value") * 100)
			}
		}
	}
	_ = zrec
	return fee
}

func (a *App) createDelivery(tx Querier, o *Order, zone int64) {
	slaMin := int64(a.settingNum("delivery", "sla_delivery_min", 45))
	if zone > 0 {
		if z, err := loadRec(tx, zone); err == nil && z.I("eta_min") > 0 {
			slaMin = z.I("eta_min")
		}
	}
	cod := a.settingBool("delivery", "cod_enabled", true) && o.Paid < o.Total
	mk(tx, "deliveries", o.Branch, map[string]any{"order": o.No, "order_id": float64(o.ID), "customer": o.Customer, "phone": o.Phone, "address": o.Address, "zone": float64(zone),
		"method": a.settingStr("delivery", "default_method", "internal"), "fee": float64(o.DeliveryFee), "cod": cod, "cod_amount": float64(o.Total - o.Paid), "cod_status": "none",
		"tracking": randHex(5), "sla_due": float64(nowUnix() + slaMin*60), "status": "pending"})
}

// when the order is completed in POS, close its delivery too
func (a *App) completeDelivery(tx Querier, o *Order) {
	if o.Channel != "delivery" {
		return
	}
	tx.Exec(`UPDATE records SET status='delivered',data=json_set(data,'$.delivered_at',?),updated_at=? WHERE res='deliveries' AND json_extract(data,'$.order_id')=? AND status NOT IN ('delivered','cancelled','failed')`, nowUnix(), nowUnix(), o.ID)
}

func (a *App) cancelDelivery(tx Querier, orderID int64) {
	tx.Exec(`UPDATE records SET status='cancelled',updated_at=? WHERE res='deliveries' AND json_extract(data,'$.order_id')=? AND status NOT IN ('delivered','cancelled')`, nowUnix(), orderID)
}

func (a *App) deliveryOf(tx Querier, id int64, c *Claims) (*Rec, error) {
	d, err := loadRec(tx, id)
	if err != nil || d.Res != "deliveries" || (c.Branch != 0 && d.Branch != c.Branch) {
		return nil, uerr("delivery not found")
	}
	return d, nil
}

// POST /api/delivery/{id}/assign {driver} | {provider}
func (a *App) handleDeliveryAssign(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var in struct{ Driver, Provider int64 }
	if !readJSON(w, r, &in) {
		return
	}
	var out *Rec
	err := a.tx(func(tx *sql.Tx) error {
		d, err := a.deliveryOf(tx, id, c)
		if err != nil {
			return err
		}
		switch d.Status {
		case "delivered", "cancelled":
			return uerr("this delivery is already %s", d.Status)
		}
		if in.Driver == 0 && in.Provider == 0 && a.settingBool("delivery", "auto_assign", false) {
			in.Driver = a.nearestDriver(tx, d)
			if in.Driver == 0 {
				return uerr("no free driver is available right now")
			}
		}
		if in.Driver > 0 { // own rider
			dr, err := loadRec(tx, in.Driver)
			if err != nil || dr.Res != "drivers" {
				return uerr("driver not found")
			}
			if dr.Status == "inactive" || dr.Status == "offline" {
				return uerr("%s is %s", dr.S("name"), dr.Status)
			}
			if old := d.I("driver"); old > 0 && old != dr.ID {
				a.setDriverStatus(tx, old)
			}
			d.Set("driver", float64(dr.ID))
			d.Set("method", "internal")
			d.Set("provider", float64(0))
			dr.Status = "busy"
			saveRec(tx, dr)
		} else if in.Provider > 0 { // outside courier
			pv, err := loadRec(tx, in.Provider)
			if err != nil || pv.Res != "couriers" || pv.Status != "active" {
				return uerr("courier provider not available")
			}
			dist := d.N("distance_km")
			if dist == 0 {
				dist = 5
			}
			fee := pv.I("base_fee")
			if pv.S("fee_model") == "per_km" {
				fee += int64(float64(pv.I("per_km")) * dist)
			}
			fee += int64(float64(d.I("fee")) * pv.N("commission") / 100)
			qid := mk(tx, "courier_quotes", d.Branch, map[string]any{"delivery": float64(d.ID), "provider": float64(pv.ID), "fee": float64(fee), "distance_km": dist, "eta_min": float64(35), "status": "accepted"})
			d.Set("provider", float64(pv.ID))
			d.Set("method", "partner")
			d.Set("driver", float64(0))
			d.Set("driver_pay", float64(fee))
			resp, ok := a.callProvider(pv, d, qid)
			mk(tx, "dispatch_log", d.Branch, map[string]any{"delivery": float64(d.ID), "provider": float64(pv.ID), "request": fmt.Sprintf("dispatch %s to %s", d.S("order"), pv.S("name")), "response": resp, "status": map[bool]string{true: "success", false: "failed"}[ok]})
		} else {
			return uerr("choose a driver or a courier provider")
		}
		d.Set("assigned_at", float64(nowUnix()))
		d.Status = "assigned"
		if err := saveRec(tx, d); err != nil {
			return err
		}
		out = d
		return nil
	})
	if err != nil {
		writeErr(w, 400, friendlyErr(err))
		return
	}
	bumpRes("deliveries")
	a.hub.PublishBranch(out.Branch, "res:deliveries")
	a.audit(r, "deliveries", id, "assign", out.S("order"))
	writeJSON(w, 200, out.JSON())
}

// custom providers get a JSON POST; others just record the request.
func (a *App) callProvider(pv, d *Rec, quote int64) (string, bool) {
	url := pv.S("api_url")
	if url == "" {
		return "recorded (no API URL configured for this provider)", true
	}
	body, _ := json.Marshal(map[string]any{"reference": d.S("order"), "tracking": d.S("tracking"), "customer": d.S("customer"), "phone": d.S("phone"), "address": d.S("address"),
		"lat": d.N("lat"), "lng": d.N("lng"), "cod_amount": d.I("cod_amount"), "quote_id": quote})
	if err := safeOutboundURL(url); err != nil {
		return err.Error(), false
	}
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return err.Error(), false
	}
	req.Header.Set("Content-Type", "application/json")
	if k := pv.S("api_key"); k != "" {
		req.Header.Set("Authorization", "Bearer "+k)
	}
	cl := &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := cl.Do(req)
	if err != nil {
		return err.Error(), false
	}
	defer resp.Body.Close()
	return "HTTP " + resp.Status, resp.StatusCode < 300
}

func (a *App) setDriverStatus(tx Querier, driverID int64) {
	var n int
	tx.QueryRow(`SELECT COUNT(*) FROM records WHERE res='deliveries' AND status IN ('assigned','accepted','picked_up','on_the_way') AND json_extract(data,'$.driver')=?`, driverID).Scan(&n)
	st := "available"
	if n > 0 {
		st = "busy"
	}
	tx.Exec(`UPDATE records SET status=? WHERE id=? AND res='drivers' AND status IN ('available','busy')`, st, driverID)
}

func (a *App) nearestDriver(tx Querier, d *Rec) int64 {
	var best int64
	bestDist := math.MaxFloat64
	for _, dr := range recs(tx, "drivers", d.Branch) {
		if dr.Status != "available" {
			continue
		}
		dist := 0.0
		if d.N("lat") != 0 && dr.N("lat") != 0 {
			dist = haversineM(d.N("lat"), d.N("lng"), dr.N("lat"), dr.N("lng"))
		}
		if dist < bestDist {
			best, bestDist = dr.ID, dist
		}
	}
	return best
}

var deliveryFlow = map[string][]string{
	"pending":    {"cancelled"},
	"assigned":   {"accepted", "picked_up", "failed", "cancelled", "pending"},
	"accepted":   {"picked_up", "failed", "cancelled"},
	"picked_up":  {"on_the_way", "delivered", "failed"},
	"on_the_way": {"delivered", "failed"},
	"failed":     {"pending"},
}

// POST /api/delivery/{id}/status {status, reason}
func (a *App) handleDeliveryStatus(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var in struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	var out *Rec
	err := a.tx(func(tx *sql.Tx) error {
		d, err := a.deliveryOf(tx, id, c)
		if err != nil {
			return err
		}
		ok := false
		for _, s := range deliveryFlow[d.Status] {
			ok = ok || s == in.Status
		}
		if !ok {
			return uerr("a %s delivery cannot become %s", d.Status, in.Status)
		}
		now := nowUnix()
		drv := d.I("driver")
		var order *Order
		if oid := d.I("order_id"); oid > 0 {
			order, _ = loadOrder(tx, oid, false)
		}
		switch in.Status {
		case "picked_up":
			d.Set("picked_at", float64(now))
			if order != nil && (order.Status == "pending" || order.Status == "preparing" || order.Status == "ready") {
				tx.Exec(`UPDATE order_items SET status='served' WHERE order_id=? AND status<>'void'`, order.ID)
				tx.Exec(`UPDATE orders SET status='served',updated_at=? WHERE id=?`, now, order.ID)
			}
		case "delivered":
			d.Set("delivered_at", float64(now))
			if order != nil {
				if order.PayStatus != "paid" && d.B("cod") && order.Total > order.Paid {
					due := order.Total - order.Paid
					tx.Exec(`INSERT INTO payments(order_id,branch_id,session_id,method,kind,amount,ref,created_by,created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
						order.ID, order.Branch, 0, "cod", "payment", due, "driver "+strconv.FormatInt(drv, 10), c.UID, now)
					tx.Exec(`UPDATE orders SET paid=paid+? WHERE id=?`, due, order.ID)
					d.Set("cod_status", "collected")
					d.Set("cod_amount", float64(due))
					ord, err := a.recalc(tx, order.ID, c)
					if err != nil {
						return err
					}
					if err := a.onFullyPaid(tx, c, ord, nil); err != nil {
						return err
					}
					order = ord
				}
				if order.Status != "completed" && order.Status != "cancelled" {
					st := "served"
					if order.PayStatus == "paid" {
						st = "completed"
					}
					tx.Exec(`UPDATE orders SET status=?,updated_at=? WHERE id=?`, st, now, order.ID)
					tx.Exec(`UPDATE order_items SET status='served' WHERE order_id=? AND status<>'void'`, order.ID)
				}
			}
			if drv > 0 {
				if err := a.driverEarned(tx, c, d, drv); err != nil {
					return err
				}
			}
		case "failed":
			d.Set("fail_reason", trunc(in.Reason, 200))
		case "pending":
			d.Set("driver", float64(0))
			d.Set("provider", float64(0))
		}
		d.Status = in.Status
		if err := saveRec(tx, d); err != nil {
			return err
		}
		if drv > 0 {
			a.setDriverStatus(tx, drv)
		}
		out = d
		return nil
	})
	if err != nil {
		writeErr(w, 400, friendlyErr(err))
		return
	}
	bumpRes("deliveries")
	a.hub.PublishBranch(out.Branch, "res:deliveries")
	a.hub.PublishBranch(out.Branch, "order")
	a.audit(r, "deliveries", id, in.Status, out.S("order"))
	if in.Status == "failed" && a.settingBool("notifications", "delivery_delay", true) {
		a.notify(RoleManager, out.Branch, "warn", "Delivery failed: "+out.S("order"), in.Reason, "#/deliveries")
	}
	writeJSON(w, 200, out.JSON())
}

// driverEarned pays the driver according to the first active earning rule.
func (a *App) driverEarned(tx *sql.Tx, c *Claims, d *Rec, drv int64) error {
	var rule *Rec
	for _, x := range recs(tx, "driver_rules", 0) {
		if x.Status == "active" {
			rule = x
			break
		}
	}
	amt := int64(0)
	if rule != nil {
		switch rule.S("type") {
		case "per_km":
			amt = rule.I("base") + int64(float64(rule.I("per_km"))*d.N("distance_km"))
		case "percent_of_fee":
			amt = rule.I("base") + int64(float64(d.I("fee"))*rule.N("percent")/100)
		default:
			amt = rule.I("base")
		}
		if m := rule.I("min_payout"); amt < m {
			amt = m
		}
	}
	d.Set("driver_pay", float64(amt))
	if amt > 0 {
		mk(tx, "driver_earnings", d.Branch, map[string]any{"driver": float64(drv), "delivery": float64(d.ID), "date": today(), "kind": "delivery", "amount": float64(amt), "status": "pending"})
		if err := a.postSimple(tx, d.Branch, today(), fmt.Sprintf("delivery:%d", d.ID), "Driver pay "+d.S("order"), "driver_expense", "driver_payable", amt, c.UID); err != nil {
			return err
		}
	}
	if rule != nil && rule.I("bonus_after") > 0 {
		var n int64
		start := time.Now().Truncate(24 * time.Hour).Unix()
		tx.QueryRow(`SELECT COUNT(*) FROM records WHERE res='deliveries' AND status='delivered' AND json_extract(data,'$.driver')=? AND json_extract(data,'$.delivered_at')>=?`, drv, start).Scan(&n)
		if n+1 == rule.I("bonus_after") && rule.I("bonus") > 0 {
			mk(tx, "driver_earnings", d.Branch, map[string]any{"driver": float64(drv), "delivery": float64(d.ID), "date": today(), "kind": "bonus", "amount": float64(rule.I("bonus")), "status": "pending"})
			a.postSimple(tx, d.Branch, today(), fmt.Sprintf("bonus:%d", d.ID), "Driver bonus", "driver_expense", "driver_payable", rule.I("bonus"), c.UID)
		}
	}
	return nil
}

// POST /api/delivery/location {driver, lat, lng}
func (a *App) handleDriverLocation(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Driver int64   `json:"driver"`
		Lat    float64 `json:"lat"`
		Lng    float64 `json:"lng"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if c := userFrom(r); c.Branch != 0 {
		var b int64
		if a.db.rdb.QueryRow(`SELECT branch_id FROM records WHERE id=? AND res='drivers'`, in.Driver).Scan(&b) != nil || (b != 0 && b != c.Branch) {
			writeErr(w, 404, "driver not found")
			return
		}
	}
	a.db.wdb.Exec(`UPDATE records SET data=json_set(data,'$.lat',?,'$.lng',?,'$.last_seen',?),updated_at=? WHERE id=? AND res='drivers'`,
		in.Lat, in.Lng, time.Now().Format("2006-01-02T15:04"), nowUnix(), in.Driver)
	bumpRes("drivers")
	a.hub.Publish("tracking")
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// GET /api/delivery/live: drivers + their active deliveries (for the map)
func (a *App) handleDeliveryLive(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	b := scopeBranch(c, r)
	a.serveCached(w, a.ordersCache, fmt.Sprintf("live|%d|%d|%d", b, verOf("drivers"), verOf("deliveries")), time.Second, func() ([]byte, error) {
		drivers := []map[string]any{}
		for _, d := range recs(a.db.rdb, "drivers", b) {
			if d.Status == "inactive" {
				continue
			}
			drivers = append(drivers, map[string]any{"id": d.ID, "name": d.S("name"), "phone": d.S("phone"), "vehicle": d.S("vehicle"), "status": d.Status, "lat": d.N("lat"), "lng": d.N("lng"), "last_seen": d.S("last_seen")})
		}
		active := []map[string]any{}
		for _, d := range recs(a.db.rdb, "deliveries", b) {
			switch d.Status {
			case "pending", "assigned", "accepted", "picked_up", "on_the_way":
				m := d.JSON()
				m["late"] = d.I("sla_due") > 0 && nowUnix() > d.I("sla_due")
				active = append(active, m)
			}
		}
		return jsonBytes(map[string]any{"drivers": drivers, "deliveries": active}), nil
	})
}

// public customer tracking page data
func (a *App) handleTrack(w http.ResponseWriter, r *http.Request) {
	code := qStr(r, "c")
	if len(code) < 6 {
		writeErr(w, 404, "tracking code not found")
		return
	}
	var id int64
	if a.db.rdb.QueryRow(`SELECT id FROM records WHERE res='deliveries' AND json_extract(data,'$.tracking')=?`, code).Scan(&id) != nil {
		writeErr(w, 404, "tracking code not found")
		return
	}
	d, _ := loadRec(a.db.rdb, id)
	out := map[string]any{"order": d.S("order"), "status": d.Status, "address": d.S("address"), "restaurant": a.settingStr("application", "app_name", a.cfg.Restaurant),
		"assigned_at": d.I("assigned_at"), "picked_at": d.I("picked_at"), "delivered_at": d.I("delivered_at"), "created_at": d.CreatedAt, "eta_by": d.I("sla_due")}
	if dr, err := loadRec(a.db.rdb, d.I("driver")); err == nil && dr.Res == "drivers" && (d.Status == "picked_up" || d.Status == "on_the_way") {
		out["driver"] = map[string]any{"name": dr.S("name"), "phone": dr.S("phone"), "lat": dr.N("lat"), "lng": dr.N("lng")}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, out)
}

// ---------- dashboard & analytics ----------

func (a *App) handleDeliveryDash(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	b := scopeBranch(c, r)
	to, from := qInt(r, "to"), qInt(r, "from")
	if to == 0 {
		to = time.Now().Unix() + 1
	}
	if from == 0 || from >= to {
		from = time.Now().Truncate(24 * time.Hour).Unix()
	}
	method := qStr(r, "method")
	a.serveCached(w, a.dashCache, fmt.Sprintf("deliv|%d|%d|%d|%s|%d", b, from/60, to/60, method, verOf("deliveries")), 5*time.Second, func() ([]byte, error) {
		var total, delivered, failed, cancelled, active, codOpen int64
		var pickSum, delSum float64
		var pickN, delN int
		funnel := map[string]int64{}
		methods := map[string]int64{}
		codStatus := map[string]int64{}
		days := map[string]int64{}
		var onTrack, delayed, monitored int64
		now := nowUnix()
		for _, d := range recs(a.db.rdb, "deliveries", b) {
			if method != "" && d.S("method") != method {
				continue
			}
			isActive := false
			switch d.Status {
			case "pending", "assigned", "accepted", "picked_up", "on_the_way":
				isActive = true
				active++
				monitored++
				if d.I("sla_due") > 0 && now > d.I("sla_due") {
					delayed++
				} else {
					onTrack++
				}
			}
			if d.CreatedAt < from || d.CreatedAt >= to {
				continue
			}
			total++
			funnel[d.Status]++
			if d.I("assigned_at") > 0 {
				funnel["_assigned"]++
			}
			if d.I("picked_at") > 0 {
				funnel["_picked"]++
			}
			methods[d.S("method")]++
			days[time.Unix(d.CreatedAt, 0).Format("2006-01-02")]++
			switch d.Status {
			case "delivered":
				delivered++
				if d.I("assigned_at") > 0 && d.I("picked_at") > 0 {
					pickSum += float64(d.I("picked_at")-d.I("assigned_at")) / 60
					pickN++
				}
				if d.I("picked_at") > 0 && d.I("delivered_at") > 0 {
					delSum += float64(d.I("delivered_at")-d.I("picked_at")) / 60
					delN++
				}
			case "failed":
				failed++
			case "cancelled":
				cancelled++
			}
			if d.B("cod") {
				codStatus[d.S("cod_status")]++
				if d.S("cod_status") == "declared" || d.S("cod_status") == "disputed" || d.S("cod_status") == "collected" {
					codOpen++
				}
			}
			_ = isActive
		}
		avg := func(s float64, n int) float64 {
			if n == 0 {
				return 0
			}
			return math.Round(s/float64(n)*10) / 10
		}
		dl := []string{}
		for d := range days {
			dl = append(dl, d)
		}
		sort.Strings(dl)
		trend := []map[string]any{}
		for _, d := range dl {
			trend = append(trend, map[string]any{"label": d, "deliveries": days[d]})
		}
		return jsonBytes(map[string]any{"total": total, "active": active, "delivered": delivered, "unresolved_cod": codOpen, "trend": trend,
			"outcomes": map[string]int64{"delivered": delivered, "failed": failed, "cancelled": cancelled}, "funnel": funnel,
			"avg_pickup_min": avg(pickSum, pickN), "avg_delivery_min": avg(delSum, delN), "monitored": monitored, "on_track": onTrack, "delayed": delayed,
			"methods": methods, "cod_status": codStatus}), nil
	})
}

func (a *App) handleDeliveryFinance(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	b := scopeBranch(c, r)
	from, to := qInt(r, "from"), qInt(r, "to")
	if to == 0 {
		to = time.Now().Unix() + 1
	}
	if from == 0 {
		from = to - 30*86400
	}
	var fees, pay, codCollected, codSettled, codOutstanding int64
	zones := a.labelMap("delivery_zones")
	byZone := map[string]int64{}
	byDriver := map[string]int64{}
	names := a.labelMap("drivers")
	var n int64
	for _, d := range recs(a.db.rdb, "deliveries", b) {
		if d.CreatedAt < from || d.CreatedAt >= to {
			continue
		}
		if d.Status == "delivered" {
			n++
			fees += d.I("fee")
			pay += d.I("driver_pay")
			byZone[zones[d.I("zone")]] += d.I("fee")
			byDriver[names[d.I("driver")]] += d.I("driver_pay")
		}
		if d.B("cod") {
			switch d.S("cod_status") {
			case "collected", "declared":
				codCollected += d.I("cod_amount")
				codOutstanding += d.I("cod_amount")
			case "settled":
				codSettled += d.I("cod_amount")
			}
		}
	}
	var pending, paid int64
	for _, e := range recs(a.db.rdb, "driver_earnings", b) {
		if e.Status == "paid" {
			paid += e.I("amount")
		} else {
			pending += e.I("amount")
		}
	}
	writeJSON(w, 200, map[string]any{"deliveries": n, "fee_income": fees, "driver_cost": pay, "margin": fees - pay, "cod_collected": codCollected, "cod_settled": codSettled,
		"cod_outstanding": codOutstanding, "earnings_pending": pending, "earnings_paid": paid, "by_zone": byZone, "by_driver": byDriver})
}

// safeOutboundURL stops a courier URL from pointing at the server itself or the
// local network (server-side request forgery). Set ALLOW_PRIVATE_URLS=1 to allow it.
func safeOutboundURL(raw string) error {
	u, err := neturl.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return fmt.Errorf("the courier URL must start with http:// or https://")
	}
	if os.Getenv("ALLOW_PRIVATE_URLS") == "1" {
		return nil
	}
	ips, err := net.LookupIP(u.Hostname())
	if err != nil {
		return fmt.Errorf("cannot resolve %s", u.Hostname())
	}
	for _, ip := range ips {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsLinkLocalMulticast() {
			return fmt.Errorf("the courier URL points to a private address, which is not allowed")
		}
	}
	return nil
}
