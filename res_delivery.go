package main

import (
	"database/sql"
	"fmt"
)

func init() {
	def("drivers", "delivery", "Drivers",
		`name*^~; phone:tel*^~; vehicle:select(bike|scooter|car|van|bicycle)=bike^; plate:text; license:text; type:select(employee|freelance|partner)=employee^; employee:ref(employees);
		payout:select(per_delivery|salary|commission)=per_delivery; rating:number=5; lat:number.; lng:number.; last_seen:datetime.; status:select(available|busy|offline|inactive)^=offline`,
		Scoped, Hint("Your delivery riders. Their status changes as they get and finish deliveries."))
	def("driver_rules", "delivery", "Driver earning rules",
		`name*^~; type:select(per_delivery|per_km|percent_of_fee)=per_delivery^; base|Base per delivery:money=0^; per_km|Per km:money=0; percent|% of delivery fee:number=0; min_payout|Minimum per delivery:money=0;
		bonus_after|Bonus after N deliveries a day:int=0; bonus:money=0; status:select(active|inactive)^=active`,
		Hint("How much a driver earns per delivery. The first active rule is used."))
	def("delivery_zones", "delivery", "Delivery zones", `name*^~; fee:money*^; min_order:money=0^; eta_min|Estimated time (min):int=30^; radius_km|Radius (km):number=5; areas:textarea; status:select(active|inactive)^=active`, Scoped,
		Hint("Areas you deliver to, each with its own fee and minimum order."))
	def("pricing_rules", "delivery", "Pricing rules", `name*^~; zone:ref(delivery_zones); min_order:money=0; max_order|Max order (0 = no limit):money=0; fee:money*^; status:select(active|inactive)^=active`,
		Hint("Fee by order value: e.g. orders above 500 pay a lower fee. The first matching rule wins; otherwise the zone / base fee applies."))
	def("peak_surcharges", "delivery", "Peak surcharges", `name*^~; from:time*^; to:time*^; kind:select(fixed|percent)=fixed; value:number*^; status:select(active|inactive)^=active`,
		Hint("Extra delivery charge during busy hours or bad weather."))
	def("couriers", "delivery", "Courier providers",
		`name*^~; code#^; type:select(custom|uber_direct|dunzo|porter|shadowfax|other)=custom; api_url:url; api_key:password; fee_model:select(flat|per_km)=flat; base_fee:money=0; per_km:money=0; commission|Commission %:number=0; status:select(active|inactive)^=active`,
		Hint("Outside courier companies for when your own riders are busy. A 'custom' provider receives a JSON request at its URL."))
	def("courier_quotes", "delivery", "Quote history", `delivery:ref(deliveries)^; provider:ref(couriers)^; fee:money^; distance_km:number; eta_min:int; status:select(quoted|accepted|expired)^=quoted`, NoCreate, Label("Quote #{id}"))
	def("dispatch_log", "delivery", "Dispatch request log", `delivery:ref(deliveries)^; provider:ref(couriers)^; request:textarea; response:textarea; status:select(success|failed)^=success`, NoCreate, NoEdit, Label("Dispatch #{id}"))
	def("deliveries", "delivery", "Deliveries",
		`order|Order no.:text^~; customer:text^~; phone:tel~; address:textarea^; lat:number; lng:number; zone:ref(delivery_zones); method:select(internal|partner)=internal^; driver:ref(drivers)^; provider:ref(couriers);
		fee:money^; driver_pay:money.; distance_km:number=0; cod:bool^; cod_amount:money=0; cod_status:select(none|collected|declared|settled|disputed)=none; order_id:int.; tracking:text.; sla_due:int.;
		assigned_at:int.; picked_at:int.; delivered_at:int.; fail_reason:text.; note; status:select(pending|assigned|accepted|picked_up|on_the_way|delivered|failed|cancelled)^=pending`,
		Scoped, Label("{order}"), Hint("Every delivery order. Dispatch it, follow it, and settle the cash."))

	// ---------------- money side ----------------
	def("driver_earnings", "delivery", "Driver earnings", `driver:ref(drivers)*^; delivery:ref(deliveries); date:date*^; kind:select(delivery|bonus|tip|adjustment)=delivery^; amount:money*^; batch:ref(payout_batches); status:select(pending|batched|paid)^=pending`,
		Scoped, Label("Earning #{id}"),
		AfterSave(func(a *App, tx *sql.Tx, c *Claims, r *Rec, old *Rec) error {
			if old == nil && r.S("kind") == "adjustment" {
				return a.postSimple(tx, r.Branch, r.S("date"), fmt.Sprintf("earning:%d", r.ID), "Driver adjustment", "driver_expense", "driver_payable", r.I("amount"), c.UID)
			}
			return nil
		}))
	def("payout_batches", "delivery", "Payout batches", `name*^~; from:date*^; to:date*^; drivers:int.^; amount:money.^; status:select(draft|approved|paid)^=draft; note`, Scoped,
		Hint("Pay several drivers at once. Creating a batch collects every pending earning in the date range."),
		AfterSave(func(a *App, tx *sql.Tx, c *Claims, r *Rec, old *Rec) error {
			if old != nil {
				return nil
			}
			var total int64
			drivers := map[int64]bool{}
			for _, e := range recs(tx, "driver_earnings", r.Branch) {
				if e.Status == "pending" && e.S("date") >= r.S("from") && e.S("date") <= r.S("to") {
					e.Status = "batched"
					e.Set("batch", float64(r.ID))
					saveRec(tx, e)
					total += e.I("amount")
					drivers[e.I("driver")] = true
				}
			}
			r.Set("amount", float64(total))
			r.Set("drivers", float64(len(drivers)))
			return saveRec(tx, r)
		}),
		ActFn("approve", "Approve", LvApprove, S("draft"), "ok", func(a *App, tx *sql.Tx, c *Claims, r *Rec, p map[string]any) error {
			r.Status = "approved"
			return nil
		}),
		ActFn("pay", "Mark paid", LvApprove, S("approved"), "ok", func(a *App, tx *sql.Tx, c *Claims, r *Rec, p map[string]any) error {
			r.Status = "paid"
			for _, e := range recs(tx, "driver_earnings", 0) {
				if e.I("batch") == r.ID && e.Status == "batched" {
					e.Status = "paid"
					saveRec(tx, e)
				}
			}
			return a.postSimple(tx, r.Branch, today(), fmt.Sprintf("payout:%d", r.ID), "Driver payout "+r.S("name"), "driver_payable", "cash", r.I("amount"), c.UID)
		}), Confirm("pay", "Pay all drivers in this batch?"))
	def("payout_issues", "delivery", "Payout issues", `driver:ref(drivers)*^; batch:ref(payout_batches); issue:text*^~; amount:money=0; status:select(open|resolved)^=open; resolution:text`, Scoped, Label("Issue #{id}"),
		Act("resolve", "Resolve", LvManage, S("open"), "resolved", "ok"))
	def("cod_settlements", "delivery", "COD settlements", `driver:ref(drivers)*^; from:date; to:date; deliveries:int.^; amount:money^; status:select(pending|settled|disputed)^=pending; settled_at:date.; note`, Scoped,
		Hint("Cash on delivery: the money a driver collected and must hand over. Leave the amount empty to total all unsettled COD deliveries."),
		BeforeSave(func(a *App, tx *sql.Tx, c *Claims, r *Rec, old *Rec) error {
			if old == nil {
				var total, n int64
				for _, d := range recs(tx, "deliveries", r.Branch) {
					if d.I("driver") == r.I("driver") && d.S("cod_status") == "collected" && (r.S("from") == "" || toDay(d.CreatedAt) >= r.S("from")) && (r.S("to") == "" || toDay(d.CreatedAt) <= r.S("to")) {
						total += d.I("cod_amount")
						n++
					}
				}
				if r.I("amount") == 0 {
					r.Set("amount", float64(total))
				}
				r.Set("deliveries", float64(n))
			}
			return nil
		}),
		ActFn("settle", "Cash received", LvApprove, S("pending", "disputed"), "ok", func(a *App, tx *sql.Tx, c *Claims, r *Rec, p map[string]any) error {
			r.Status = "settled"
			r.Set("settled_at", today())
			for _, d := range recs(tx, "deliveries", r.Branch) {
				if d.I("driver") == r.I("driver") && d.S("cod_status") == "collected" {
					d.Set("cod_status", "settled")
					saveRec(tx, d)
				}
			}
			return a.postSimple(tx, r.Branch, today(), fmt.Sprintf("cod:%d", r.ID), "COD cash from driver", "cash", "ar", r.I("amount"), c.UID)
		}),
		Act("dispute", "Dispute", LvManage, S("pending"), "disputed", "bad"))
	def("provider_settlements", "delivery", "Provider settlements", `provider:ref(couriers)*^; from:date*^; to:date*^; deliveries:int.^; amount:money*^; status:select(pending|paid)^=pending; note`, Scoped,
		Hint("What you owe an outside courier for the deliveries they completed."),
		BeforeSave(func(a *App, tx *sql.Tx, c *Claims, r *Rec, old *Rec) error {
			if old == nil {
				var n int64
				var total int64
				for _, d := range recs(tx, "deliveries", r.Branch) {
					if d.I("provider") == r.I("provider") && d.Status == "delivered" && toDay(d.CreatedAt) >= r.S("from") && toDay(d.CreatedAt) <= r.S("to") {
						n++
						total += d.I("driver_pay")
					}
				}
				r.Set("deliveries", float64(n))
				if r.I("amount") == 0 {
					r.Set("amount", float64(total))
				}
			}
			return nil
		}),
		ActFn("pay", "Mark paid", LvApprove, S("pending"), "ok", func(a *App, tx *sql.Tx, c *Claims, r *Rec, p map[string]any) error {
			r.Status = "paid"
			return a.postSimple(tx, r.Branch, today(), fmt.Sprintf("provider:%d", r.ID), "Courier provider payment", "driver_expense", "bank", r.I("amount"), c.UID)
		}))
}
