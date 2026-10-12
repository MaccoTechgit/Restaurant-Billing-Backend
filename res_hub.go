package main

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// docNo makes the next document number like PO-000012 (runs inside the write tx).
func docNo(q Querier, res, prefix string) string {
	var cnt int64
	q.QueryRow(`SELECT COUNT(*) FROM records WHERE res=?`, res).Scan(&cnt)
	for {
		cnt++
		no := fmt.Sprintf("%s-%06d", prefix, cnt)
		var n int
		q.QueryRow(`SELECT COUNT(*) FROM records WHERE res=? AND ref=?`, res, no).Scan(&n)
		if n == 0 {
			return no
		}
	}
}

func today() string { return time.Now().Format("2006-01-02") }

func init() {
	// ---------------- Administration ----------------
	// Users live in their own table; this entry only lets other forms pick a person.
	def("users", "admin", "Users", `name*^; username*^; role`, NoCreate, NoEdit, NoDelete, Label("{name}"))
	def("branches", "admin", "Branches",
		`name*^~; code#^; status:select(active|inactive)^=active; phone:tel^; email:email; address:textarea; city; lat:number; lng:number; tax_id; opening_hours:text=09:00-23:00; timezone`,
		Hint("Each branch has its own orders, stock, staff and cash. Users can be tied to one branch."))

	// ---------------- Operations ----------------
	def("reasons", "orders", "Reasons", `name*^~; type:select(cancel|refund|void|discount|waste)*^; status:select(active|inactive)=active`,
		Hint("Reasons staff can pick when cancelling, refunding, voiding or wasting."))
	def("registers", "pos", "Registers", `name*^~; printer; status:select(active|inactive)^=active`, Scoped,
		Hint("A cash register / till. Cashiers open a session on a register to start their shift."))
	def("zones", "floor", "Zones", `name*^~; description`, Scoped)
	def("floors", "floor", "Floors", `name*^~; level:int=1^`, Scoped)
	def("tables", "floor", "Tables",
		`name*^~; zone:ref(zones)^; floor:ref(floors); seats:int*^=4; shape:select(square|round|rect)=square; status:select(available|occupied|cleaning|reserved|blocked|merged)^=available; waiter:ref(users); qr_token:text.`,
		Scoped, Group("Floor"), Hint("Tables guests sit at. Status changes automatically as orders open and close."),
		BeforeSave(func(a *App, tx *sql.Tx, c *Claims, r *Rec, old *Rec) error {
			if old == nil {
				r.Set("qr_token", randHex(6))
			}
			return nil
		}))
	def("table_merges", "floor", "Table merges", `primary:ref(tables)*^; tables:multiref(tables)*; note; status:select(active|split)^=active`, Scoped,
		Hint("Join tables together for a large party. Splitting frees them again."),
		ActFn("split", "Split tables", LvManage, S("active"), "bad", func(a *App, tx *sql.Tx, c *Claims, r *Rec, p map[string]any) error {
			r.Status = "split"
			for _, id := range append(r.IDs("tables"), r.I("primary")) {
				setTableStatus(tx, id, "available", true)
			}
			return nil
		}),
		AfterSave(func(a *App, tx *sql.Tx, c *Claims, r *Rec, old *Rec) error {
			if old == nil {
				for _, id := range r.IDs("tables") {
					if id != r.I("primary") {
						setTableStatus(tx, id, "merged", false)
					}
				}
			}
			return nil
		}))
	def("table_blocks", "floor", "Table blocks", `table:ref(tables)*^; from:datetime*^; to:datetime*^; reason:text^; status:select(active|released)=active`, Scoped,
		Hint("Take a table out of service (maintenance, private event) for a period."),
		ActFn("release", "Release", LvManage, S("active"), "ok", func(a *App, tx *sql.Tx, c *Claims, r *Rec, p map[string]any) error {
			r.Status = "released"
			setTableStatus(tx, r.I("table"), "available", true)
			return nil
		}),
		AfterSave(func(a *App, tx *sql.Tx, c *Claims, r *Rec, old *Rec) error {
			if old == nil {
				setTableStatus(tx, r.I("table"), "blocked", true)
			}
			return nil
		}))
	def("waitlist", "reservations", "Waitlist", `name*^~; phone:tel^~; party:int*^=2; quoted_min|Quoted wait (min):int=15^; note; status:select(waiting|notified|seated|left|cancelled)^=waiting`, Scoped,
		Hint("Walk-in guests waiting for a table."),
		Act("notify", "Notify guest", LvManage, S("waiting"), "notified", "pri"),
		Act("seat", "Seat", LvManage, S("waiting", "notified"), "seated", "ok"),
		Act("left", "Left", LvManage, S("waiting", "notified"), "left", "bad"))
	def("reservations", "reservations", "Reservations",
		`name*^~; phone:tel^~; party:int*^=2; date:date*^; time:time*^; table:ref(tables)^; status:select(pending|confirmed|seated|completed|cancelled|no_show)^=pending; source:select(staff|phone|online|walk_in)=staff; note:textarea`,
		Scoped, Hint("Table bookings. Confirm, seat and complete them as guests arrive."),
		Act("confirm", "Confirm", LvManage, S("pending"), "confirmed", "pri"),
		ActFn("seat", "Seat guests", LvManage, S("pending", "confirmed"), "ok", func(a *App, tx *sql.Tx, c *Claims, r *Rec, p map[string]any) error {
			r.Status = "seated"
			setTableStatus(tx, r.I("table"), "occupied", true)
			return nil
		}),
		Act("complete", "Complete", LvManage, S("seated"), "completed", "ok"),
		Act("no_show", "No-show", LvManage, S("pending", "confirmed"), "no_show", "bad"),
		Act("cancel", "Cancel", LvManage, S("pending", "confirmed"), "cancelled", "bad"),
		BeforeSave(func(a *App, tx *sql.Tx, c *Claims, r *Rec, old *Rec) error {
			if old == nil && r.I("table") > 0 { // same table, same slot -> reject double booking
				var n int
				tx.QueryRow(`SELECT COUNT(*) FROM records WHERE res='reservations' AND status IN ('pending','confirmed','seated')
					AND json_extract(data,'$.table')=? AND json_extract(data,'$.date')=? AND json_extract(data,'$.time')=?`,
					r.I("table"), r.S("date"), r.S("time")).Scan(&n)
				if n > 0 {
					return uerr("that table is already booked at this time")
				}
			}
			return nil
		}))

	// ---------------- Menu & catalog ----------------
	def("categories", "menu", "Categories", `name*^~; emoji:text^; station:ref(stations); sort:int=0^; image:file; status:select(active|inactive)^=active`,
		AfterSave(func(a *App, tx *sql.Tx, c *Claims, r *Rec, old *Rec) error { a.menuDirty(); return nil }))
	def("products", "menu", "Products",
		`name*^~; category:ref(categories)*^; price:money*^; cost:money; tax:ref(taxes)^; sku|SKU:text#^; barcode:text~; emoji:text; image:file; description:textarea;
		options:multiref(options); prep_min|Prep time (min):int=10; track_stock:bool=true; available:bool=true^; featured:bool; status:select(active|inactive)^=active`,
		Hint("Everything you sell. Link a recipe to deduct stock automatically."),
		AfterSave(func(a *App, tx *sql.Tx, c *Claims, r *Rec, old *Rec) error { a.menuDirty(); return nil }),
		BeforeDelete(func(a *App, tx *sql.Tx, c *Claims, r *Rec, _ *Rec) error {
			var n int
			tx.QueryRow(`SELECT COUNT(*) FROM order_items WHERE product_id=?`, r.ID).Scan(&n)
			if n > 0 {
				return uerr("this product has sales history - set it to Inactive instead of deleting")
			}
			a.menuDirty()
			return nil
		}))
	def("options", "menu", "Options & add-ons", `name*^~; kind:select(single|multiple)=single^; required:bool; choices:lines(name*,price:money=0)*; status:select(active|inactive)=active`,
		Hint("Choices attached to products - size, spice level, extra cheese ..."),
		AfterSave(func(a *App, tx *sql.Tx, c *Claims, r *Rec, old *Rec) error { a.menuDirty(); return nil }))
	def("menus", "menu", "Menus", `name*^~; type:select(dine_in|online|both)=both^; categories:multiref(categories); description:text; status:select(active|inactive)^=active`,
		Hint("Group categories into menus, e.g. a lunch menu or an online menu."))
	def("stations", "menu", "Kitchen stations", `name*^~; printer; color:color=#f97316`, Hint("Grill, Bar, Cold kitchen... Categories send their items to a station."),
		AfterSave(func(a *App, tx *sql.Tx, c *Claims, r *Rec, old *Rec) error { a.menuDirty(); return nil }))
	def("recipes", "menu", "Recipes", `product:ref(products)*^#; portions|Portions per batch:number=1; lines:lines(ingredient:ref(ingredients)*,qty:number*)*; waste_pct|Waste %:number=0`,
		Label("Recipe #{id}"), Hint("Ingredients used per portion. Selling the product reduces stock and records food cost."),
		AfterSave(func(a *App, tx *sql.Tx, c *Claims, r *Rec, old *Rec) error { a.menuDirty(); return nil }))

	// ---------------- Inventory ----------------
	def("units", "inventory", "Units", `name*^~; short|Symbol:text*^; type:select(weight|volume|count|other)=count`)
	def("ingredients", "inventory", "Ingredients",
		`name*^~; sku|SKU:text#^; category:text^; unit:ref(units)*^; reorder_level:number=0^; cost|Cost per unit:money=0^; supplier:ref(suppliers); shelf_life_days:int; status:select(active|inactive)^=active`,
		Hint("Raw materials. Stock levels are kept per branch."))
	def("suppliers", "purchasing", "Suppliers", `name*^~; contact:text^; phone:tel^; email:email; address:textarea; tax_id; payment_terms:select(cash|net7|net15|net30|net60)=net30; note; status:select(active|inactive)^=active`)
	def("purchases", "purchasing", "Purchases",
		`no|PO no.:text#^; supplier:ref(suppliers)*^; date:date*^; expected:date; status:select(draft|ordered|received|cancelled)^=draft;
		lines:lines(ingredient:ref(ingredients)*,qty:number*,cost|Unit cost:money*)*; tax:money=0; subtotal:money.; total:money.^; paid:money.=0; note:textarea`,
		Scoped, Label("{no}"), Hint("Purchase orders. Receiving adds stock and posts the supplier bill to accounting."),
		BeforeSave(purchaseBefore),
		Act("order", "Mark ordered", LvManage, S("draft"), "ordered", "pri"),
		ActFn("receive", "Receive goods", LvManage, S("ordered", "draft"), "ok", receivePurchase),
		Act("cancel", "Cancel", LvManage, S("draft", "ordered"), "cancelled", "bad"),
		Confirm("receive", "Receive all items into stock?"))
	def("transfers", "inventory", "Stock transfers",
		`no|No.:text#^; from_branch:ref(branches)*^; to_branch:ref(branches)*^; date:date*^; status:select(draft|in_transit|received|cancelled)^=draft; lines:lines(ingredient:ref(ingredients)*,qty:number*)*; note`,
		Label("{no}"), Hint("Move stock between branches."),
		BeforeSave(func(a *App, tx *sql.Tx, c *Claims, r *Rec, old *Rec) error {
			if r.I("from_branch") == r.I("to_branch") {
				return uerr("choose two different branches")
			}
			if old == nil && r.S("no") == "" {
				r.Set("no", docNo(tx, "transfers", "TR"))
				r.Ref = r.S("no")
			}
			return nil
		}),
		ActFn("send", "Send", LvManage, S("draft"), "pri", sendTransfer),
		ActFn("receive", "Receive", LvManage, S("in_transit"), "ok", receiveTransfer),
		Act("cancel", "Cancel", LvManage, S("draft"), "cancelled", "bad"))
	def("counts", "inventory", "Inventory counts",
		`name*^~; date:date*^; status:select(draft|applied)^=draft; lines:lines(ingredient:ref(ingredients)*,counted:number*)*; note`, Scoped,
		Hint("Count what is physically on the shelf. Applying posts the difference to stock."),
		ActFn("apply", "Apply to stock", LvApprove, S("draft"), "ok", applyCount), Confirm("apply", "Adjust stock to match this count?"))
	def("waste", "inventory", "Waste log", `ingredient:ref(ingredients)*^; qty:number*^; reason:text^; date:date*^; note`, Scoped, Label("Waste #{id}"),
		Hint("Spoiled or thrown away items. Reduces stock and is posted as an expense."),
		AfterSave(func(a *App, tx *sql.Tx, c *Claims, r *Rec, old *Rec) error {
			if old != nil {
				return nil
			}
			cost := a.ingCost(tx, r.I("ingredient"))
			if err := a.stockMove(tx, r.Branch, r.I("ingredient"), -r.N("qty"), cost, "waste", fmt.Sprintf("waste:%d", r.ID), r.S("reason"), c.UID); err != nil {
				return err
			}
			return a.postSimple(tx, r.Branch, r.S("date"), fmt.Sprintf("waste:%d", r.ID), "Stock wasted", "waste_expense", "inventory", int64(float64(cost)*r.N("qty")), c.UID)
		}))

	// ---------------- Customers & growth ----------------
	def("customers", "customers", "Customers",
		`name*^~; phone:tel#^; email:email~; birthday:date; company; tax_id; address:textarea; points:int.^=0; tier:text.^; visits:int.=0; spent:money.^=0; marketing|Accepts marketing:bool=true; note:textarea; status:select(active|blocked)^=active`,
		Hint("Guests who order. Points, visits and spend update automatically."),
		BeforeSave(func(a *App, tx *sql.Tx, c *Claims, r *Rec, old *Rec) error {
			if r.S("phone") == "" {
				r.Ref = "" // phone is optional; unique only when given
			}
			return nil
		}))
	def("discounts", "customers", "Discounts", `name*^~; type:select(percent|fixed)*^; value:number*^; min_order:money=0; channel:select(all|dine_in|takeaway|delivery|pickup)=all; start:date; end:date; status:select(active|inactive)^=active`, Scoped,
		Hint("Ready-made discounts staff can apply on the POS."))
	def("vouchers", "customers", "Vouchers", `code#*^~; type:select(percent|fixed)*^; value:number*^; min_order:money=0; max_uses:int=0; used:int.^=0; valid_from:date; valid_to:date; status:select(active|inactive)^=active`,
		Label("{code}"), Hint("Codes customers type at checkout. Max uses 0 = unlimited."))
	def("loyalty_programs", "customers", "Loyalty programs", `name*^~; earn_rate|Points per 100 spent:number*=5^; point_value|Value of 1 point:money*=100^; min_redeem:int=50; expiry_days:int=365; status:select(active|inactive)^=active`)
	def("loyalty_tiers", "customers", "Loyalty tiers", `name*^~; min_spend:money*^; multiplier|Points multiplier:number=1^; discount_pct|Tier discount %:number=0; color:color=#f97316`)
	def("loyalty_rewards", "customers", "Loyalty rewards", `name*^~; points_cost:int*^; type:select(discount|free_item|gift)=discount; value:money=0; product:ref(products); status:select(active|inactive)^=active`)
	def("gift_cards", "customers", "Gift cards",
		`code#*^~; initial:money*^; balance:money.^; customer:ref(customers); issued:date; expires:date; status:select(active|used|expired|blocked)^=active; note`, Label("{code}"),
		Hint("Prepaid cards customers can spend like cash."),
		BeforeSave(func(a *App, tx *sql.Tx, c *Claims, r *Rec, old *Rec) error {
			if old == nil {
				r.Set("balance", r.I("initial"))
				if r.S("issued") == "" {
					r.Set("issued", today())
				}
			}
			return nil
		}),
		AfterSave(func(a *App, tx *sql.Tx, c *Claims, r *Rec, old *Rec) error {
			if old == nil {
				mk(tx, "gift_txns", r.Branch, map[string]any{"card": float64(r.ID), "kind": "issue", "amount": float64(r.I("initial")), "note": "Card issued"})
				return a.postSimple(tx, r.Branch, today(), fmt.Sprintf("giftcard:%d", r.ID), "Gift card sold "+r.S("code"), "cash", "gift_liability", r.I("initial"), c.UID)
			}
			return nil
		}))
	def("gift_txns", "customers", "Gift card transactions", `card:ref(gift_cards)*^; kind:select(issue|redeem|topup|refund|expire)*^; amount:money*^; order:int; note`,
		NoCreate, NoDelete, Label("Txn #{id}"))
	def("gift_batches", "customers", "Gift card breakage batches",
		`name*^~; date:date*^; expired_cards:int.^; amount:money.^; status:select(draft|posted)^=draft; note`,
		Hint("Expired, unspent gift-card balances are recognised as income (breakage)."),
		ActFn("post", "Calculate & post", LvApprove, S("draft"), "ok", postBreakage), Confirm("post", "Expire old cards and post the income?"))
}

func setTableStatus(tx Querier, id int64, status string, force bool) {
	if id <= 0 {
		return
	}
	cond := ""
	if !force {
		cond = " AND status='available'"
	}
	tx.Exec(`UPDATE records SET status=?,updated_at=? WHERE id=? AND res='tables'`+cond, status, nowUnix(), id)
}

func purchaseBefore(a *App, tx *sql.Tx, c *Claims, r *Rec, old *Rec) error {
	if old != nil && old.Status == "received" {
		return uerr("a received purchase cannot be changed")
	}
	var sub int64
	for _, l := range r.L("lines") {
		sub += int64(toFloat(l["qty"]) * toFloat(l["cost"]))
	}
	r.Set("subtotal", float64(sub))
	r.Set("total", float64(sub+r.I("tax")))
	if old == nil {
		if r.S("no") == "" {
			r.Set("no", docNo(tx, "purchases", "PO"))
			r.Ref = r.S("no")
		}
		if r.Branch == 0 {
			r.Branch = c.Branch
		}
	}
	return nil
}

var _ = strings.TrimSpace
