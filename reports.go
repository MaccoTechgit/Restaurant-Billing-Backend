package main

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
)

// =====================================================================
//  Reports: one generic endpoint returns {columns, rows, totals}; the
//  browser draws the table and can export it to CSV.
// =====================================================================

type Col struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Type  string `json:"type"` // text int money pct num date
}

func cols(spec ...string) []Col {
	out := make([]Col, 0, len(spec))
	for _, s := range spec {
		p := strings.Split(s, ":")
		c := Col{Key: p[0], Label: niceLabel(p[0]), Type: "text"}
		if len(p) > 1 {
			c.Label = p[1]
		}
		if len(p) > 2 {
			c.Type = p[2]
		}
		out = append(out, c)
	}
	return out
}

func (a *App) handleReport(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	kind := r.PathValue("kind")
	b := scopeBranch(c, r)
	from, to := qInt(r, "from"), qInt(r, "to")
	tz := int(qInt(r, "tz"))
	if to == 0 {
		to = time.Now().Unix() + 1
	}
	if from == 0 {
		from = to - 30*86400
	}
	group := qStr(r, "group")
	key := fmt.Sprintf("rep|%s|%d|%d|%d|%s|%d", kind, b, from/60, to/60, group, tz)
	a.serveCached(w, a.dashCache, key, 5*time.Second, func() ([]byte, error) {
		out, ok := a.buildReport(kind, b, from, to, tz, group)
		if !ok {
			return nil, errCacheLoad
		}
		return jsonBytes(out), nil
	})
}

func (a *App) buildReport(kind string, b, from, to int64, tz int, group string) (map[string]any, bool) {
	bw, bargs := "", []any{}
	ow := "" // for queries aliased as o
	if b != 0 {
		bw, ow = " AND branch_id=?", " AND o.branch_id=?"
		bargs = []any{b}
	}
	base := []any{from, to}
	paid := "pay_status='paid' AND status NOT IN ('cancelled','refunded','held')"
	opaid := "o.pay_status='paid' AND o.status NOT IN ('cancelled','refunded','held')"
	var cs []Col
	rows := []map[string]any{}
	title := ""
	switch kind {
	case "sales":
		title = "Sales report"
		f := map[string]string{"day": "%Y-%m-%d", "week": "%Y-W%W", "month": "%Y-%m"}[group]
		if f == "" {
			f = "%Y-%m-%d"
		}
		cs = cols("period:Period", "orders:Total orders:int", "products:Total products:int", "subtotal:Subtotal:money", "discount:Discount:money", "tax:Tax:money", "service:Service:money", "tips:Tips:money", "total:Total:money")
		rows = a.queryMaps(`SELECT strftime('`+f+`',o.created_at+?,'unixepoch') period,COUNT(*) orders,COALESCE(SUM((SELECT SUM(qty) FROM order_items WHERE order_id=o.id AND status<>'void')),0) products,
			SUM(o.subtotal) subtotal,SUM(o.discount) discount,SUM(o.tax) tax,SUM(o.service) service,SUM(o.tip) tips,SUM(o.total) total FROM orders o
			WHERE `+opaid+` AND o.created_at>=? AND o.created_at<?`+ow+` GROUP BY period ORDER BY period DESC`, append([]any{tz * 60, from, to}, bargs...)...)
	case "products":
		title = "Product report"
		cs = cols("name:Product", "category:Category", "qty:Qty sold:int", "revenue:Revenue:money", "cost:Cost:money", "margin:Margin:money", "margin_pct:Margin %:pct")
		rows = a.queryMaps(`SELECT i.name,i.category,SUM(i.qty) qty,SUM(i.qty*i.price) revenue,SUM(i.qty*i.cost) cost,SUM(i.qty*(i.price-i.cost)) margin,
			ROUND(100.0*SUM(i.qty*(i.price-i.cost))/NULLIF(SUM(i.qty*i.price),0),1) margin_pct FROM order_items i JOIN orders o ON o.id=i.order_id
			WHERE `+opaid+` AND i.status<>'void' AND o.created_at>=? AND o.created_at<?`+ow+` GROUP BY i.product_id ORDER BY revenue DESC`, append(base, bargs...)...)
	case "categories":
		title = "Category report"
		cs = cols("category:Category", "qty:Qty sold:int", "revenue:Revenue:money", "share:Share %:pct")
		rows = a.queryMaps(`SELECT i.category,SUM(i.qty) qty,SUM(i.qty*i.price) revenue FROM order_items i JOIN orders o ON o.id=i.order_id
			WHERE `+opaid+` AND i.status<>'void' AND o.created_at>=? AND o.created_at<?`+ow+` GROUP BY i.category ORDER BY revenue DESC`, append(base, bargs...)...)
		var tot int64
		for _, m := range rows {
			tot += toInt(m["revenue"])
		}
		for _, m := range rows {
			if tot > 0 {
				m["share"] = math.Round(float64(toInt(m["revenue"]))/float64(tot)*1000) / 10
			}
		}
	case "payments":
		title = "Payment methods"
		cs = cols("method:Method", "payments:Payments:int", "amount:Received:money", "refunds:Refunded:money", "net:Net:money")
		pw := strings.ReplaceAll(bw, "branch_id", "p.branch_id")
		rows = a.queryMaps(`SELECT p.method,SUM(p.kind='payment') payments,SUM(CASE WHEN p.kind='payment' THEN p.amount ELSE 0 END) amount,SUM(CASE WHEN p.kind='refund' THEN p.amount ELSE 0 END) refunds,
			SUM(CASE WHEN p.kind='payment' THEN p.amount ELSE -p.amount END) net FROM payments p WHERE p.created_at>=? AND p.created_at<?`+pw+` GROUP BY p.method ORDER BY net DESC`, append(base, bargs...)...)
	case "tax":
		title = "Tax report"
		cs = cols("rate:Tax rate %:num", "taxable:Taxable sales:money", "tax:Tax collected:money")
		rows = a.queryMaps(`SELECT i.tax_pct rate,SUM(i.qty*i.price) taxable,CAST(ROUND(SUM(i.qty*i.price*i.tax_pct/100.0)) AS INTEGER) tax FROM order_items i JOIN orders o ON o.id=i.order_id
			WHERE `+opaid+` AND i.status<>'void' AND o.created_at>=? AND o.created_at<?`+ow+` GROUP BY i.tax_pct ORDER BY rate`, append(base, bargs...)...)
	case "staff":
		title = "Staff performance"
		cs = cols("name:Staff", "orders:Orders:int", "sales:Sales:money", "avg:Average bill:money", "tips:Tips:money", "discounts:Discounts given:money", "voids:Cancelled:int")
		rows = a.queryMaps(`SELECT COALESCE(u.name,'?') name,SUM(CASE WHEN `+opaid+` THEN 1 ELSE 0 END) orders,SUM(CASE WHEN `+opaid+` THEN o.total ELSE 0 END) sales,
			CAST(AVG(CASE WHEN `+opaid+` THEN o.total END) AS INTEGER) avg,SUM(CASE WHEN `+opaid+` THEN o.tip ELSE 0 END) tips,SUM(CASE WHEN `+opaid+` THEN o.discount ELSE 0 END) discounts,
			SUM(CASE WHEN o.status='cancelled' THEN 1 ELSE 0 END) voids FROM orders o LEFT JOIN users u ON u.id=o.created_by WHERE o.created_at>=? AND o.created_at<?`+ow+` GROUP BY o.created_by ORDER BY sales DESC`, append(base, bargs...)...)
	case "branches":
		title = "Branch comparison"
		cs = cols("branch:Branch", "orders:Orders:int", "sales:Sales:money", "avg:Average bill:money", "tax:Tax:money", "discounts:Discounts:money")
		rows = a.queryMaps(`SELECT branch_id,COUNT(*) orders,SUM(total) sales,CAST(AVG(total) AS INTEGER) avg,SUM(tax) tax,SUM(discount) discounts FROM orders WHERE `+paid+` AND created_at>=? AND created_at<? GROUP BY branch_id ORDER BY sales DESC`, base...)
		names := a.labelMap("branches")
		for _, m := range rows {
			m["branch"] = names[toInt(m["branch_id"])]
		}
	case "hours":
		title = "Hourly sales"
		cs = cols("hour:Hour", "orders:Orders:int", "sales:Sales:money", "avg:Average bill:money")
		rows = a.queryMaps(`SELECT printf('%02d:00',CAST(strftime('%H',o.created_at+?,'unixepoch') AS INTEGER)) hour,COUNT(*) orders,SUM(o.total) sales,CAST(AVG(o.total) AS INTEGER) avg FROM orders o
			WHERE `+opaid+` AND o.created_at>=? AND o.created_at<?`+ow+` GROUP BY hour ORDER BY hour`, append([]any{tz * 60, from, to}, bargs...)...)
	case "voids":
		title = "Cancellations & refunds"
		cs = cols("no:Order", "status:Status", "total:Amount:money", "reason:Reason", "staff:Staff", "date:Date")
		rows = a.queryMaps(`SELECT o.no,o.status,o.total,o.reason,COALESCE(u.name,'') staff,datetime(o.updated_at+?,'unixepoch') date FROM orders o LEFT JOIN users u ON u.id=o.created_by
			WHERE o.status IN ('cancelled','refunded') AND o.created_at>=? AND o.created_at<?`+ow+` ORDER BY o.id DESC LIMIT 1000`, append([]any{tz * 60, from, to}, bargs...)...)
	case "inventory":
		title = "Inventory report"
		cs = cols("name:Ingredient", "category:Category", "unit:Unit", "qty:On hand:num", "reorder:Reorder level:num", "cost:Unit cost:money", "value:Stock value:money", "used:Used in period:num", "state:State")
		used := map[int64]float64{}
		uq := `SELECT ingredient_id,-SUM(qty) FROM stock_moves WHERE kind='sale' AND created_at>=? AND created_at<?`
		uargs := []any{from, to}
		if b != 0 {
			uq += ` AND branch_id=?`
			uargs = append(uargs, b)
		}
		if rs, err := a.db.rdb.Query(uq+` GROUP BY ingredient_id`, uargs...); err == nil {
			for rs.Next() {
				var id int64
				var q float64
				rs.Scan(&id, &q)
				used[id] = q
			}
			rs.Close()
		}
		for _, s := range a.stockRows(b) {
			st := "ok"
			if s.Low {
				st = "LOW"
			}
			rows = append(rows, map[string]any{"name": s.Name, "category": s.Cat, "unit": s.Unit, "qty": math.Round(s.Qty*100) / 100, "reorder": s.Reorder, "cost": s.Cost, "value": s.Value, "used": math.Round(used[s.ID]*100) / 100, "state": st})
		}
	case "delivery":
		title = "Delivery report"
		cs = cols("driver:Driver / provider", "deliveries:Deliveries:int", "delivered:Delivered:int", "failed:Failed:int", "fees:Delivery fees:money", "pay:Driver pay:money", "cod:COD collected:money")
		names := a.labelMap("drivers")
		prov := a.labelMap("couriers")
		type agg struct{ n, ok, fail, fees, pay, cod int64 }
		m := map[string]*agg{}
		for _, d := range recs(a.db.rdb, "deliveries", b) {
			if d.CreatedAt < from || d.CreatedAt >= to {
				continue
			}
			nm := names[d.I("driver")]
			if d.S("method") == "partner" {
				nm = prov[d.I("provider")] + " (partner)"
			}
			if nm == "" {
				nm = "Unassigned"
			}
			x := m[nm]
			if x == nil {
				x = &agg{}
				m[nm] = x
			}
			x.n++
			switch d.Status {
			case "delivered":
				x.ok++
				x.fees += d.I("fee")
				x.pay += d.I("driver_pay")
				if d.S("cod_status") == "collected" || d.S("cod_status") == "settled" {
					x.cod += d.I("cod_amount")
				}
			case "failed":
				x.fail++
			}
		}
		for k, x := range m {
			rows = append(rows, map[string]any{"driver": k, "deliveries": x.n, "delivered": x.ok, "failed": x.fail, "fees": x.fees, "pay": x.pay, "cod": x.cod})
		}
		sort.Slice(rows, func(i, j int) bool { return toInt(rows[i]["deliveries"]) > toInt(rows[j]["deliveries"]) })
	default:
		return nil, false
	}
	if rows == nil {
		rows = []map[string]any{}
	}
	totals := map[string]any{}
	for _, c := range cs {
		if c.Type == "int" || c.Type == "money" {
			var t int64
			for _, m := range rows {
				t += toInt(m[c.Key])
			}
			totals[c.Key] = t
		}
	}
	return map[string]any{"title": title, "columns": cs, "rows": rows, "totals": totals, "from": from, "to": to}, true
}
