package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"sync/atomic"
)

// Permission levels per module.
const (
	LvNone    = 0
	LvView    = 1 // see data
	LvManage  = 2 // create / edit / delete
	LvApprove = 3 // approve, post, refund, close periods ...
)

type Module struct{ Key, Label string }

var Modules = []Module{
	{"pos", "Point of Sale"}, {"orders", "Orders & Payments"}, {"kitchen", "Kitchen (KDS)"},
	{"menu", "Menu & Catalog"}, {"inventory", "Inventory"}, {"purchasing", "Purchasing"},
	{"customers", "Customers & Growth"}, {"floor", "Floor & Tables"}, {"reservations", "Reservations"},
	{"admin", "Administration"}, {"hr", "HR (people & setup)"}, {"attendance", "Attendance & Leave"},
	{"payroll", "Payroll"}, {"accounting", "Accounting"}, {"delivery", "Delivery"},
	{"reports", "Reports"}, {"settings", "Settings"}, {"support", "Support inbox (all tickets)"}, {"printing", "Printing bills & tickets"},
}

const (
	RoleSuperAdmin  = "superadmin"
	RoleAdmin       = "admin"
	RoleAdminBranch = "admin_branch"
	RoleManager     = "manager"
	RoleCashier     = "cashier"
	RoleKitchen     = "kitchen"
	RoleWaiter      = "waiter"
	RoleHROfficer   = "hr_officer"
	RoleHRManager   = "hr_manager"
	RoleAccountant  = "accountant"
	RoleAccManager  = "accounting_manager"
	RoleAccAuditor  = "accounting_auditor"
	RolePOSOperator = "pos_operator"
)

type Role struct {
	Key     string         `json:"key"`
	Label   string         `json:"label"`
	Perms   map[string]int `json:"perms"`
	Builtin bool           `json:"builtin"`
}

func allPerms(l int) map[string]int {
	m := map[string]int{}
	for _, mod := range Modules {
		m[mod.Key] = l
	}
	return m
}

func perms(kv ...any) map[string]int {
	m := map[string]int{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1].(int)
	}
	return m
}

var defaultRoles = []Role{
	{RoleSuperAdmin, "Super Admin", allPerms(3), true},
	{RoleAdmin, "Admin", allPerms(3), true},
	{RoleAdminBranch, "Admin Branch", allPerms(3), true},
	{RoleManager, "Manager", perms("pos", 2, "orders", 3, "kitchen", 2, "menu", 2, "inventory", 2, "purchasing", 2, "customers", 2,
		"floor", 2, "reservations", 2, "admin", 1, "hr", 1, "attendance", 1, "accounting", 1, "delivery", 2, "reports", 1, "settings", 1, "printing", 2), true},
	{RoleCashier, "Cashier", perms("pos", 2, "orders", 2, "kitchen", 1, "customers", 2, "floor", 1, "reservations", 1, "delivery", 1, "printing", 2), true},
	{RoleKitchen, "Kitchen", perms("kitchen", 2, "orders", 1, "inventory", 1, "printing", 2), true},
	{RoleWaiter, "Waiter", perms("pos", 2, "orders", 1, "kitchen", 1, "customers", 1, "floor", 1, "reservations", 2), true},
	{RoleHROfficer, "HR Officer", perms("hr", 2, "attendance", 2, "payroll", 1), true},
	{RoleHRManager, "HR Manager", perms("hr", 3, "attendance", 3, "payroll", 3, "reports", 1), true},
	{RoleAccountant, "Accountant", perms("accounting", 2, "purchasing", 1, "inventory", 1, "orders", 1, "reports", 1), true},
	{RoleAccManager, "Accounting Manager", perms("accounting", 3, "purchasing", 2, "inventory", 1, "orders", 1, "reports", 2), true},
	{RolePOSOperator, "POS Operator", perms("pos", 2, "floor", 1, "customers", 1, "printing", 2), true},
	{RoleAccAuditor, "Accounting Auditor", perms("accounting", 1, "purchasing", 1, "inventory", 1, "orders", 1, "reports", 1, "admin", 1), true},
}

var demoRoleOrder = []string{RoleSuperAdmin, RoleAdmin, RoleAdminBranch, RoleManager, RoleCashier, RoleKitchen, RoleWaiter,
	RoleHROfficer, RoleHRManager, RoleAccountant, RoleAccManager, RoleAccAuditor, RolePOSOperator}

// roleCache is an immutable snapshot so permission checks never touch the DB.
type roleCache struct {
	p atomic.Pointer[map[string]*Role]
}

func (a *App) loadRoles() {
	m := map[string]*Role{}
	rows, err := a.db.rdb.Query(`SELECT key,label,perms,builtin FROM roles`)
	if err == nil {
		for rows.Next() {
			var r Role
			var p string
			var b int
			rows.Scan(&r.Key, &r.Label, &p, &b)
			r.Builtin = b == 1
			json.Unmarshal([]byte(p), &r.Perms)
			if r.Perms == nil {
				r.Perms = map[string]int{}
			}
			m[r.Key] = &r
		}
		rows.Close()
	}
	a.roles.p.Store(&m)
}

func (a *App) role(key string) *Role {
	if m := a.roles.p.Load(); m != nil {
		return (*m)[key]
	}
	return nil
}

// level returns the user's permission level for a module.
func (a *App) level(role, module string) int {
	if role == RoleSuperAdmin {
		return LvApprove
	}
	if r := a.role(role); r != nil {
		return r.Perms[module]
	}
	return LvNone
}

func (a *App) rolesList() []*Role {
	out := []*Role{}
	if m := a.roles.p.Load(); m != nil {
		for _, r := range *m {
			out = append(out, r)
		}
	}
	idx := map[string]int{}
	for i, k := range demoRoleOrder {
		idx[k] = i + 1
	}
	sort.Slice(out, func(i, j int) bool {
		x, y := idx[out[i].Key], idx[out[j].Key]
		if x == 0 {
			x = 99
		}
		if y == 0 {
			y = 99
		}
		if x != y {
			return x < y
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// need = signed-in user whose role has at least `lvl` on `module`.
func (a *App) need(module string, lvl int) func(http.HandlerFunc) http.HandlerFunc {
	return func(next http.HandlerFunc) http.HandlerFunc {
		return a.authed(func(w http.ResponseWriter, r *http.Request) {
			c := userFrom(r)
			if a.level(c.Role, module) < lvl {
				writeErr(w, 403, "you don't have permission for this")
				return
			}
			next(w, r)
		})
	}
}

// ---------- branch scoping ----------

// scopeBranch decides which branch a request may see:
//
//	users tied to a branch -> always that branch
//	others                 -> ?branch=<id> (0 = all branches)
func scopeBranch(c *Claims, r *http.Request) int64 {
	if c.Branch != 0 {
		return c.Branch
	}
	return qInt(r, "branch")
}

func (a *App) handleRoles(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"roles": a.rolesList(), "modules": Modules})
}

func (a *App) handleSaveRole(w http.ResponseWriter, r *http.Request) {
	var in Role
	if !readJSON(w, r, &in) {
		return
	}
	key := r.PathValue("key")
	if key == RoleSuperAdmin {
		writeErr(w, 400, "super admin always has full access")
		return
	}
	clean := map[string]int{}
	for _, m := range Modules {
		if v := in.Perms[m.Key]; v > 0 && v <= 3 {
			clean[m.Key] = v
		}
	}
	pj, _ := json.Marshal(clean)
	if key == "" { // create custom role
		if in.Key == "" || in.Label == "" || !isSlug(in.Key) {
			writeErr(w, 400, "role key (letters/numbers/underscore) and label are required")
			return
		}
		if a.role(in.Key) != nil {
			writeErr(w, 409, "this role already exists")
			return
		}
		a.db.wdb.Exec(`INSERT INTO roles(key,label,perms,builtin) VALUES(?,?,?,0)`, in.Key, in.Label, string(pj))
	} else {
		if a.role(key) == nil {
			writeErr(w, 404, "role not found")
			return
		}
		a.db.wdb.Exec(`UPDATE roles SET label=?,perms=? WHERE key=?`, in.Label, string(pj), key)
	}
	a.loadRoles()
	a.audit(r, "roles", 0, "save", key+in.Key)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *App) handleDeleteRole(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if ro := a.role(key); ro == nil || ro.Builtin {
		writeErr(w, 400, "built-in roles cannot be deleted")
		return
	}
	var n int
	a.db.rdb.QueryRow(`SELECT COUNT(*) FROM users WHERE role=?`, key).Scan(&n)
	if n > 0 {
		writeErr(w, 409, "users still have this role")
		return
	}
	a.db.wdb.Exec(`DELETE FROM roles WHERE key=?`, key)
	a.loadRoles()
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func isSlug(s string) bool {
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return s != ""
}

// global: only head-office users (not tied to one branch) may use this endpoint.
// Branch admins run their own branch; settings, roles, backups and the chart of
// accounts affect every branch.
func (a *App) global(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if c := userFrom(r); c != nil && c.Branch != 0 {
			writeErr(w, 403, "only head-office users can do this")
			return
		}
		next(w, r)
	}
}

// ownOrdersOnly: staff who may take orders but have no "Orders" access (the POS operator)
// can only open, print, cancel or add to the bills they created themselves.
func (a *App) ownOrdersOnly(c *Claims) bool {
	return c != nil && c.Role != "guest" && c.Role != RoleSuperAdmin && a.level(c.Role, "orders") < LvView
}
