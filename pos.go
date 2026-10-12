package main

import (
	"database/sql"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// =====================================================================
//  Orders: create / append / hold / pay / cancel / refund.
//  Prices, tax, discounts and stock are always computed on the server.
// =====================================================================

type Item struct {
	ID       int64   `json:"id"`
	OrderID  int64   `json:"order_id,omitempty"`
	Product  int64   `json:"product_id"`
	Name     string  `json:"name"`
	Category string  `json:"category,omitempty"`
	Station  string  `json:"station,omitempty"`
	Price    int64   `json:"price"`
	TaxPct   float64 `json:"tax_pct"`
	Qty      int64   `json:"qty"`
	Cost     int64   `json:"cost,omitempty"`
	Options  string  `json:"options,omitempty"`
	Note     string  `json:"note,omitempty"`
	Status   string  `json:"status"`
}

type Payment struct {
	ID      int64  `json:"id"`
	OrderID int64  `json:"order_id"`
	Method  string `json:"method"`
	Kind    string `json:"kind"`
	Amount  int64  `json:"amount"`
	Ref     string `json:"ref,omitempty"`
	At      int64  `json:"created_at"`
	By      string `json:"by,omitempty"`
}

type Order struct {
	ID          int64     `json:"id"`
	Branch      int64     `json:"branch_id"`
	No          string    `json:"no"`
	Channel     string    `json:"channel"`
	Source      string    `json:"source"`
	Status      string    `json:"status"`
	PayStatus   string    `json:"pay_status"`
	TableID     int64     `json:"table_id"`
	TableName   string    `json:"table_name"`
	CustomerID  int64     `json:"customer_id"`
	Customer    string    `json:"customer"`
	Phone       string    `json:"phone"`
	Address     string    `json:"address"`
	WaiterID    int64     `json:"waiter_id"`
	SessionID   int64     `json:"session_id"`
	Subtotal    int64     `json:"subtotal"`
	Discount    int64     `json:"discount"`
	Voucher     string    `json:"voucher"`
	DiscType    string    `json:"disc_type"`
	DiscValue   float64   `json:"disc_value"`
	Service     int64     `json:"service"`
	Tax         int64     `json:"tax"`
	Tip         int64     `json:"tip"`
	DeliveryFee int64     `json:"delivery_fee"`
	Total       int64     `json:"total"`
	Paid        int64     `json:"paid"`
	Cost        int64     `json:"cost"`
	PointsUsed  int64     `json:"points_used"`
	Note        string    `json:"note"`
	Reason      string    `json:"reason"`
	PayMode     string    `json:"pay_mode"`
	ScheduledAt int64     `json:"scheduled_at"`
	CreatedBy   int64     `json:"created_by"`
	Staff       string    `json:"staff,omitempty"`
	CreatedAt   int64     `json:"created_at"`
	UpdatedAt   int64     `json:"updated_at"`
	Items       []Item    `json:"items"`
	Payments    []Payment `json:"payments,omitempty"`
}

const orderCols = `o.id,o.branch_id,o.no,o.channel,o.source,o.status,o.pay_status,o.table_id,o.table_name,o.customer_id,o.customer,o.phone,o.address,
o.waiter_id,o.session_id,o.subtotal,o.discount,o.voucher,o.disc_type,o.disc_value,o.service,o.tax,o.tip,o.delivery_fee,o.total,o.paid,o.cost,o.points_used,
o.note,o.reason,o.pay_mode,o.scheduled_at,o.created_by,COALESCE(u.name,''),o.created_at,o.updated_at`
const orderFrom = ` FROM orders o LEFT JOIN users u ON u.id=o.created_by `

func scanOrder(s interface{ Scan(...any) error }) (*Order, error) {
	var o Order
	err := s.Scan(&o.ID, &o.Branch, &o.No, &o.Channel, &o.Source, &o.Status, &o.PayStatus, &o.TableID, &o.TableName, &o.CustomerID, &o.Customer, &o.Phone, &o.Address,
		&o.WaiterID, &o.SessionID, &o.Subtotal, &o.Discount, &o.Voucher, &o.DiscType, &o.DiscValue, &o.Service, &o.Tax, &o.Tip, &o.DeliveryFee, &o.Total, &o.Paid, &o.Cost, &o.PointsUsed,
		&o.Note, &o.Reason, &o.PayMode, &o.ScheduledAt, &o.CreatedBy, &o.Staff, &o.CreatedAt, &o.UpdatedAt)
	o.Items = []Item{}
	return &o, err
}

func loadOrder(q Querier, id int64, withPay bool) (*Order, error) {
	o, err := scanOrder(q.QueryRow(`SELECT `+orderCols+orderFrom+` WHERE o.id=?`, id))
	if err != nil {
		return nil, err
	}
	loadItemsFor(q, []*Order{o})
	if withPay {
		o.Payments = loadPayments(q, o.ID)
	}
	return o, nil
}

func loadPayments(q Querier, orderID int64) []Payment {
	out := []Payment{}
	rows, err := q.Query(`SELECT p.id,p.order_id,p.method,p.kind,p.amount,p.ref,p.created_at,COALESCE(u.name,'') FROM payments p LEFT JOIN users u ON u.id=p.created_by WHERE p.order_id=? ORDER BY p.id`, orderID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var p Payment
		rows.Scan(&p.ID, &p.OrderID, &p.Method, &p.Kind, &p.Amount, &p.Ref, &p.At, &p.By)
		out = append(out, p)
	}
	return out
}

func loadItemsFor(q Querier, orders []*Order) {
	if len(orders) == 0 {
		return
	}
	idx := map[int64]*Order{}
	ids := make([]string, len(orders))
	args := make([]any, len(orders))
	for i, o := range orders {
		idx[o.ID] = o
		ids[i] = "?"
		args[i] = o.ID
	}
	rows, err := q.Query(`SELECT id,order_id,product_id,name,category,station,price,tax_pct,qty,cost,options,note,status FROM order_items WHERE order_id IN (`+strings.Join(ids, ",")+`) ORDER BY id`, args...)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var it Item
		rows.Scan(&it.ID, &it.OrderID, &it.Product, &it.Name, &it.Category, &it.Station, &it.Price, &it.TaxPct, &it.Qty, &it.Cost, &it.Options, &it.Note, &it.Status)
		if o := idx[it.OrderID]; o != nil {
			o.Items = append(o.Items, it)
		}
	}
}

// ---------- input ----------

type ItemIn struct {
	Product int64  `json:"product_id"`
	Qty     int64  `json:"qty"`
	Note    string `json:"note"`
	Options []struct {
		Option int64  `json:"option"`
		Choice string `json:"choice"`
	} `json:"options"`
}

type OrderIn struct {
	OrderID     int64    `json:"order_id"` // append to this order
	BranchID    int64    `json:"branch_id"`
	Channel     string   `json:"channel"`
	TableID     int64    `json:"table_id"`
	CustomerID  int64    `json:"customer_id"`
	Customer    string   `json:"customer"`
	Phone       string   `json:"phone"`
	Address     string   `json:"address"`
	Note        string   `json:"note"`
	DiscType    string   `json:"disc_type"`
	DiscValue   float64  `json:"disc_value"`
	DiscountID  int64    `json:"discount_id"`
	Voucher     string   `json:"voucher"`
	Tip         int64    `json:"tip"`
	Hold        bool     `json:"hold"`
	Source      string   `json:"source"`
	Zone        int64    `json:"zone"`
	WaiterID    int64    `json:"waiter_id"`
	ScheduledAt int64    `json:"scheduled_at"`
	Items       []ItemIn `json:"items"`
	Replace     bool     `json:"replace"` // replace items of a held order
	PayMode     string   `json:"pay_mode"`
}

var channels = map[string]bool{"dine_in": true, "takeaway": true, "delivery": true, "pickup": true, "drive_thru": true, "pre_order": true, "catering": true}

var branchCodeCache = struct {
	ver int64
	m   map[int64]string
}{ver: -1}

func (a *App) branchCode(q Querier, id int64) string {
	v := verOf("branches")
	ingMu.Lock()
	defer ingMu.Unlock()
	if branchCodeCache.ver != v {
		branchCodeCache.m = map[int64]string{}
		branchCodeCache.ver = v
		for _, r := range recs(q, "branches", 0) {
			code := r.Ref
			if code == "" {
				code = "B" + strconv.FormatInt(r.ID, 10)
			}
			branchCodeCache.m[r.ID] = code
		}
	}
	if c := branchCodeCache.m[id]; c != "" {
		return c
	}
	return "B" + strconv.FormatInt(id, 10)
}

func (a *App) nextOrderNo(tx Querier, branch int64) string {
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Unix()
	var n int64
	tx.QueryRow(`SELECT COUNT(*) FROM orders WHERE branch_id=? AND created_at>=?`, branch, start).Scan(&n)
	return fmt.Sprintf("%s-%s-%03d", a.branchCode(tx, branch), now.Format("0102"), n+1)
}

// buildItems prices each line from the server-side menu (never trusts the client).
func (a *App) buildItems(snap *menuSnap, ins []ItemIn) ([]Item, error) {
	if len(ins) == 0 || len(ins) > 150 {
		return nil, uerr("add at least one item")
	}
	items := make([]Item, 0, len(ins))
	for _, in := range ins {
		p := snap.products[in.Product]
		if p == nil || !p.Available {
			return nil, uerr("an item in the cart is no longer available")
		}
		if in.Qty < 1 || in.Qty > 99 {
			return nil, uerr("quantity must be between 1 and 99")
		}
		price := p.Price
		var names []string
		chosen := map[int64]int{}
		for _, o := range in.Options {
			opt := snap.options[o.Option]
			if opt == nil {
				continue
			}
			for _, ch := range opt.Choices {
				if ch.Name == o.Choice {
					price += ch.Price
					label := ch.Name
					if ch.Price != 0 {
						label += fmt.Sprintf(" (+%.2f)", float64(ch.Price)/100)
					}
					names = append(names, label)
					chosen[opt.ID]++
				}
			}
		}
		for _, oid := range p.Options {
			if opt := snap.options[oid]; opt != nil && opt.Required && chosen[oid] == 0 {
				return nil, uerr("please choose %s for %s", opt.Name, p.Name)
			}
		}
		items = append(items, Item{Product: p.ID, Name: p.Name, Category: p.CatName, Station: p.Station, Price: price, TaxPct: p.TaxRate,
			Qty: in.Qty, Cost: p.Cost, Options: strings.Join(names, ", "), Note: trunc(in.Note, 200), Status: "pending"})
	}
	return items, nil
}

func round(f float64) int64 { return int64(math.Round(f)) }

// discountAmount = manual discount (percent/fixed) + valid voucher.
func (a *App) discountAmount(q Querier, o *Order, subtotal int64, c *Claims) (int64, error) {
	var d int64
	switch o.DiscType {
	case "percent":
		pct := o.DiscValue
		if pct < 0 {
			pct = 0
		}
		d = round(float64(subtotal) * math.Min(pct, 100) / 100)
	case "fixed":
		d = int64(math.Max(0, o.DiscValue))
	}
	if d > 0 && c != nil && a.level(c.Role, "orders") < LvApprove && c.Role != RoleAdmin {
		if !a.settingBool("pos", "allow_discount", true) {
			return 0, uerr("discounts are turned off")
		}
		maxPct := a.settingNum("pos", "max_discount_pct", 20)
		if subtotal > 0 && float64(d)*100/float64(subtotal) > maxPct+0.001 {
			return 0, uerr("you can give at most %.0f%% discount - ask a manager", maxPct)
		}
	}
	if o.Voucher != "" {
		v, err := a.checkVoucher(q, o.Voucher, subtotal)
		if err != nil {
			return 0, err
		}
		if v.S("type") == "percent" {
			d += round(float64(subtotal) * v.N("value") / 100)
		} else {
			d += int64(v.N("value"))
		}
	}
	if d > subtotal {
		d = subtotal
	}
	return d, nil
}

func (a *App) checkVoucher(q Querier, code string, subtotal int64) (*Rec, error) {
	var id int64
	if q.QueryRow(`SELECT id FROM records WHERE res='vouchers' AND ref=?`, code).Scan(&id) != nil {
		return nil, uerr("voucher %q does not exist", code)
	}
	v, _ := loadRec(q, id)
	t := today()
	switch {
	case v.Status != "active":
		return nil, uerr("voucher %q is not active", code)
	case v.S("valid_from") != "" && v.S("valid_from") > t, v.S("valid_to") != "" && v.S("valid_to") < t:
		return nil, uerr("voucher %q is not valid today", code)
	case v.I("max_uses") > 0 && v.I("used") >= v.I("max_uses"):
		return nil, uerr("voucher %q has been fully used", code)
	case subtotal < v.I("min_order"):
		return nil, uerr("voucher %q needs a minimum order of %.2f", code, float64(v.I("min_order"))/100)
	}
	return v, nil
}

// recalc recomputes every money field of an order from its items.
func (a *App) recalc(tx Querier, id int64, c *Claims) (*Order, error) {
	o, err := loadOrder(tx, id, false)
	if err != nil {
		return nil, err
	}
	inclusive := a.settingBool("pos", "tax_inclusive", false)
	var sub int64
	var rawTax, cost float64
	for _, it := range o.Items {
		if it.Status == "void" {
			continue
		}
		gross := float64(it.Price * it.Qty)
		sub += it.Price * it.Qty
		cost += float64(it.Cost * it.Qty)
		if inclusive {
			rawTax += gross - gross/(1+it.TaxPct/100)
		} else {
			rawTax += gross * it.TaxPct / 100
		}
	}
	disc, err := a.discountAmount(tx, o, sub, c)
	if err != nil {
		return nil, err
	}
	f := 1.0
	if sub > 0 {
		f = float64(sub-disc) / float64(sub)
	}
	tax := round(rawTax * f)
	net := sub - disc
	var service int64
	if o.Channel == "dine_in" {
		service = round(float64(net) * a.settingNum("pos", "service_charge_pct", 0) / 100)
	}
	fee := o.DeliveryFee
	total := net + service + o.Tip + fee
	if !inclusive {
		total += tax
	}
	if o.Channel != "delivery" {
		fee, total = 0, total-o.DeliveryFee
	}
	o.Subtotal, o.Discount, o.Tax, o.Service, o.Total, o.Cost, o.DeliveryFee = sub, disc, tax, service, total, round(cost), fee
	ps := "unpaid"
	if o.Paid > 0 {
		ps = "partial"
	}
	if o.Paid >= total && total > 0 {
		ps = "paid"
	}
	if o.PayStatus == "refunded" {
		ps = "refunded"
	}
	o.PayStatus = ps
	_, err = tx.Exec(`UPDATE orders SET subtotal=?,discount=?,tax=?,service=?,total=?,cost=?,delivery_fee=?,pay_status=?,tip=?,updated_at=? WHERE id=?`,
		o.Subtotal, o.Discount, o.Tax, o.Service, o.Total, o.Cost, o.DeliveryFee, o.PayStatus, o.Tip, nowUnix(), o.ID)
	return o, err
}

// ---------- stock ----------

func (a *App) deductStock(tx Querier, snap *menuSnap, branch, orderID int64, items []Item, uid int64) error {
	allowNeg := a.settingBool("pos", "allow_negative_stock", true)
	need := map[int64]float64{}
	for _, it := range items {
		if it.Status == "void" {
			continue
		}
		for _, l := range snap.recipes[it.Product] {
			need[l.Ingredient] += l.Qty * float64(it.Qty)
		}
	}
	ref := fmt.Sprintf("order:%d", orderID)
	for ing, q := range need {
		if !allowNeg {
			var have float64
			tx.QueryRow(`SELECT qty FROM ingredients_stock WHERE branch_id=? AND ingredient_id=?`, branch, ing).Scan(&have)
			if have < q {
				return uerr("not enough stock of %s", a.ing(tx, ing).Name)
			}
		}
		if err := a.stockMove(tx, branch, ing, -q, a.ingCost(tx, ing), "sale", ref, "", uid); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) restoreStock(tx Querier, branch, orderID int64, uid int64, note string) {
	ref := fmt.Sprintf("order:%d", orderID)
	rows, err := tx.Query(`SELECT ingredient_id,SUM(qty) FROM stock_moves WHERE ref=? AND kind IN ('sale','return') GROUP BY ingredient_id`, ref)
	if err != nil {
		return
	}
	type m struct {
		ing int64
		qty float64
	}
	var ms []m
	for rows.Next() {
		var x m
		rows.Scan(&x.ing, &x.qty)
		ms = append(ms, x)
	}
	rows.Close()
	for _, x := range ms {
		if x.qty < 0 { // net consumed
			a.stockMove(tx, branch, x.ing, -x.qty, a.ingCost(tx, x.ing), "return", ref, note, uid)
		}
	}
}

// ---------- create / append ----------

func (a *App) placeOrder(tx *sql.Tx, c *Claims, in *OrderIn) (*Order, error) {
	snap := a.menu()
	if !channels[in.Channel] {
		in.Channel = "dine_in"
	}
	branch := c.Branch
	if branch == 0 {
		branch = in.BranchID
	}
	if branch == 0 && in.TableID > 0 { // head-office users: the table decides the branch
		tx.QueryRow(`SELECT branch_id FROM records WHERE id=? AND res='tables'`, in.TableID).Scan(&branch)
	}
	if branch == 0 {
		branch = a.defaultBranch(tx)
	}
	items, err := a.buildItems(snap, in.Items)
	if err != nil {
		return nil, err
	}

	var o *Order
	if in.OrderID > 0 {
		if o, err = loadOrder(tx, in.OrderID, false); err != nil || (c.Branch != 0 && o.Branch != c.Branch) {
			return nil, uerr("order not found")
		}
	} else if in.Channel == "dine_in" && in.TableID > 0 && !in.Hold {
		// a table can only have one open bill: add to it
		var oid int64
		if tx.QueryRow(`SELECT id FROM orders WHERE branch_id=? AND table_id=? AND status IN ('pending','preparing','ready','served') AND pay_status<>'paid' ORDER BY id DESC LIMIT 1`, branch, in.TableID).Scan(&oid) == nil {
			o, _ = loadOrder(tx, oid, false)
		}
	}
	now := nowUnix()
	if o != nil { // ----- append to an existing order -----
		if a.ownOrdersOnly(c) && o.CreatedBy != c.UID {
			return nil, uerr("this table already has a bill taken by someone else - ask the waiter or cashier to add items")
		}
		switch o.Status {
		case "completed", "cancelled", "refunded":
			return nil, uerr("this order is already %s", o.Status)
		}
		if o.Status == "held" && in.Replace {
			tx.Exec(`DELETE FROM order_items WHERE order_id=?`, o.ID)
		}
		for _, it := range items {
			tx.Exec(`INSERT INTO order_items(order_id,product_id,name,category,station,price,tax_pct,qty,cost,options,note,status) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
				o.ID, it.Product, it.Name, it.Category, it.Station, it.Price, it.TaxPct, it.Qty, it.Cost, it.Options, it.Note, it.Status)
		}
		if in.Voucher != "" {
			o.Voucher = in.Voucher
		}
		if in.DiscType != "" {
			tx.Exec(`UPDATE orders SET disc_type=?,disc_value=? WHERE id=?`, in.DiscType, in.DiscValue, o.ID)
		}
		if o.Status == "held" && !in.Hold {
			tx.Exec(`UPDATE orders SET status='pending' WHERE id=?`, o.ID)
			o.Status = "pending"
			all, _ := loadOrder(tx, o.ID, false)
			if err := a.deductStock(tx, snap, branch, o.ID, all.Items, c.UID); err != nil {
				return nil, err
			}
			a.bumpVoucher(tx, o.Voucher, 1)
		} else if o.Status != "held" {
			if err := a.deductStock(tx, snap, branch, o.ID, items, c.UID); err != nil {
				return nil, err
			}
			// new food: bring the order back to the kitchen
			if o.Status == "ready" || o.Status == "served" || o.Status == "preparing" {
				tx.Exec(`UPDATE orders SET status=CASE WHEN status='preparing' THEN 'preparing' ELSE 'pending' END WHERE id=?`, o.ID)
			}
		}
		tx.Exec(`UPDATE orders SET tip=CASE WHEN ?>0 THEN ? ELSE tip END WHERE id=?`, in.Tip, in.Tip, o.ID)
		if pm := cleanPayMode(in.PayMode); pm != "" {
			tx.Exec(`UPDATE orders SET pay_mode=? WHERE id=?`, pm, o.ID)
		}
		return a.recalc(tx, o.ID, c)
	}

	// ----- new order -----
	status := "pending"
	if in.Hold {
		status = "held"
	}
	if in.Source == "" {
		in.Source = "pos"
	}
	var tname string
	if in.Channel == "dine_in" {
		if in.TableID == 0 {
			return nil, uerr("please choose a table")
		}
		tx.QueryRow(`SELECT json_extract(data,'$.name') FROM records WHERE id=? AND res='tables'`, in.TableID).Scan(&tname)
		if tname == "" {
			return nil, uerr("table not found")
		}
	} else {
		in.TableID = 0
	}
	if in.Channel == "delivery" && (strings.TrimSpace(in.Address) == "" || strings.TrimSpace(in.Phone) == "") && a.settingBool("pos", "require_customer_for_delivery", true) && in.CustomerID == 0 {
		return nil, uerr("delivery orders need a customer phone and address")
	}
	if in.CustomerID > 0 {
		if cu, err := loadRec(tx, in.CustomerID); err == nil && cu.Res == "customers" {
			if in.Customer == "" {
				in.Customer = cu.S("name")
			}
			if in.Phone == "" {
				in.Phone = cu.S("phone")
			}
			if in.Address == "" {
				in.Address = cu.S("address")
			}
		}
	}
	voucher := strings.TrimSpace(in.Voucher)
	res, err := tx.Exec(`INSERT INTO orders(branch_id,no,channel,source,status,table_id,table_name,customer_id,customer,phone,address,waiter_id,session_id,voucher,disc_type,disc_value,tip,note,pay_mode,scheduled_at,created_by,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		branch, a.nextOrderNo(tx, branch), in.Channel, in.Source, status, in.TableID, tname, in.CustomerID, trunc(in.Customer, 100), trunc(in.Phone, 30), trunc(in.Address, 300),
		firstNonZero(in.WaiterID, c.UID), a.openSession(tx, c.UID, branch), voucher, in.DiscType, in.DiscValue, in.Tip, trunc(in.Note, 300), cleanPayMode(in.PayMode), in.ScheduledAt, c.UID, now, now)
	if err != nil {
		return nil, err
	}
	oid, _ := res.LastInsertId()
	for _, it := range items {
		tx.Exec(`INSERT INTO order_items(order_id,product_id,name,category,station,price,tax_pct,qty,cost,options,note,status) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			oid, it.Product, it.Name, it.Category, it.Station, it.Price, it.TaxPct, it.Qty, it.Cost, it.Options, it.Note, it.Status)
	}
	if in.Channel == "delivery" {
		var sub int64
		for _, it := range items {
			sub += it.Price * it.Qty
		}
		tx.Exec(`UPDATE orders SET delivery_fee=? WHERE id=?`, a.deliveryFee(tx, branch, sub, in.Zone), oid)
	}
	ord, err := a.recalc(tx, oid, c)
	if err != nil {
		return nil, err
	}
	if in.Channel == "delivery" {
		a.createDelivery(tx, ord, in.Zone)
	}
	if !in.Hold {
		if err := a.deductStock(tx, snap, branch, oid, items, c.UID); err != nil {
			return nil, err
		}
		if in.TableID > 0 {
			setTableStatus(tx, in.TableID, "occupied", true)
		}
		a.bumpVoucher(tx, voucher, 1)
	}
	return ord, nil
}

var payModes = map[string]bool{"cash": true, "card": true, "upi": true, "wallet": true, "other": true}

func cleanPayMode(s string) string {
	if payModes[s] {
		return s
	}
	return ""
}

func firstNonZero(vs ...int64) int64 {
	for _, v := range vs {
		if v != 0 {
			return v
		}
	}
	return 0
}

func (a *App) bumpVoucher(tx Querier, code string, d int64) {
	if code != "" {
		tx.Exec(`UPDATE records SET data=json_set(data,'$.used',COALESCE(json_extract(data,'$.used'),0)+?) WHERE res='vouchers' AND ref=?`, d, code)
	}
}

func (a *App) openSession(q Querier, uid, branch int64) int64 {
	var id int64
	q.QueryRow(`SELECT id FROM records WHERE res='pos_sessions' AND status='open' AND json_extract(data,'$.user')=? AND branch_id=? ORDER BY id DESC LIMIT 1`, uid, branch).Scan(&id)
	return id
}

// freeTableIfDone makes a table "cleaning" once its last open bill is closed.
func (a *App) freeTableIfDone(tx Querier, tableID, branch int64) {
	if tableID <= 0 {
		return
	}
	var n int
	tx.QueryRow(`SELECT COUNT(*) FROM orders WHERE table_id=? AND branch_id=? AND status IN ('held','pending','preparing','ready','served') AND status<>'held'`, tableID, branch).Scan(&n)
	if n == 0 {
		setTableStatus(tx, tableID, "cleaning", false)
		tx.Exec(`UPDATE records SET status='cleaning' WHERE id=? AND res='tables' AND status='occupied'`, tableID)
	}
}

// ---------- handlers ----------

func (a *App) handleCreateOrder(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	var in OrderIn
	if !readJSON(w, r, &in) {
		return
	}
	var out *Order
	err := a.tx(func(tx *sql.Tx) (err error) {
		out, err = a.placeOrder(tx, c, &in)
		return
	})
	if err != nil {
		writeErr(w, 400, friendlyErr(err))
		return
	}
	a.hub.PublishBranch(out.Branch, "order")
	a.audit(r, "orders", out.ID, "create", out.No)
	writeJSON(w, 201, out)
}

func (a *App) handleGetOrder(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	o, err := loadOrder(a.db.rdb, id, true)
	if err != nil || (c.Branch != 0 && o.Branch != c.Branch) || (a.ownOrdersOnly(c) && o.CreatedBy != c.UID) {
		writeErr(w, 404, "order not found")
		return
	}
	writeJSON(w, 200, o)
}

// ---------- status flow ----------

var statusFlow = map[string][]string{
	"pending":   {"preparing", "ready"},
	"preparing": {"ready"},
	"ready":     {"served"},
	"served":    {"completed"},
}

func (a *App) handleOrderStatus(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var in struct{ Status string }
	if !readJSON(w, r, &in) {
		return
	}
	var out *Order
	err := a.tx(func(tx *sql.Tx) error {
		o, err := loadOrder(tx, id, false)
		if err != nil || (c.Branch != 0 && o.Branch != c.Branch) {
			return uerr("order not found")
		}
		ok := false
		for _, s := range statusFlow[o.Status] {
			ok = ok || s == in.Status
		}
		if !ok {
			return uerr("this order is %s", o.Status)
		}
		switch in.Status {
		case "preparing", "ready":
			if a.level(c.Role, "kitchen") < LvManage && a.level(c.Role, "pos") < LvManage {
				return uerr("your role cannot do this step")
			}
		case "served":
			if a.level(c.Role, "pos") < LvManage && a.level(c.Role, "kitchen") < LvManage {
				return uerr("your role cannot do this step")
			}
		case "completed":
			if a.level(c.Role, "orders") < LvManage {
				return uerr("your role cannot do this step")
			}
			if o.PayStatus != "paid" {
				return uerr("take payment first")
			}
		}
		itemSt := map[string]string{"preparing": "preparing", "ready": "ready", "served": "served"}[in.Status]
		if itemSt != "" {
			tx.Exec(`UPDATE order_items SET status=? WHERE order_id=? AND status<>'void'`, itemSt, id)
		}
		st := in.Status
		if st == "served" && o.PayStatus == "paid" {
			st = "completed"
		}
		tx.Exec(`UPDATE orders SET status=?,updated_at=? WHERE id=?`, st, nowUnix(), id)
		if st == "ready" {
			a.orderReady(tx, id)
		}
		if st == "completed" {
			a.freeTableIfDone(tx, o.TableID, o.Branch)
			a.completeDelivery(tx, o)
		}
		out, _ = loadOrder(tx, id, false)
		return nil
	})
	if err != nil {
		writeErr(w, 400, friendlyErr(err))
		return
	}
	a.hub.PublishBranch(out.Branch, "order")
	writeJSON(w, 200, out)
}

// orderReady tells the waiter (and the other waiters of the branch) that food is ready:
// a live pop-up on their POS screen plus a bell notification. Only the order number is sent.
func (a *App) orderReady(q Querier, oid int64) {
	var branch, creator, waiter int64
	var no, table, channel string
	if q.QueryRow(`SELECT branch_id,no,table_name,channel,created_by,waiter_id FROM orders WHERE id=?`, oid).Scan(&branch, &no, &table, &channel, &creator, &waiter) != nil {
		return
	}
	where := table
	if where == "" {
		where = niceLabel(channel)
	}
	short := no
	if i := strings.LastIndex(no, "-"); i >= 0 {
		short = no[i+1:]
	}
	owner := firstNonZero(waiter, creator)
	a.hub.PublishBranch(branch, fmt.Sprintf("ready|%s|%s|%d|%d", short, where, creator, waiter))
	if a.settingBool("notifications", "order_ready", true) {
		a.notifyUser(owner, "info", "Order #"+short+" is ready", where, "#/pos")
		a.notify(RoleWaiter, branch, "info", "Order #"+short+" is ready", where, "#/pos")
	}
}

// per-item kitchen status (KDS)
func (a *App) handleItemStatus(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var in struct{ Status string }
	if !readJSON(w, r, &in) {
		return
	}
	if in.Status != "preparing" && in.Status != "ready" && in.Status != "served" {
		writeErr(w, 400, "invalid status")
		return
	}
	var branch int64
	err := a.tx(func(tx *sql.Tx) error {
		var oid int64
		var cur string
		if tx.QueryRow(`SELECT order_id,status FROM order_items WHERE id=?`, id).Scan(&oid, &cur) != nil || cur == "void" {
			return uerr("item not found")
		}
		o, _ := loadOrder(tx, oid, false)
		if c.Branch != 0 && o.Branch != c.Branch {
			return uerr("item not found")
		}
		branch = o.Branch
		tx.Exec(`UPDATE order_items SET status=? WHERE id=?`, in.Status, id)
		a.syncOrderStatus(tx, oid)
		return nil
	})
	if err != nil {
		writeErr(w, 400, friendlyErr(err))
		return
	}
	a.hub.PublishBranch(branch, "order")
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// syncOrderStatus derives the order status from its items.
func (a *App) syncOrderStatus(tx Querier, oid int64) {
	rows, _ := tx.Query(`SELECT status FROM order_items WHERE order_id=? AND status<>'void'`, oid)
	cnt := map[string]int{}
	total := 0
	for rows.Next() {
		var s string
		rows.Scan(&s)
		cnt[s]++
		total++
	}
	rows.Close()
	var cur string
	var paid string
	var tid, branch int64
	tx.QueryRow(`SELECT status,pay_status,table_id,branch_id FROM orders WHERE id=?`, oid).Scan(&cur, &paid, &tid, &branch)
	if total == 0 || cur == "held" || cur == "completed" || cur == "cancelled" || cur == "refunded" {
		return
	}
	next := cur
	switch {
	case cnt["served"] == total:
		next = "served"
	case cnt["ready"]+cnt["served"] == total && a.settingBool("kitchen", "auto_ready", true):
		next = "ready"
	case cnt["preparing"]+cnt["ready"]+cnt["served"] > 0:
		next = "preparing"
	default:
		next = "pending"
	}
	if next == "served" && paid == "paid" {
		next = "completed"
		a.freeTableIfDone(tx, tid, branch)
	}
	if next != cur {
		tx.Exec(`UPDATE orders SET status=?,updated_at=? WHERE id=?`, next, nowUnix(), oid)
		if next == "ready" {
			a.orderReady(tx, oid)
		}
	}
}

// ---------- payment ----------

var payMethods = map[string]string{"cash": "cash", "card": "card_clearing", "upi": "bank", "wallet": "bank", "bank_transfer": "bank",
	"cheque": "bank", "gift_card": "gift_liability", "loyalty": "loyalty_expense", "cod": "ar", "account": "ar", "other": "bank"}

type payIn struct {
	Method string `json:"method"`
	Amount int64  `json:"amount"`
	Ref    string `json:"ref"`
	Points int64  `json:"points"`
}

func (a *App) handlePay(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var in struct {
		Payments []payIn `json:"payments"`
		Tip      *int64  `json:"tip"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	var out *Order
	var change int64
	err := a.tx(func(tx *sql.Tx) error {
		o, err := loadOrder(tx, id, false)
		if err != nil || (c.Branch != 0 && o.Branch != c.Branch) {
			return uerr("order not found")
		}
		switch o.Status {
		case "completed", "cancelled", "refunded":
			return uerr("this order is already %s", o.Status)
		}
		if len(in.Payments) == 0 {
			return uerr("enter a payment")
		}
		snap := a.menu()
		if o.Status == "held" { // "Pay & Fire": paying sends it to the kitchen
			tx.Exec(`UPDATE orders SET status='pending' WHERE id=?`, id)
			if err := a.deductStock(tx, snap, o.Branch, id, o.Items, c.UID); err != nil {
				return err
			}
			a.bumpVoucher(tx, o.Voucher, 1)
			if o.TableID > 0 {
				setTableStatus(tx, o.TableID, "occupied", true)
			}
			o.Status = "pending"
		}
		if in.Tip != nil && *in.Tip >= 0 && *in.Tip != o.Tip {
			tx.Exec(`UPDATE orders SET tip=? WHERE id=?`, *in.Tip, id)
		}
		if o, err = a.recalc(tx, id, c); err != nil {
			return err
		}
		session := a.openSession(tx, c.UID, o.Branch)
		remaining := o.Total - o.Paid
		if remaining <= 0 {
			return uerr("this order is already fully paid")
		}
		var cust *Rec
		if o.CustomerID > 0 {
			cust, _ = loadRec(tx, o.CustomerID)
		}
		var pointsUsed int64
		for _, p := range in.Payments {
			if _, ok := payMethods[p.Method]; !ok {
				return uerr("unknown payment method %q", p.Method)
			}
			if remaining <= 0 {
				break
			}
			amt := p.Amount
			ref := trunc(p.Ref, 60)
			switch p.Method {
			case "gift_card":
				var gid int64
				if tx.QueryRow(`SELECT id FROM records WHERE res='gift_cards' AND ref=?`, strings.TrimSpace(p.Ref)).Scan(&gid) != nil {
					return uerr("gift card not found")
				}
				g, _ := loadRec(tx, gid)
				if g.Status != "active" || (g.S("expires") != "" && g.S("expires") < today()) {
					return uerr("this gift card is not active")
				}
				if amt <= 0 || amt > g.I("balance") {
					amt = g.I("balance")
				}
				if amt > remaining {
					amt = remaining
				}
				g.Set("balance", float64(g.I("balance")-amt))
				if g.I("balance") == 0 {
					g.Status = "used"
				}
				saveRec(tx, g)
				mk(tx, "gift_txns", o.Branch, map[string]any{"card": float64(gid), "kind": "redeem", "amount": float64(amt), "order": float64(id), "note": "Order " + o.No})
			case "loyalty":
				if cust == nil {
					return uerr("choose a customer to use loyalty points")
				}
				pv := a.pointValue(tx)
				pts := p.Points
				if pts <= 0 || pts > cust.I("points") {
					pts = cust.I("points")
				}
				if min := a.minRedeem(tx); pts < min {
					return uerr("you need at least %d points to redeem", min)
				}
				amt = pts * pv
				if amt > remaining {
					amt = remaining
					pts = (amt + pv - 1) / pv
				}
				pointsUsed += pts
				cust.Set("points", float64(cust.I("points")-pts))
				ref = fmt.Sprintf("%d pts", pts)
			default:
				if amt <= 0 {
					return uerr("enter an amount")
				}
				if amt > remaining {
					if p.Method != "cash" {
						return uerr("the amount is more than what is due")
					}
					change += amt - remaining
					amt = remaining
				}
			}
			remaining -= amt
			if _, err := tx.Exec(`INSERT INTO payments(order_id,branch_id,session_id,method,kind,amount,ref,created_by,created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
				id, o.Branch, session, p.Method, "payment", amt, ref, c.UID, nowUnix()); err != nil {
				return err
			}
			o.Paid += amt
		}
		tx.Exec(`UPDATE orders SET paid=?,points_used=points_used+?,updated_at=? WHERE id=?`, o.Paid, pointsUsed, nowUnix(), id)
		ord, err := a.recalc(tx, id, c)
		if err != nil {
			return err
		}
		if ord.PayStatus == "paid" {
			if ord.Status == "served" {
				tx.Exec(`UPDATE orders SET status='completed' WHERE id=?`, id)
				a.freeTableIfDone(tx, ord.TableID, ord.Branch)
				ord.Status = "completed"
			}
			if err := a.onFullyPaid(tx, c, ord, cust); err != nil {
				return err
			}
		} else if cust != nil && pointsUsed > 0 {
			saveRec(tx, cust)
		}
		out, _ = loadOrder(tx, id, true)
		return nil
	})
	if err != nil {
		writeErr(w, 400, friendlyErr(err))
		return
	}
	a.hub.PublishBranch(out.Branch, "order")
	a.audit(r, "orders", id, "pay", out.No)
	writeJSON(w, 200, map[string]any{"order": out, "change": change})
}

func (a *App) pointValue(q Querier) int64 {
	for _, p := range recs(q, "loyalty_programs", 0) {
		if p.Status == "active" {
			return p.I("point_value")
		}
	}
	return int64(a.settingNum("pos", "loyalty_value", 1) * 100)
}

func (a *App) minRedeem(q Querier) int64 {
	for _, p := range recs(q, "loyalty_programs", 0) {
		if p.Status == "active" {
			return p.I("min_redeem")
		}
	}
	return 0
}

// onFullyPaid: customer stats / loyalty points and the accounting entry.
func (a *App) onFullyPaid(tx *sql.Tx, c *Claims, o *Order, cust *Rec) error {
	if cust != nil {
		earnRate := a.settingNum("pos", "loyalty_earn", 5)
		var tiers []*Rec
		for _, p := range recs(tx, "loyalty_programs", 0) {
			if p.Status == "active" {
				earnRate = p.N("earn_rate")
				break
			}
		}
		tiers = recs(tx, "loyalty_tiers", 0)
		spent := cust.I("spent") + o.Total
		mult, tierName := 1.0, ""
		var best int64 = -1
		for _, t := range tiers {
			if t.I("min_spend") <= spent && t.I("min_spend") > best {
				best, mult, tierName = t.I("min_spend"), t.N("multiplier"), t.S("name")
			}
		}
		earned := int64(math.Floor(float64(o.Total-o.Tip) / 10000 * earnRate * mult))
		cust.Set("points", float64(cust.I("points")+earned))
		cust.Set("spent", float64(spent))
		cust.Set("visits", float64(cust.I("visits")+1))
		cust.Set("tier", tierName)
		if err := saveRec(tx, cust); err != nil {
			return err
		}
	}
	if a.autoPost() {
		a.postSaleSafe(tx, c, o)
	}
	return nil
}

// ---------- cancel / refund / void ----------

func (a *App) handleCancel(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var in struct{ Reason string }
	if !readJSON(w, r, &in) {
		return
	}
	var out *Order
	err := a.tx(func(tx *sql.Tx) error {
		o, err := loadOrder(tx, id, false)
		if err != nil || (c.Branch != 0 && o.Branch != c.Branch) || (a.ownOrdersOnly(c) && o.CreatedBy != c.UID) {
			return uerr("order not found")
		}
		switch o.Status {
		case "completed", "cancelled", "refunded":
			return uerr("this order is already %s", o.Status)
		}
		if o.Paid > 0 {
			return uerr("this order has payments - refund it instead")
		}
		if (o.Status == "preparing" || o.Status == "ready" || o.Status == "served") && a.level(c.Role, "orders") < LvApprove && c.Role != RoleSuperAdmin {
			return uerr("food is already being prepared - ask a manager to cancel")
		}
		if o.Status != "held" {
			a.restoreStock(tx, o.Branch, id, c.UID, "cancelled "+o.No)
			a.bumpVoucher(tx, o.Voucher, -1)
		}
		tx.Exec(`UPDATE order_items SET status='void' WHERE order_id=?`, id)
		tx.Exec(`UPDATE orders SET status='cancelled',reason=?,updated_at=? WHERE id=?`, trunc(in.Reason, 200), nowUnix(), id)
		a.freeTableIfDone(tx, o.TableID, o.Branch)
		a.cancelDelivery(tx, o.ID)
		out, _ = loadOrder(tx, id, false)
		return nil
	})
	if err != nil {
		writeErr(w, 400, friendlyErr(err))
		return
	}
	a.hub.PublishBranch(out.Branch, "order")
	a.audit(r, "orders", id, "cancel", out.No+" "+in.Reason)
	writeJSON(w, 200, out)
}

func (a *App) handleRefund(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var in struct{ Reason string }
	if !readJSON(w, r, &in) {
		return
	}
	var out *Order
	err := a.tx(func(tx *sql.Tx) error {
		o, err := loadOrder(tx, id, true)
		if err != nil || (c.Branch != 0 && o.Branch != c.Branch) {
			return uerr("order not found")
		}
		if o.Paid == 0 || o.PayStatus == "refunded" || o.Status == "cancelled" {
			return uerr("nothing to refund on this order")
		}
		session := a.openSession(tx, c.UID, o.Branch)
		for _, p := range o.Payments {
			if p.Kind != "payment" {
				continue
			}
			tx.Exec(`INSERT INTO payments(order_id,branch_id,session_id,method,kind,amount,ref,created_by,created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
				id, o.Branch, session, p.Method, "refund", p.Amount, "refund", c.UID, nowUnix())
			if p.Method == "gift_card" {
				var gid int64
				if tx.QueryRow(`SELECT id FROM records WHERE res='gift_cards' AND ref=?`, p.Ref).Scan(&gid) == nil {
					if g, err := loadRec(tx, gid); err == nil {
						g.Set("balance", float64(g.I("balance")+p.Amount))
						if g.Status == "used" {
							g.Status = "active"
						}
						saveRec(tx, g)
						mk(tx, "gift_txns", o.Branch, map[string]any{"card": float64(gid), "kind": "refund", "amount": float64(p.Amount), "order": float64(id), "note": "Refund " + o.No})
					}
				}
			}
		}
		if o.Status == "pending" || o.Status == "preparing" || o.Status == "ready" {
			a.restoreStock(tx, o.Branch, id, c.UID, "refund "+o.No)
		}
		a.bumpVoucher(tx, o.Voucher, -1)
		if o.CustomerID > 0 {
			if cu, err := loadRec(tx, o.CustomerID); err == nil {
				cu.Set("spent", float64(max(0, cu.I("spent")-o.Total)))
				cu.Set("visits", float64(max(0, cu.I("visits")-1)))
				cu.Set("points", float64(cu.I("points")+o.PointsUsed))
				saveRec(tx, cu)
			}
		}
		// mirror the sale journal
		var jid int64
		if tx.QueryRow(`SELECT id FROM journals WHERE source=? AND status='posted' ORDER BY id DESC LIMIT 1`, fmt.Sprintf("order:%d", id)).Scan(&jid) == nil {
			if err := a.reverseJournal(tx, jid, c.UID, "Refund "+o.No); err != nil {
				return err
			}
		}
		tx.Exec(`UPDATE order_items SET status='void' WHERE order_id=?`, id)
		tx.Exec(`UPDATE orders SET status='refunded',pay_status='refunded',reason=?,updated_at=? WHERE id=?`, trunc(in.Reason, 200), nowUnix(), id)
		a.freeTableIfDone(tx, o.TableID, o.Branch)
		out, _ = loadOrder(tx, id, true)
		return nil
	})
	if err != nil {
		writeErr(w, 400, friendlyErr(err))
		return
	}
	a.hub.PublishBranch(out.Branch, "order")
	a.audit(r, "orders", id, "refund", out.No+" "+in.Reason)
	writeJSON(w, 200, out)
}

func (a *App) handleVoidItem(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var out *Order
	err := a.tx(func(tx *sql.Tx) error {
		var oid int64
		var it Item
		if tx.QueryRow(`SELECT order_id,product_id,qty,status FROM order_items WHERE id=?`, id).Scan(&oid, &it.Product, &it.Qty, &it.Status) != nil || it.Status == "void" {
			return uerr("item not found")
		}
		o, _ := loadOrder(tx, oid, false)
		if (c.Branch != 0 && o.Branch != c.Branch) || (a.ownOrdersOnly(c) && o.CreatedBy != c.UID) || o.Paid > 0 || o.Status == "completed" || o.Status == "cancelled" {
			return uerr("this item cannot be removed")
		}
		if it.Status != "pending" && a.level(c.Role, "orders") < LvApprove && c.Role != RoleSuperAdmin {
			return uerr("the kitchen already started this item - ask a manager")
		}
		tx.Exec(`UPDATE order_items SET status='void' WHERE id=?`, id)
		if o.Status != "held" {
			snap := a.menu()
			for _, l := range snap.recipes[it.Product] {
				a.stockMove(tx, o.Branch, l.Ingredient, l.Qty*float64(it.Qty), a.ingCost(tx, l.Ingredient), "return", fmt.Sprintf("order:%d", oid), "item removed", c.UID)
			}
		}
		out, err := a.recalc(tx, oid, c)
		if err == nil && len(out.Items) > 0 {
			active := 0
			for _, x := range out.Items {
				if x.Status != "void" && x.ID != id {
					active++
				}
			}
			if active == 0 {
				tx.Exec(`UPDATE orders SET status='cancelled',reason='all items removed' WHERE id=?`, oid)
				a.freeTableIfDone(tx, o.TableID, o.Branch)
			}
		}
		return err
	})
	if err != nil {
		writeErr(w, 400, friendlyErr(err))
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
	_ = out
	a.hub.Publish("order")
}
