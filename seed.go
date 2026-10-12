package main

import (
	"database/sql"
	"fmt"
	"log"
	"math/rand"
	"time"
)

func f(v int64) float64 { return float64(v) }

// seed fills a brand-new database with a believable demo restaurant group.
func (a *App) seed(demoPassword string) {
	var n int
	a.db.rdb.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	if n > 0 {
		return
	}
	log.Println("first run: creating demo data (this takes a few seconds)...")
	t0 := time.Now()
	now := nowUnix()
	for _, r := range defaultRoles {
		pj := jsonBytes(r.Perms)
		a.db.wdb.Exec(`INSERT OR IGNORE INTO roles(key,label,perms,builtin) VALUES(?,?,?,1)`, r.Key, r.Label, string(pj))
	}
	a.loadRoles()
	h := hashPassword(demoPassword)

	ids := map[string]int64{} // name -> id lookup used while seeding
	err := a.tx(func(tx *sql.Tx) error {
		a.seedChart(tx)
		// ---- branches ----
		main := mk(tx, "branches", 0, map[string]any{"name": "Main Branch", "code": "MAIN", "status": "active", "phone": "+91 98100 00001", "city": "Mumbai", "address": "12 Marine Drive, Mumbai", "lat": 18.943, "lng": 72.823, "opening_hours": "09:00-23:30"})
		dt := mk(tx, "branches", 0, map[string]any{"name": "Downtown Branch", "code": "DT", "status": "active", "phone": "+91 98100 00002", "city": "Mumbai", "address": "88 Linking Road, Bandra", "lat": 19.06, "lng": 72.836})
		ap := mk(tx, "branches", 0, map[string]any{"name": "Airport Kiosk", "code": "AP", "status": "active", "phone": "+91 98100 00003", "city": "Mumbai", "address": "Terminal 2, Mumbai Airport", "lat": 19.089, "lng": 72.868})
		ids["main"], ids["dt"], ids["ap"] = main, dt, ap
		// ---- users (one per role, password demo123) ----
		users := []struct {
			u, n, role string
			b          int64
		}{
			{"superadmin", "Super Admin", RoleSuperAdmin, 0}, {"admin", "Admin", RoleAdmin, 0}, {"admin_branch", "Branch Admin", RoleAdminBranch, dt},
			{"manager", "Manager", RoleManager, 0}, {"cashier", "Cashier", RoleCashier, main}, {"kitchen", "Kitchen", RoleKitchen, main}, {"waiter", "Waiter", RoleWaiter, main},
			{"hr_officer", "HR Officer", RoleHROfficer, 0}, {"hr_manager", "HR Manager", RoleHRManager, 0}, {"accountant", "Accountant", RoleAccountant, 0},
			{"accounting_manager", "Accounting Manager", RoleAccManager, 0}, {"accounting_auditor", "Accounting Auditor", RoleAccAuditor, 0}, {"pos_operator", "POS Operator", RolePOSOperator, main},
		}
		for _, u := range users {
			res, _ := tx.Exec(`INSERT INTO users(username,name,email,role,branch_id,pass_hash,created_at) VALUES(?,?,?,?,?,?,?)`, u.u, u.n, u.u+"@example.com", u.role, u.b, h, now)
			ids["u_"+u.u], _ = res.LastInsertId()
		}
		// extra staff logins for the busy branch
		for i, nm := range []string{"Aarav (Waiter)", "Meera (Waiter)", "Rohan (Cashier)"} {
			role := RoleWaiter
			if i == 2 {
				role = RoleCashier
			}
			un := fmt.Sprintf("staff%d", i+1)
			res, _ := tx.Exec(`INSERT INTO users(username,name,role,branch_id,pass_hash,created_at) VALUES(?,?,?,?,?,?)`, un, nm, role, main, h, now)
			ids["u_"+un], _ = res.LastInsertId()
		}

		// ---- settings-ish masters ----
		tax5 := mk(tx, "taxes", 0, map[string]any{"name": "GST 5%", "rate": 5.0, "default": true, "status": "active"})
		tax12 := mk(tx, "taxes", 0, map[string]any{"name": "GST 12%", "rate": 12.0, "status": "active"})
		mk(tx, "taxes", 0, map[string]any{"name": "GST 18%", "rate": 18.0, "status": "active"})
		mk(tx, "taxes", 0, map[string]any{"name": "No tax", "rate": 0.0, "status": "active"})
		for _, c := range [][3]any{{"INR", "Indian Rupee", 1.0}, {"USD", "US Dollar", 0.012}, {"AED", "UAE Dirham", 0.044}, {"JOD", "Jordanian Dinar", 0.0085}} {
			mk(tx, "currencies", 0, map[string]any{"code": c[0], "name": c[1], "rate": c[2], "as_of": today(), "status": "active"})
		}
		for _, r := range [][2]string{{"Customer changed mind", "cancel"}, {"Wrong order", "cancel"}, {"Out of stock", "cancel"}, {"Food quality issue", "refund"}, {"Long wait", "refund"}, {"Manager discount", "discount"}, {"Spillage", "waste"}} {
			mk(tx, "reasons", 0, map[string]any{"name": r[0], "type": r[1], "status": "active"})
		}
		for _, p := range []struct{ n, k string }{{"Receipt printer", "receipt"}, {"Kitchen printer", "kitchen"}} {
			mk(tx, "printers", main, map[string]any{"name": p.n, "kind": p.k, "connection": "browser", "paper_width": 80.0, "status": "active"})
		}
		mk(tx, "notif_templates", 0, map[string]any{"event": "new_order", "channel": "in_app", "subject": "New order", "body": "Order {order} received for {amount}", "status": "active"})
		for k, v := range map[string]string{"nav.pos": "पीओएस", "nav.orders": "ऑर्डर", "nav.kitchen": "रसोई", "nav.dashboard": "डैशबोर्ड"} {
			tid := mk(tx, "translations", 0, map[string]any{"lang": "hi", "key": k, "text": v})
			tx.Exec(`UPDATE records SET ref=? WHERE id=?`, "hi|"+k, tid)
		}

		// ---- registers, zones, floors, tables ----
		reg1 := mk(tx, "registers", main, map[string]any{"name": "Counter 1", "status": "active"})
		mk(tx, "registers", main, map[string]any{"name": "Counter 2", "status": "active"})
		mk(tx, "registers", dt, map[string]any{"name": "Downtown Till", "status": "active"})
		mk(tx, "registers", ap, map[string]any{"name": "Kiosk Till", "status": "active"})
		ids["reg1"] = reg1
		for _, br := range []int64{main, dt, ap} {
			fl := mk(tx, "floors", br, map[string]any{"name": "Ground floor", "level": 1.0})
			var zs []int64
			for _, z := range []string{"Indoor", "Terrace", "Quiet Zone"} {
				if br == ap && z != "Indoor" {
					continue
				}
				zs = append(zs, mk(tx, "zones", br, map[string]any{"name": z}))
			}
			cnt := 12
			if br == ap {
				cnt = 6
			}
			for i := 1; i <= cnt; i++ {
				zid := zs[(i-1)%len(zs)]
				id := mk(tx, "tables", br, map[string]any{"name": fmt.Sprintf("T%02d", i), "zone": f(zid), "floor": f(fl), "seats": f(int64(2 + 2*(i%3))), "shape": []string{"square", "round", "rect"}[i%3], "status": "available", "qr_token": randHex(6)})
				ids[fmt.Sprintf("t%d_%d", br, i)] = id
			}
		}

		// ---- kitchen stations & menu ----
		st := map[string]int64{}
		for _, s := range []string{"Main Kitchen", "Grill", "Tandoor", "Cold Kitchen", "Bar"} {
			st[s] = mk(tx, "stations", 0, map[string]any{"name": s, "color": "#f97316"})
		}
		cat := map[string]int64{}
		for i, c := range []struct{ n, e, s string }{{"Starters", "🥗", "Grill"}, {"Mains", "🍛", "Main Kitchen"}, {"Rice & Biryani", "🍚", "Main Kitchen"}, {"Breads", "🥖", "Tandoor"}, {"Manakish", "🥙", "Tandoor"},
			{"Drinks", "🥤", "Bar"}, {"Desserts", "🍨", "Cold Kitchen"}} {
			cat[c.n] = mk(tx, "categories", 0, map[string]any{"name": c.n, "emoji": c.e, "station": f(st[c.s]), "sort": f(int64(i)), "status": "active"})
		}
		unit := map[string]int64{}
		for _, u := range [][3]string{{"Kilogram", "kg", "weight"}, {"Gram", "g", "weight"}, {"Litre", "L", "volume"}, {"Piece", "pcs", "count"}, {"Millilitre", "ml", "volume"}} {
			unit[u[1]] = mk(tx, "units", 0, map[string]any{"name": u[0], "short": u[1], "type": u[2]})
		}
		sup := map[string]int64{}
		for _, s := range []struct{ n, c, t string }{{"Fresh Farms Co.", "Ramesh", "net7"}, {"City Meat & Poultry", "Imran", "net15"}, {"Spice Route Traders", "Sunita", "net30"}, {"Dairy Direct", "Kavita", "net7"}, {"Metro Wholesale", "Arjun", "net30"}} {
			sup[s.n] = mk(tx, "suppliers", 0, map[string]any{"name": s.n, "contact": s.c, "phone": "+91 99000 1" + fmt.Sprint(len(sup)), "payment_terms": s.t, "status": "active"})
		}
		ing := map[string]int64{}
		for _, x := range []struct {
			n, c, u string
			cost    int64
			re      float64
			s       string
		}{
			{"Chicken", "Meat", "kg", 22000, 15, "City Meat & Poultry"}, {"Paneer", "Dairy", "kg", 32000, 8, "Dairy Direct"}, {"Basmati rice", "Dry", "kg", 11000, 20, "Metro Wholesale"},
			{"Wheat flour", "Dry", "kg", 4500, 25, "Metro Wholesale"}, {"Tomato", "Veg", "kg", 4000, 10, "Fresh Farms Co."}, {"Onion", "Veg", "kg", 3500, 15, "Fresh Farms Co."},
			{"Cooking oil", "Dry", "L", 14000, 10, "Metro Wholesale"}, {"Butter", "Dairy", "kg", 52000, 5, "Dairy Direct"}, {"Cream", "Dairy", "L", 26000, 5, "Dairy Direct"},
			{"Garam masala", "Spice", "kg", 60000, 2, "Spice Route Traders"}, {"Milk", "Dairy", "L", 6000, 20, "Dairy Direct"}, {"Sugar", "Dry", "kg", 4500, 10, "Metro Wholesale"},
			{"Tea leaves", "Dry", "kg", 40000, 3, "Spice Route Traders"}, {"Coffee beans", "Dry", "kg", 90000, 3, "Spice Route Traders"}, {"Oranges", "Veg", "kg", 7000, 15, "Fresh Farms Co."},
			{"Lemons", "Veg", "pcs", 300, 60, "Fresh Farms Co."}, {"Eggs", "Dairy", "pcs", 700, 90, "Dairy Direct"}, {"Mozzarella", "Dairy", "kg", 45000, 6, "Dairy Direct"},
			{"Lentils (dal)", "Dry", "kg", 13000, 10, "Metro Wholesale"}, {"Potatoes", "Veg", "kg", 3000, 20, "Fresh Farms Co."}, {"Chickpeas", "Dry", "kg", 9000, 8, "Metro Wholesale"},
			{"Tahini", "Dry", "kg", 38000, 3, "Spice Route Traders"}, {"Zaatar", "Spice", "kg", 70000, 2, "Spice Route Traders"}, {"Yogurt", "Dairy", "kg", 8000, 10, "Dairy Direct"},
			{"Ice cream base", "Dairy", "L", 18000, 6, "Dairy Direct"}, {"Cocoa", "Dry", "kg", 65000, 2, "Spice Route Traders"},
		} {
			ing[x.n] = mk(tx, "ingredients", 0, map[string]any{"name": x.n, "category": x.c, "unit": f(unit[x.u]), "cost": f(x.cost), "reorder_level": x.re, "supplier": f(sup[x.s]), "status": "active"})
		}
		// options
		optSpice := mk(tx, "options", 0, map[string]any{"name": "Spice level", "kind": "single", "required": false, "status": "active", "choices": []any{map[string]any{"name": "Mild", "price": 0.0}, map[string]any{"name": "Medium", "price": 0.0}, map[string]any{"name": "Hot", "price": 0.0}}})
		optSize := mk(tx, "options", 0, map[string]any{"name": "Size", "kind": "single", "required": false, "status": "active", "choices": []any{map[string]any{"name": "Regular", "price": 0.0}, map[string]any{"name": "Large", "price": 4000.0}}})
		optExtra := mk(tx, "options", 0, map[string]any{"name": "Extras", "kind": "multiple", "required": false, "status": "active", "choices": []any{map[string]any{"name": "Extra cheese", "price": 3000.0}, map[string]any{"name": "Extra sauce", "price": 1500.0}}})

		type P struct {
			n, c, e string
			price   int64
			tax     int64
			rec     map[string]float64
			opts    []int64
		}
		prods := []P{
			{"Paneer Tikka", "Starters", "🧀", 22000, tax5, map[string]float64{"Paneer": 0.15, "Yogurt": 0.05, "Garam masala": 0.005}, []int64{optSpice}},
			{"Chicken 65", "Starters", "🍗", 24000, tax5, map[string]float64{"Chicken": 0.2, "Cooking oil": 0.05, "Garam masala": 0.005}, []int64{optSpice}},
			{"Veg Spring Roll", "Starters", "🥟", 14000, tax5, map[string]float64{"Wheat flour": 0.08, "Onion": 0.05, "Cooking oil": 0.04}, nil},
			{"Hummus Platter", "Starters", "🥣", 18000, tax5, map[string]float64{"Chickpeas": 0.1, "Tahini": 0.03, "Lemons": 0.5}, nil},
			{"Butter Chicken", "Mains", "🍛", 32000, tax5, map[string]float64{"Chicken": 0.25, "Butter": 0.03, "Cream": 0.05, "Tomato": 0.15}, []int64{optSpice, optSize}},
			{"Dal Makhani", "Mains", "🍲", 22000, tax5, map[string]float64{"Lentils (dal)": 0.12, "Butter": 0.03, "Cream": 0.04, "Tomato": 0.08}, []int64{optSpice}},
			{"Paneer Butter Masala", "Mains", "🍛", 26000, tax5, map[string]float64{"Paneer": 0.18, "Butter": 0.03, "Cream": 0.05, "Tomato": 0.15}, []int64{optSpice, optSize}},
			{"Chole Masala", "Mains", "🍲", 20000, tax5, map[string]float64{"Chickpeas": 0.15, "Onion": 0.08, "Tomato": 0.1}, []int64{optSpice}},
			{"Aloo Gobi", "Mains", "🥔", 18000, tax5, map[string]float64{"Potatoes": 0.15, "Onion": 0.05}, nil},
			{"Veg Biryani", "Rice & Biryani", "🍚", 24000, tax5, map[string]float64{"Basmati rice": 0.2, "Onion": 0.06, "Cooking oil": 0.03, "Garam masala": 0.006}, []int64{optSpice}},
			{"Chicken Biryani", "Rice & Biryani", "🍗", 29000, tax5, map[string]float64{"Basmati rice": 0.2, "Chicken": 0.22, "Onion": 0.06, "Garam masala": 0.006}, []int64{optSpice, optSize}},
			{"Jeera Rice", "Rice & Biryani", "🍚", 12000, tax5, map[string]float64{"Basmati rice": 0.15}, nil},
			{"Butter Naan", "Breads", "🥖", 4500, tax5, map[string]float64{"Wheat flour": 0.1, "Butter": 0.01}, nil},
			{"Garlic Naan", "Breads", "🥖", 5500, tax5, map[string]float64{"Wheat flour": 0.1, "Butter": 0.01}, nil},
			{"Tandoori Roti", "Breads", "🥖", 2500, tax5, map[string]float64{"Wheat flour": 0.08}, nil},
			{"Zaatar Manakish", "Manakish", "🥙", 12000, tax5, map[string]float64{"Wheat flour": 0.15, "Zaatar": 0.02, "Cooking oil": 0.02}, []int64{optExtra}},
			{"Cheese Manakish", "Manakish", "🧀", 15000, tax5, map[string]float64{"Wheat flour": 0.15, "Mozzarella": 0.08}, []int64{optExtra}},
			{"Meat Manakish", "Manakish", "🥙", 17000, tax5, map[string]float64{"Wheat flour": 0.15, "Chicken": 0.1}, []int64{optExtra}},
			{"Fresh Orange Juice", "Drinks", "🍊", 9000, tax12, map[string]float64{"Oranges": 0.4}, []int64{optSize}},
			{"Masala Chai", "Drinks", "🍵", 4000, tax5, map[string]float64{"Tea leaves": 0.005, "Milk": 0.12, "Sugar": 0.01}, nil},
			{"Cold Coffee", "Drinks", "🧋", 11000, tax12, map[string]float64{"Coffee beans": 0.015, "Milk": 0.2, "Sugar": 0.02}, []int64{optSize}},
			{"Fresh Lime Soda", "Drinks", "🥤", 7000, tax12, map[string]float64{"Lemons": 2, "Sugar": 0.02}, nil},
			{"Lassi", "Drinks", "🥛", 8000, tax5, map[string]float64{"Yogurt": 0.2, "Sugar": 0.02}, nil},
			{"Gulab Jamun", "Desserts", "🍡", 8000, tax5, map[string]float64{"Milk": 0.1, "Sugar": 0.05}, nil},
			{"Chocolate Brownie", "Desserts", "🍫", 13000, tax12, map[string]float64{"Cocoa": 0.02, "Butter": 0.03, "Sugar": 0.04, "Eggs": 1}, nil},
			{"Ice Cream Scoop", "Desserts", "🍨", 6000, tax12, map[string]float64{"Ice cream base": 0.1}, nil},
		}
		for _, p := range prods {
			data := map[string]any{"name": p.n, "category": f(cat[p.c]), "price": f(p.price), "tax": f(p.tax), "emoji": p.e, "prep_min": 12.0, "track_stock": true, "available": true, "status": "active"}
			if len(p.opts) > 0 {
				arr := []any{}
				for _, o := range p.opts {
					arr = append(arr, f(o))
				}
				data["options"] = arr
			}
			pid := mk(tx, "products", 0, data)
			ids["p_"+p.n] = pid
			var lines []any
			for in, q := range p.rec {
				lines = append(lines, map[string]any{"ingredient": f(ing[in]), "qty": q})
			}
			mk(tx, "recipes", 0, map[string]any{"product": f(pid), "portions": 1.0, "lines": lines, "waste_pct": 3.0})
		}
		mk(tx, "menus", 0, map[string]any{"name": "Main menu", "type": "both", "status": "active", "categories": []any{f(cat["Starters"]), f(cat["Mains"]), f(cat["Rice & Biryani"]), f(cat["Breads"]), f(cat["Drinks"]), f(cat["Desserts"])}})
		mk(tx, "menus", 0, map[string]any{"name": "Manakish bar", "type": "dine_in", "status": "active", "categories": []any{f(cat["Manakish"])}})

		// ---- loyalty, promotions, gift cards ----
		mk(tx, "loyalty_programs", 0, map[string]any{"name": "TableBill Rewards", "earn_rate": 5.0, "point_value": 100.0, "min_redeem": 50.0, "expiry_days": 365.0, "status": "active"})
		mk(tx, "loyalty_tiers", 0, map[string]any{"name": "Silver", "min_spend": 500000.0, "multiplier": 1.2, "discount_pct": 0.0, "color": "#94a3b8"})
		mk(tx, "loyalty_tiers", 0, map[string]any{"name": "Gold", "min_spend": 1500000.0, "multiplier": 1.5, "discount_pct": 5.0, "color": "#f59e0b"})
		mk(tx, "loyalty_rewards", 0, map[string]any{"name": "₹50 off", "points_cost": 50.0, "type": "discount", "value": 5000.0, "status": "active"})
		mk(tx, "discounts", 0, map[string]any{"name": "Happy hour 10%", "type": "percent", "value": 10.0, "channel": "all", "status": "active"})
		mk(tx, "discounts", 0, map[string]any{"name": "Staff meal 50%", "type": "percent", "value": 50.0, "channel": "dine_in", "status": "active"})
		mk(tx, "vouchers", 0, map[string]any{"code": "WELCOME10", "type": "percent", "value": 10.0, "min_order": 20000.0, "max_uses": 0.0, "used": 0.0, "status": "active"})
		mk(tx, "vouchers", 0, map[string]any{"code": "FLAT100", "type": "fixed", "value": 10000.0, "min_order": 50000.0, "max_uses": 100.0, "used": 0.0, "status": "active"})
		for i, bal := range []int64{100000, 50000, 200000} {
			id := mk(tx, "gift_cards", 0, map[string]any{"code": fmt.Sprintf("GIFT-%04d", 1001+i), "initial": f(bal), "balance": f(bal), "issued": today(), "expires": time.Now().AddDate(1, 0, 0).Format("2006-01-02"), "status": "active"})
			mk(tx, "gift_txns", 0, map[string]any{"card": f(id), "kind": "issue", "amount": f(bal), "note": "Card issued"})
		}
		custNames := []string{"Rahul Sharma", "Priya Patel", "Amit Verma", "Sneha Iyer", "Vikram Singh", "Anjali Nair", "Karan Mehta", "Neha Gupta", "Rohit Jain", "Pooja Reddy", "Sanjay Kulkarni", "Divya Menon",
			"Arjun Kapoor", "Ritu Desai", "Manish Agarwal", "Kavya Rao", "Deepak Joshi", "Isha Malhotra", "Faisal Khan", "Layla Hassan"}
		for i, nm := range custNames {
			ids[fmt.Sprintf("c%d", i)] = mk(tx, "customers", 0, map[string]any{"name": nm, "phone": fmt.Sprintf("98%08d", 10000000+i*37119), "email": fmt.Sprintf("customer%d@example.com", i), "points": 0.0, "visits": 0.0, "spent": 0.0, "marketing": true, "status": "active",
				"address": fmt.Sprintf("%d, Hill Road, Mumbai", 10+i), "birthday": fmt.Sprintf("199%d-%02d-%02d", i%10, 1+i%12, 1+i%27)})
		}

		// ---- accounting setup ----
		fy := mk(tx, "fiscal_years", 0, map[string]any{"name": fmt.Sprintf("FY %d", time.Now().Year()), "from": fmt.Sprintf("%d-01-01", time.Now().Year()), "to": fmt.Sprintf("%d-12-31", time.Now().Year()), "status": "open"})
		for m := 0; m < 12; m++ {
			s := time.Date(time.Now().Year(), time.Month(m+1), 1, 0, 0, 0, 0, time.Local)
			mk(tx, "periods", 0, map[string]any{"name": s.Format("Jan 2006"), "fiscal_year": f(fy), "from": s.Format("2006-01-02"), "to": s.AddDate(0, 1, -1).Format("2006-01-02"), "status": "open"})
		}
		mk(tx, "fin_accounts", 0, map[string]any{"name": "Cash drawer", "kind": "cash", "ledger": "cash", "currency": "INR", "opening_balance": 0.0, "status": "active"})
		mk(tx, "fin_accounts", 0, map[string]any{"name": "HDFC current account", "kind": "bank", "ledger": "bank", "number": "50200012345678", "currency": "INR", "opening_balance": 0.0, "status": "active"})
		mk(tx, "fin_accounts", 0, map[string]any{"name": "Card machine clearing", "kind": "card", "ledger": "card_clearing", "currency": "INR", "status": "active"})
		mk(tx, "legal_entities", 0, map[string]any{"name": "TableBill Hospitality Pvt Ltd", "code": "TBH", "country": "India", "currency": "INR", "status": "active", "branches": []any{f(main), f(dt), f(ap)}})
		mk(tx, "dimensions", 0, map[string]any{"name": "Cost centre", "code": "CC", "status": "active", "values": []any{map[string]any{"code": "KIT", "name": "Kitchen"}, map[string]any{"code": "SVC", "name": "Service"}, map[string]any{"code": "ADM", "name": "Admin"}}})
		mk(tx, "revenue_centers", 0, map[string]any{"name": "Dining room", "code": "DINE", "channel": "dine_in", "status": "active"})
		mk(tx, "revenue_centers", 0, map[string]any{"name": "Delivery", "code": "DLV", "channel": "delivery", "status": "active"})
		mk(tx, "withholding_rules", 0, map[string]any{"name": "TDS on contractors 2%", "applies_to": "service", "rate": 2.0, "threshold": 3000000.0, "status": "active"})
		assetPol := mk(tx, "asset_policies", 0, map[string]any{"name": "Kitchen equipment (5 yrs)", "method": "straight_line", "life_years": 5.0, "salvage_pct": 5.0, "status": "active"})
		mk(tx, "costing_policies", 0, map[string]any{"name": "Weighted average", "method": "average", "applies_to": "all", "include_waste": true, "status": "active"})
		mk(tx, "fixed_assets", main, map[string]any{"name": "Tandoor oven", "tag": "FA-001", "policy": f(assetPol), "cost": 18000000.0, "purchase_date": time.Now().AddDate(0, -4, 0).Format("2006-01-02"), "funded_by": "none", "accumulated": 0.0, "book_value": 18000000.0, "status": "active"})

		// ---- HR setup ----
		dep := map[string]int64{}
		for _, d := range []string{"Kitchen", "Service", "Management", "Delivery", "Accounts & HR"} {
			dep[d] = mk(tx, "departments", 0, map[string]any{"name": d, "status": "active"})
		}
		des := map[string]int64{}
		for _, d := range []struct{ n, dep string }{{"Head Chef", "Kitchen"}, {"Cook", "Kitchen"}, {"Waiter", "Service"}, {"Cashier", "Service"}, {"Restaurant Manager", "Management"}, {"Rider", "Delivery"}, {"Accountant", "Accounts & HR"}, {"HR Executive", "Accounts & HR"}} {
			des[d.n] = mk(tx, "designations", 0, map[string]any{"name": d.n, "department": f(dep[d.dep]), "status": "active"})
		}
		shM := mk(tx, "shifts", main, map[string]any{"name": "Morning (07-15)", "start": "07:00", "end": "15:00", "break_min": 45.0, "grace_min": 10.0, "status": "active"})
		shE := mk(tx, "shifts", main, map[string]any{"name": "Evening (15-23)", "start": "15:00", "end": "23:00", "break_min": 45.0, "grace_min": 10.0, "status": "active"})
		mk(tx, "shifts", main, map[string]any{"name": "Night (23-07)", "start": "23:00", "end": "07:00", "break_min": 45.0, "grace_min": 10.0, "night": true, "status": "active"})
		for _, h := range []struct{ n, d string }{{"Republic Day", "01-26"}, {"Independence Day", "08-15"}, {"Gandhi Jayanti", "10-02"}, {"Diwali", "11-01"}, {"Christmas", "12-25"}} {
			mk(tx, "holidays", 0, map[string]any{"name": h.n, "date": fmt.Sprintf("%d-%s", time.Now().Year(), h.d), "kind": "public"})
		}
		lt := map[string]int64{}
		for _, l := range []struct {
			n, c string
			d    float64
			paid bool
		}{{"Annual leave", "AL", 21, true}, {"Sick leave", "SL", 10, true}, {"Casual leave", "CL", 7, true}, {"Unpaid leave", "UL", 30, false}} {
			lt[l.c] = mk(tx, "leave_types", 0, map[string]any{"name": l.n, "code": l.c, "paid": l.paid, "days_per_year": l.d, "carry_forward": l.c == "AL", "max_carry": 5.0, "status": "active"})
		}
		mk(tx, "leave_rules", 0, map[string]any{"name": "Auto-approve short casual leave", "leave_type": f(lt["CL"]), "min_days": 0.0, "approver": "hr_manager", "auto_approve_below": 1.0, "status": "active"})
		mk(tx, "leave_rules", 0, map[string]any{"name": "Long leave needs HR manager", "min_days": 3.0, "approver": "hr_manager", "auto_approve_below": 0.0, "status": "active"})
		mk(tx, "leave_policies", 0, map[string]any{"name": "Kitchen annual leave", "leave_type": f(lt["AL"]), "entitlement": 24.0, "accrual": "yearly", "department": f(dep["Kitchen"]), "status": "active"})
		mk(tx, "geofences", main, map[string]any{"name": "Main Branch premises", "lat": 18.943, "lng": 72.823, "radius_m": 200.0, "status": "active"})
		mk(tx, "doc_types", 0, map[string]any{"name": "ID proof", "required": true, "has_expiry": false, "status": "active"})
		mk(tx, "doc_types", 0, map[string]any{"name": "Food handler certificate", "required": true, "has_expiry": true, "status": "active"})
		// payroll components
		hra := mk(tx, "salary_components", 0, map[string]any{"name": "House rent allowance", "code": "HRA", "kind": "earning", "calc": "percent_of_basic", "percent": 20.0, "taxable": true, "status": "active"})
		tr := mk(tx, "salary_components", 0, map[string]any{"name": "Transport allowance", "code": "TRN", "kind": "earning", "calc": "fixed", "amount": 150000.0, "taxable": false, "status": "active"})
		mk(tx, "salary_components", 0, map[string]any{"name": "Meal deduction", "code": "MEAL", "kind": "deduction", "calc": "fixed", "amount": 50000.0, "status": "active"})
		mk(tx, "salary_components", 0, map[string]any{"name": "Employer insurance", "code": "INS", "kind": "employer_contribution", "calc": "fixed", "amount": 80000.0, "status": "active"})
		ssKitchen := mk(tx, "salary_structures", 0, map[string]any{"name": "Kitchen staff", "basic": 1800000.0, "status": "active", "lines": []any{map[string]any{"component": f(hra), "amount": 0.0, "percent": 20.0}, map[string]any{"component": f(tr), "amount": 150000.0, "percent": 0.0}}})
		ssService := mk(tx, "salary_structures", 0, map[string]any{"name": "Service staff", "basic": 1500000.0, "status": "active", "lines": []any{map[string]any{"component": f(hra), "amount": 0.0, "percent": 15.0}}})
		ssMgr := mk(tx, "salary_structures", 0, map[string]any{"name": "Management", "basic": 4500000.0, "status": "active", "lines": []any{map[string]any{"component": f(hra), "amount": 0.0, "percent": 25.0}, map[string]any{"component": f(tr), "amount": 300000.0, "percent": 0.0}}})
		mk(tx, "tax_policies", 0, map[string]any{"name": "Monthly slab tax", "standard_deduction": 4166700.0, "default": true, "status": "active",
			"brackets": []any{map[string]any{"up_to": 5000000.0, "rate": 0.0}, map[string]any{"up_to": 10000000.0, "rate": 5.0}, map[string]any{"up_to": 0.0, "rate": 10.0}}})
		mk(tx, "social_policies", 0, map[string]any{"name": "Provident fund", "employee_pct": 12.0, "employer_pct": 12.0, "ceiling": 1500000.0, "default": true, "status": "active"})

		// employees
		type E struct {
			n, des, dep string
			ss          int64
			sh          int64
			br          int64
			user        string
		}
		emps := []E{
			{"Sanjeev Rao", "Head Chef", "Kitchen", ssMgr, shM, main, ""}, {"Mohan Das", "Cook", "Kitchen", ssKitchen, shM, main, "kitchen"}, {"Lakshmi Pillai", "Cook", "Kitchen", ssKitchen, shE, main, ""},
			{"Imran Sheikh", "Cook", "Kitchen", ssKitchen, shE, dt, ""}, {"Aarav Nair", "Waiter", "Service", ssService, shM, main, "staff1"}, {"Meera Shah", "Waiter", "Service", ssService, shE, main, "staff2"},
			{"Waiter Demo", "Waiter", "Service", ssService, shE, main, "waiter"}, {"Rohan Dutta", "Cashier", "Service", ssService, shM, main, "staff3"}, {"Cashier Demo", "Cashier", "Service", ssService, shM, main, "cashier"},
			{"Pooja Bhatt", "Waiter", "Service", ssService, shE, dt, ""}, {"Ravi Kumar", "Rider", "Delivery", ssService, shE, main, ""}, {"Suresh Yadav", "Rider", "Delivery", ssService, shM, main, ""},
			{"Manager Demo", "Restaurant Manager", "Management", ssMgr, shM, main, "manager"}, {"Nisha Kapoor", "Accountant", "Accounts & HR", ssMgr, shM, main, "accountant"}, {"Tara Menon", "HR Executive", "Accounts & HR", ssMgr, shM, main, "hr_officer"},
			{"Dev Malhotra", "Cook", "Kitchen", ssKitchen, shM, ap, ""}, {"Zoya Ali", "Cashier", "Service", ssService, shE, ap, ""}, {"Kabir Anand", "Waiter", "Service", ssService, shM, dt, ""},
		}
		for i, e := range emps {
			d := map[string]any{"code": fmt.Sprintf("EMP-%03d", i+1), "name": e.n, "designation": f(des[e.des]), "department": f(dep[e.dep]), "shift": f(e.sh), "join_date": time.Now().AddDate(-1-i%3, -i, 0).Format("2006-01-02"),
				"employment_type": "full_time", "salary_structure": f(e.ss), "phone": fmt.Sprintf("97%08d", 5000000+i*1313), "gender": []string{"male", "female"}[i%2], "status": "active"}
			if e.user != "" {
				d["user"] = f(ids["u_"+e.user])
			}
			ids[fmt.Sprintf("e%d", i)] = mk(tx, "employees", e.br, d)
		}
		ids["nEmp"] = int64(len(emps))

		// delivery setup
		z1 := mk(tx, "delivery_zones", main, map[string]any{"name": "Zone A (0-3 km)", "fee": 3000.0, "min_order": 20000.0, "eta_min": 30.0, "radius_km": 3.0, "status": "active"})
		mk(tx, "delivery_zones", main, map[string]any{"name": "Zone B (3-7 km)", "fee": 6000.0, "min_order": 35000.0, "eta_min": 45.0, "radius_km": 7.0, "status": "active"})
		mk(tx, "pricing_rules", 0, map[string]any{"name": "Big orders ship cheaper", "min_order": 100000.0, "max_order": 0.0, "fee": 1500.0, "status": "active"})
		mk(tx, "peak_surcharges", 0, map[string]any{"name": "Dinner rush", "from": "20:00", "to": "22:00", "kind": "fixed", "value": 20.0, "status": "active"})
		mk(tx, "driver_rules", 0, map[string]any{"name": "Standard rider pay", "type": "per_delivery", "base": 4000.0, "min_payout": 3000.0, "bonus_after": 8.0, "bonus": 20000.0, "status": "active"})
		for i, d := range []struct{ n, v string }{{"Ravi Kumar", "bike"}, {"Suresh Yadav", "scooter"}, {"Imtiyaz Ali", "bike"}, {"Gopal Singh", "bicycle"}} {
			ids[fmt.Sprintf("drv%d", i)] = mk(tx, "drivers", main, map[string]any{"name": d.n, "phone": fmt.Sprintf("96%08d", 1000000+i*7777), "vehicle": d.v, "type": "employee", "status": []string{"available", "available", "busy", "offline"}[i],
				"lat": 18.94 + float64(i)*0.01, "lng": 72.82 + float64(i)*0.008, "last_seen": time.Now().Format("2006-01-02T15:04"), "rating": 4.6})
		}
		mk(tx, "couriers", 0, map[string]any{"name": "QuickShip Couriers", "code": "QSHIP", "type": "custom", "fee_model": "per_km", "base_fee": 4000.0, "per_km": 800.0, "commission": 0.0, "status": "active"})
		ids["z1"] = z1
		return nil
	})
	if err != nil {
		log.Fatal("seed (masters): ", err)
	}
	for _, k := range []string{"products", "categories", "options", "stations", "recipes", "taxes", "ingredients", "menus", "branches", "tables", "reservations", "pos_sessions"} {
		bumpRes(k)
	}
	a.loadChart()
	a.loadSettings()

	a.seedHistory(ids)
	log.Printf("demo data ready in %.1fs", time.Since(t0).Seconds())
}

// seedHistory creates ~30 days of orders, stock, purchases, attendance, payroll and deliveries.
func (a *App) seedHistory(ids map[string]int64) {
	rng := rand.New(rand.NewSource(11))
	main, dt, ap := ids["main"], ids["dt"], ids["ap"]
	branches := []int64{main, main, main, dt, dt, ap}
	cashier := &Claims{UID: ids["u_cashier"], User: "cashier", Name: "Cashier", Role: RoleCashier, Branch: 0}
	snap := a.menu()
	var pids []int64
	for id := range snap.products {
		pids = append(pids, id)
	}
	sortInt64(pids)

	err := a.tx(func(tx *sql.Tx) error {
		// opening cash & stock via purchases (received on day -30)
		ingIDs := []int64{}
		for _, r := range recs(tx, "ingredients", 0) {
			ingIDs = append(ingIDs, r.ID)
		}
		for _, br := range []int64{main, dt, ap} {
			var lines []any
			for _, id := range ingIDs {
				qty := 220.0 + rng.Float64()*120
				if br != main {
					qty /= 2
				}
				lines = append(lines, map[string]any{"ingredient": f(id), "qty": qty, "cost": f(a.ingCost(tx, id))})
			}
			sup := recs(tx, "suppliers", 0)[0]
			day := time.Now().AddDate(0, 0, -30).Format("2006-01-02")
			p := &Rec{Res: "purchases", Branch: br, Status: "ordered", Data: map[string]any{"supplier": f(sup.ID), "date": day, "lines": lines, "tax": 0.0}}
			purchaseBefore(a, tx, cashier, p, nil)
			p.Ref = p.S("no")
			saveRec(tx, p)
			if err := receivePurchase(a, tx, cashier, p, nil); err != nil {
				return err
			}
			saveRec(tx, p)
		}
		// owner capital so the books start with money in the bank
		a.post(tx, 0, time.Now().AddDate(0, 0, -31).Format("2006-01-02"), "opening", "Owner capital", 0, Dr("bank", 300000000), Dr("cash", 10000000), Cr("equity", 310000000))
		a.post(tx, 0, time.Now().AddDate(0, 0, -31).Format("2006-01-02"), "opening", "Kitchen equipment", 0, Dr("fixed_assets", 18000000), Cr("equity", 18000000))
		return nil
	})
	if err != nil {
		log.Println("seed purchases:", err)
	}

	staff := []int64{ids["u_cashier"], ids["u_staff1"], ids["u_staff2"], ids["u_staff3"], ids["u_waiter"]}
	methods := []string{"cash", "cash", "cash", "card", "card", "upi", "upi", "wallet"}
	for day := 30; day >= 0; day-- {
		n := 26 + rng.Intn(22)
		if day == 0 {
			n = 6
		}
		dayStart := time.Now().AddDate(0, 0, -day)
		weekend := dayStart.Weekday() == time.Friday || dayStart.Weekday() == time.Saturday
		if weekend {
			n += 16
		}
		err := a.tx(func(tx *sql.Tx) error {
			for i := 0; i < n; i++ {
				br := branches[rng.Intn(len(branches))]
				uid := staff[rng.Intn(len(staff))]
				c := &Claims{UID: uid, User: "seed", Name: "seed", Role: RoleCashier, Branch: br}
				ch := []string{"dine_in", "dine_in", "dine_in", "dine_in", "takeaway", "takeaway", "delivery", "pickup"}[rng.Intn(8)]
				in := &OrderIn{Channel: ch, BranchID: br}
				if ch == "dine_in" {
					in.TableID = ids[fmt.Sprintf("t%d_%d", br, 1+rng.Intn(6))]
				}
				if rng.Intn(3) == 0 {
					in.CustomerID = ids[fmt.Sprintf("c%d", rng.Intn(20))]
				}
				if ch == "delivery" {
					in.Customer, in.Phone, in.Address = "Delivery guest", fmt.Sprintf("97000%05d", rng.Intn(99999)), "Flat 4B, Sea View Apartments, Worli"
					in.Zone = ids["z1"]
				}
				for k := 0; k < 1+rng.Intn(4); k++ {
					in.Items = append(in.Items, ItemIn{Product: pids[rng.Intn(len(pids))], Qty: int64(1 + rng.Intn(3))})
				}
				if rng.Intn(12) == 0 {
					in.DiscType, in.DiscValue = "percent", 10
				}
				if rng.Intn(5) == 0 {
					in.Tip = 2000
				}
				if ch == "dine_in" { // free the table so the next guest can sit
					setTableStatus(tx, in.TableID, "available", true)
					tx.Exec(`UPDATE orders SET status='completed' WHERE table_id=? AND status IN ('pending','preparing','ready','served')`, in.TableID)
				}
				o, err := a.placeOrder(tx, c, in)
				if err != nil {
					continue
				}
				// 4% cancelled, 2% refunded, rest paid
				ts := time.Date(dayStart.Year(), dayStart.Month(), dayStart.Day(), 11+rng.Intn(11), rng.Intn(60), 0, 0, dayStart.Location()).Unix()
				if day == 0 && ts > nowUnix() {
					ts = nowUnix() - int64(rng.Intn(3600))
				}
				roll := rng.Intn(100)
				if roll < 4 {
					tx.Exec(`UPDATE order_items SET status='void' WHERE order_id=?`, o.ID)
					a.restoreStock(tx, o.Branch, o.ID, uid, "seed cancel")
					tx.Exec(`UPDATE orders SET status='cancelled',reason='Customer changed mind' WHERE id=?`, o.ID)
				} else if day == 0 && rng.Intn(3) > 0 { // today: leave a few open for the kitchen screens
					st := []string{"pending", "preparing", "ready", "served"}[rng.Intn(4)]
					tx.Exec(`UPDATE orders SET status=? WHERE id=?`, st, o.ID)
					tx.Exec(`UPDATE order_items SET status=? WHERE order_id=?`, map[string]string{"pending": "pending", "preparing": "preparing", "ready": "ready", "served": "served"}[st], o.ID)
					if ch == "dine_in" {
						setTableStatus(tx, in.TableID, "occupied", true)
					}
				} else {
					seedPay(a, tx, cashier, o, methods[rng.Intn(len(methods))], rng)
					if roll > 97 {
						tx.Exec(`UPDATE orders SET status='refunded',pay_status='refunded',reason='Food quality issue' WHERE id=?`, o.ID)
					}
				}
				// backdate everything this order touched
				tx.Exec(`UPDATE orders SET created_at=?,updated_at=?+900 WHERE id=?`, ts, ts, o.ID)
				tx.Exec(`UPDATE payments SET created_at=? WHERE order_id=?`, ts+600, o.ID)
				tx.Exec(`UPDATE stock_moves SET created_at=? WHERE ref=?`, ts, fmt.Sprintf("order:%d", o.ID))
				d := time.Unix(ts, 0).Format("2006-01-02")
				tx.Exec(`UPDATE journals SET date=?,created_at=? WHERE source=?`, d, ts, fmt.Sprintf("order:%d", o.ID))
				tx.Exec(`UPDATE journal_lines SET date=? WHERE journal_id IN (SELECT id FROM journals WHERE source=?)`, d, fmt.Sprintf("order:%d", o.ID))
				if ch == "delivery" {
					seedDelivery(a, tx, o, ids, rng, ts)
				}
			}
			return nil
		})
		if err != nil {
			log.Println("seed day", day, err)
		}
	}
	// leave today's dine-in tables in a sensible state
	a.db.wdb.Exec(`UPDATE records SET status='available' WHERE res='tables' AND status<>'occupied'`)
	a.db.wdb.Exec(`UPDATE records SET status='occupied' WHERE res='tables' AND id IN (SELECT table_id FROM orders WHERE status IN ('pending','preparing','ready','served') AND table_id>0)`)
	a.db.wdb.Exec(`UPDATE records SET status='available' WHERE res='tables' AND status='occupied' AND id NOT IN (SELECT table_id FROM orders WHERE status IN ('pending','preparing','ready','served') AND table_id>0)`)
	a.db.wdb.Exec(`UPDATE records SET status='cleaning' WHERE res='tables' AND id IN (SELECT id FROM records WHERE res='tables' ORDER BY id LIMIT 2)`)

	a.seedHR(ids, rng)
	a.seedExtras(ids, rng)
	for _, k := range []string{"tables", "customers", "deliveries", "drivers", "driver_earnings", "ingredients", "employees", "payslips", "payroll_runs", "supplier_bills", "purchases"} {
		bumpRes(k)
	}
}

func sortInt64(a []int64) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}

func seedPay(a *App, tx *sql.Tx, c *Claims, o *Order, method string, rng *rand.Rand) {
	cc := *c
	cc.Branch = o.Branch
	if o.CustomerID == 0 && method == "wallet" {
		method = "upi"
	}
	tx.Exec(`INSERT INTO payments(order_id,branch_id,session_id,method,kind,amount,ref,created_by,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, o.ID, o.Branch, 0, method, "payment", o.Total, "", c.UID, nowUnix())
	tx.Exec(`UPDATE orders SET paid=?,status='completed',updated_at=? WHERE id=?`, o.Total, nowUnix(), o.ID)
	tx.Exec(`UPDATE order_items SET status='served' WHERE order_id=?`, o.ID)
	var cust *Rec
	if o.CustomerID > 0 {
		cust, _ = loadRec(tx, o.CustomerID)
	}
	if ord, err := a.recalc(tx, o.ID, &cc); err == nil {
		ord.Status = "completed"
		a.onFullyPaid(tx, &cc, ord, cust)
	}
}

func seedDelivery(a *App, tx *sql.Tx, o *Order, ids map[string]int64, rng *rand.Rand, ts int64) {
	drv := ids[fmt.Sprintf("drv%d", rng.Intn(3))]
	fail := rng.Intn(15) == 0
	st := "delivered"
	if fail {
		st = "failed"
	}
	pick := ts + 600 + int64(rng.Intn(600))
	del := pick + 900 + int64(rng.Intn(1500))
	tx.Exec(`UPDATE records SET status=?,created_at=?,data=json_set(data,'$.driver',?,'$.assigned_at',?,'$.picked_at',?,'$.delivered_at',?,'$.distance_km',?,'$.cod_status',?,'$.driver_pay',?) WHERE res='deliveries' AND json_extract(data,'$.order_id')=?`,
		st, ts, drv, ts+300, pick, del, 2+rng.Float64()*5, "settled", 4000, o.ID)
	if !fail {
		mk(tx, "driver_earnings", o.Branch, map[string]any{"driver": f(drv), "date": time.Unix(ts, 0).Format("2006-01-02"), "kind": "delivery", "amount": 4000.0, "status": "paid"})
	}
}

func (a *App) seedHR(ids map[string]int64, rng *rand.Rand) {
	n := int(ids["nEmp"])
	err := a.tx(func(tx *sql.Tx) error {
		off := a.weeklyOff()
		for i := 0; i < n; i++ {
			emp, _ := loadRec(tx, ids[fmt.Sprintf("e%d", i)])
			if emp == nil || emp.Res != "employees" {
				continue
			}
			sh, _ := loadRec(tx, emp.I("shift"))
			start := 7
			if sh != nil && sh.S("start") == "15:00" {
				start = 15
			}
			for day := 29; day >= 0; day-- {
				d := time.Now().AddDate(0, 0, -day)
				if d.Weekday() == off {
					continue
				}
				ds := d.Format("2006-01-02")
				roll := rng.Intn(100)
				switch {
				case roll < 4:
					tx.Exec(`INSERT OR IGNORE INTO attendance(employee_id,branch_id,date,status,source) VALUES(?,?,?,'absent','auto')`, emp.ID, emp.Branch, ds)
				case roll < 12:
					late := 12 + rng.Intn(30)
					in := time.Date(d.Year(), d.Month(), d.Day(), start, late, 0, 0, d.Location())
					out := in.Add(8*time.Hour + time.Duration(rng.Intn(60))*time.Minute)
					tx.Exec(`INSERT OR IGNORE INTO attendance(employee_id,branch_id,date,check_in,check_out,status,late_min,worked_min,source) VALUES(?,?,?,?,?,'late',?,?,'app')`, emp.ID, emp.Branch, ds, in.Unix(), out.Unix(), late, int(out.Sub(in).Minutes())-45)
				default:
					in := time.Date(d.Year(), d.Month(), d.Day(), start, rng.Intn(8), 0, 0, d.Location())
					out := in.Add(8*time.Hour + time.Duration(rng.Intn(90))*time.Minute)
					wm := int(out.Sub(in).Minutes()) - 45
					tx.Exec(`INSERT OR IGNORE INTO attendance(employee_id,branch_id,date,check_in,check_out,status,worked_min,ot_min,source) VALUES(?,?,?,?,?,'present',?,?,'app')`, emp.ID, emp.Branch, ds, in.Unix(), out.Unix(), wm, max(0, wm-480))
				}
			}
		}
		// leave & overtime
		lt := recs(tx, "leave_types", 0)
		for i := 0; i < 6; i++ {
			emp := ids[fmt.Sprintf("e%d", i+2)]
			from := time.Now().AddDate(0, 0, 5+i*4)
			body := map[string]any{"employee": f(emp), "leave_type": f(lt[i%3].ID), "from": from.Format("2006-01-02"), "to": from.AddDate(0, 0, rng.Intn(2)).Format("2006-01-02"), "reason": "Family function", "status": "pending"}
			res := registry["leave_requests"]
			rec, err := a.buildRec(res, body, nil)
			if err != nil {
				continue
			}
			if err := leaveBefore(a, tx, &Claims{Role: RoleHRManager}, rec, nil); err != nil {
				continue
			}
			rec.CreatedBy = ids["u_hr_officer"]
			saveRec(tx, rec)
			leaveAfter(a, tx, &Claims{Role: RoleHRManager}, rec, nil)
			if i%3 == 1 {
				leaveApprove(a, tx, &Claims{Name: "HR Manager"}, rec, map[string]any{})
				rec.Status = "approved"
				saveRec(tx, rec)
			}
		}
		for i := 0; i < 5; i++ {
			st := []string{"approved", "approved", "pending", "approved", "rejected"}[i]
			mk(tx, "overtime", 0, map[string]any{"employee": f(ids[fmt.Sprintf("e%d", i)]), "date": time.Now().AddDate(0, 0, -3-i).Format("2006-01-02"), "hours": 2.0 + float64(i%3), "reason": "Weekend rush", "status": st})
		}
		// payroll for last month
		pm := time.Now().AddDate(0, -1, 0)
		from := time.Date(pm.Year(), pm.Month(), 1, 0, 0, 0, 0, time.Local)
		to := from.AddDate(0, 1, -1)
		per := mk(tx, "payroll_periods", 0, map[string]any{"name": from.Format("Jan 2006"), "from": from.Format("2006-01-02"), "to": to.Format("2006-01-02"), "pay_date": to.Format("2006-01-02"), "status": "open"})
		run := &Rec{Res: "payroll_runs", Status: "draft", Data: map[string]any{"name": "Payroll " + from.Format("Jan 2006"), "period": f(per)}}
		saveRec(tx, run)
		cl := &Claims{UID: ids["u_hr_manager"], Name: "HR Manager", Role: RoleHRManager}
		if err := calcPayroll(a, tx, cl, run, nil); err != nil {
			return err
		}
		saveRec(tx, run)
		tx.Exec(`UPDATE records SET status='approved' WHERE res='payslips' AND json_extract(data,'$.run')=?`, run.ID)
		run.Status = "approved"
		postPayroll(a, tx, &Claims{UID: ids["u_accounting_manager"]}, run, nil)
		payPayroll(a, tx, &Claims{UID: ids["u_accounting_manager"]}, run, map[string]any{})
		saveRec(tx, run)
		// current month run left as draft
		per2 := mk(tx, "payroll_periods", 0, map[string]any{"name": time.Now().Format("Jan 2006"), "from": time.Now().Format("2006-01") + "-01", "to": time.Now().AddDate(0, 1, -time.Now().Day()).Format("2006-01-02"), "status": "open"})
		mk(tx, "payroll_runs", 0, map[string]any{"name": "Payroll " + time.Now().Format("Jan 2006"), "period": f(per2), "status": "draft"})
		return nil
	})
	if err != nil {
		log.Println("seed HR:", err)
	}
}

func (a *App) seedExtras(ids map[string]int64, rng *rand.Rand) {
	err := a.tx(func(tx *sql.Tx) error {
		cl := &Claims{UID: ids["u_accountant"], Role: RoleAccountant}
		// suppliers: pay half of the first bills
		bills := recs(tx, "supplier_bills", 0)
		for i, b := range bills {
			if i%2 == 0 {
				a.payBill(tx, cl, b, b.I("amount")/2, "bank", today(), "seed")
			}
		}
		// reservations & waitlist
		tm := ids[fmt.Sprintf("t%d_%d", ids["main"], 3)]
		for i, nm := range []string{"Gupta family", "Birthday - Nair", "Corporate lunch"} {
			mk(tx, "reservations", ids["main"], map[string]any{"name": nm, "phone": fmt.Sprintf("98000%05d", i*111), "party": 4.0 + float64(i*2), "date": today(), "time": fmt.Sprintf("%02d:00", 19+i), "table": f(tm + int64(i)), "status": []string{"confirmed", "pending", "confirmed"}[i], "source": "phone"})
		}
		mk(tx, "waitlist", ids["main"], map[string]any{"name": "Walk-in Mehta", "phone": "9811111111", "party": 3.0, "quoted_min": 20.0, "status": "waiting"})
		// a few expense / bills
		mk(tx, "supplier_bills", ids["main"], map[string]any{"no": "RENT-" + time.Now().Format("0102"), "supplier": f(recs(tx, "suppliers", 0)[4].ID), "date": today(), "due": time.Now().AddDate(0, 0, 10).Format("2006-01-02"), "amount": 8000000.0, "paid": 0.0, "posted": true, "status": "open", "note": "Monthly rent"})
		a.postSimple(tx, ids["main"], today(), "bill:rent", "Rent", "rent", "ap", 8000000, 0)
		mk(tx, "petty_cash", ids["main"], map[string]any{"date": today(), "kind": "replenish", "amount": 500000.0, "description": "Top-up", "status": "posted"})
		a.postSimple(tx, ids["main"], today(), "petty:seed", "Petty cash top-up", "petty_cash", "cash", 500000, 0)
		mk(tx, "bank_recs", 0, map[string]any{"account": f(recs(tx, "fin_accounts", 0)[1].ID), "statement_date": today(), "statement_balance": 0.0, "book_balance": 0.0, "difference": 0.0, "status": "open"})
		mk(tx, "expense_claims", ids["main"], map[string]any{"employee": f(ids["e12"]), "date": today(), "category": "supplies", "description": "Cleaning supplies", "amount": 320000.0, "status": "submitted"})
		// a pending purchase order
		sup := recs(tx, "suppliers", 0)[0]
		var lines []any
		for _, r := range recs(tx, "ingredients", 0)[:5] {
			lines = append(lines, map[string]any{"ingredient": f(r.ID), "qty": 25.0, "cost": f(r.I("cost"))})
		}
		p := &Rec{Res: "purchases", Branch: ids["main"], Status: "ordered", Data: map[string]any{"supplier": f(sup.ID), "date": today(), "lines": lines, "tax": 0.0}}
		purchaseBefore(a, tx, cl, p, nil)
		p.Ref = p.S("no")
		saveRec(tx, p)
		// a notification or two
		return nil
	})
	if err != nil {
		log.Println("seed extras:", err)
	}
	a.notify(RoleManager, 0, "info", "Welcome to TableBill", "Demo data is loaded - explore every module from the left rail.", "#/dashboard")
	a.notify("", 0, "warn", "Tip", "Open Settings → Table QR ordering to try guest ordering from a phone.", "#/set:qr")
}
