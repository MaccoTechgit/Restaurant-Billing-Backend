package main

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------
//  Test harness: a real server on a throw-away database with demo data.
//  Run with:  go test ./...   (takes ~10 s)
// ---------------------------------------------------------------------

var (
	tOnce sync.Once
	tApp  *App
	tSrv  *httptest.Server
	tTok  = map[string]string{}
	tMu   sync.Mutex
)

func harness(t *testing.T) *App {
	tOnce.Do(func() {
		dir, _ := os.MkdirTemp("", "tablebill-test-")
		cfg := Config{Addr: ":0", DBPath: filepath.Join(dir, "t.db"), Demo: true, NoLimit: true, Currency: "₹", Restaurant: "Test"}
		db, err := openDB(cfg.DBPath)
		if err != nil {
			panic(err)
		}
		tApp = &App{cfg: cfg, db: db, secret: []byte("test-secret-test-secret-test-secret!"), hub: NewHub(),
			lookups: newFlightCache(100), ordersCache: newFlightCache(100), dashCache: newFlightCache(100), notifCache: newFlightCache(100)}
		tApp.startBG()
		tApp.loadSettings()
		for _, r := range defaultRoles {
			db.wdb.Exec(`INSERT OR IGNORE INTO roles(key,label,perms,builtin) VALUES(?,?,?,1)`, r.Key, r.Label, string(jsonBytes(r.Perms)))
		}
		tApp.loadRoles()
		tApp.loadChart()
		tApp.bootstrap()
		tSrv = httptest.NewServer(middleware(tApp.routes()))
	})
	return tApp
}

func do(t *testing.T, user, method, path string, body any) (int, map[string]any, []byte) {
	t.Helper()
	harness(t)
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, tSrv.URL+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if user != "" {
		tMu.Lock()
		tok := tTok[user]
		if tok == "" {
			rr, _ := http.Post(tSrv.URL+"/api/auth/demo", "application/json", strings.NewReader(`{"username":"`+user+`"}`))
			var d struct{ Token string }
			json.NewDecoder(rr.Body).Decode(&d)
			rr.Body.Close()
			tok = d.Token
			tTok[user] = tok
		}
		tMu.Unlock()
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	json.Unmarshal(raw, &m)
	return resp.StatusCode, m, raw
}

func want(t *testing.T, name string, got, exp int) {
	t.Helper()
	if got != exp {
		t.Errorf("%s: got HTTP %d, want %d", name, got, exp)
	}
}

func productID(t *testing.T, name string) float64 {
	_, _, raw := do(t, "admin", "GET", "/api/menu", nil)
	var m struct {
		Products []struct {
			ID   float64
			Name string
		}
	}
	json.Unmarshal(raw, &m)
	for _, p := range m.Products {
		if p.Name == name {
			return p.ID
		}
	}
	t.Fatalf("product %q not found", name)
	return 0
}

// ---------- pure logic ----------

func TestParseSpec(t *testing.T) {
	fs := parseSpec(`name*^~; code#^; tax_id; price:money*^=5; kind|Type:select(a|b|c)=b; lines:lines(item:ref(items)*,qty:number*)*; flag:bool.`)
	by := map[string]Field{}
	for _, f := range fs {
		by[f.Key] = f
	}
	if f := by["name"]; f.Type != "text" || !f.Req || !f.List || !f.Search {
		t.Errorf("colon-less 'name*^~' must be a required, listed, searchable text field: %+v", f)
	}
	if f := by["code"]; !f.Code || f.Type != "text" {
		t.Errorf("'code#^' must be a unique-code text field: %+v", f)
	}
	if _, ok := by["tax_id"]; !ok {
		t.Error("plain 'tax_id' lost")
	}
	if f := by["price"]; f.Type != "money" || f.Def != float64(5) {
		t.Errorf("money default: %+v", f)
	}
	if f := by["kind"]; f.Label != "Type" || len(f.Opts) != 3 {
		t.Errorf("select/label: %+v", f)
	}
	if f := by["lines"]; len(f.Sub) != 2 || !f.Req || f.Sub[0].Ref != "items" {
		t.Errorf("lines: %+v", f)
	}
	if !by["flag"].RO {
		t.Error("read-only flag lost")
	}
}

func TestEveryResourceSpecIsSane(t *testing.T) {
	for k, r := range registry {
		seen := map[string]bool{}
		for _, f := range r.Fields {
			if !keyRe.MatchString(f.Key) {
				t.Errorf("resource %s has a field with a bad key %q (a spec is missing its ':type')", k, f.Key)
			}
			if seen[f.Key] {
				t.Errorf("resource %s repeats field %s", k, f.Key)
			}
			seen[f.Key] = true
			if f.Type == "select" && len(f.Opts) == 0 {
				t.Errorf("resource %s.%s is a select without options", k, f.Key)
			}
			if (f.Type == "ref" || f.Type == "multiref") && registry[f.Ref] == nil {
				t.Errorf("resource %s.%s points at unknown resource %q", k, f.Key, f.Ref)
			}
			switch f.Key {
			case "id", "ref", "branch_id", "created_at", "updated_at", "created_by":
				t.Errorf("resource %s uses reserved field name %q", k, f.Key)
			}
		}
		for _, a := range r.Actions {
			if r.OnAction[a.Key] == nil && a.To == "" {
				t.Errorf("resource %s action %s does nothing", k, a.Key)
			}
			if a.To != "" {
				ok := false
				for _, s := range r.Statuses {
					ok = ok || s == a.To
				}
				if !ok {
					t.Errorf("resource %s action %s moves to unknown status %q", k, a.Key, a.To)
				}
			}
		}
	}
	for _, rl := range navTree {
		var walk func([]NavItem)
		walk = func(items []NavItem) {
			for _, it := range items {
				if it.Kind == "res" && registry[it.Target] == nil {
					t.Errorf("menu item %q points at unknown resource %q", it.Title, it.Target)
				}
				walk(it.Children)
			}
		}
		walk(rl.Items)
	}
}

func TestTOTPKnownVector(t *testing.T) { // RFC 6238 appendix B (SHA-1), last 6 digits of 94287082
	if got := totpAt("GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ", 59); got != "287082" {
		t.Errorf("TOTP got %s want 287082", got)
	}
}

func TestCleanFieldsRules(t *testing.T) {
	fs := parseSpec(`name*; balance:money.; secret:password; kind:select(a|b)`)
	out, err := cleanFields(fs, map[string]any{"name": " X ", "balance": 99999, "secret": "k", "kind": "a"}, nil)
	if err != nil || out["name"] != "X" {
		t.Fatalf("clean: %v %v", out, err)
	}
	if _, has := out["balance"]; has {
		t.Error("a read-only field must be ignored on create")
	}
	if _, err := cleanFields(fs, map[string]any{"name": "x", "kind": "zzz"}, nil); err == nil {
		t.Error("invalid select must be rejected")
	}
	if _, err := cleanFields(fs, map[string]any{"name": ""}, nil); err == nil {
		t.Error("required must be enforced")
	}
	kept, _ := cleanFields(fs, map[string]any{"secret": maskedPw}, map[string]any{"name": "n", "secret": "real"})
	if kept["secret"] != "real" {
		t.Errorf("masked secret must keep the stored value, got %v", kept["secret"])
	}
}

// ---------- business flow ----------

func TestOrderPaymentAndLedger(t *testing.T) {
	harness(t)
	pid := productID(t, "Butter Chicken")
	nid := productID(t, "Butter Naan")
	code, o, _ := do(t, "cashier", "POST", "/api/pos/orders", map[string]any{"channel": "takeaway", "items": []any{map[string]any{"product_id": pid, "qty": 2}, map[string]any{"product_id": nid, "qty": 3}}})
	want(t, "create", code, 201)
	if o["subtotal"] != float64(77500) || o["tax"] != float64(3875) || o["total"] != float64(81375) {
		t.Fatalf("totals wrong: %v", o)
	}
	id := int64(o["id"].(float64))
	// the client cannot choose prices
	code, _, _ = do(t, "cashier", "POST", "/api/pos/orders", map[string]any{"channel": "takeaway", "items": []any{map[string]any{"product_id": 999999, "qty": 1}}})
	want(t, "unknown product", code, 400)
	code, _, _ = do(t, "cashier", "POST", "/api/pos/orders", map[string]any{"channel": "takeaway", "items": []any{map[string]any{"product_id": pid, "qty": -5}}})
	want(t, "negative qty", code, 400)
	// negative / zero payment
	code, _, _ = do(t, "cashier", "POST", "/api/pos/orders/"+itoa(id)+"/pay", map[string]any{"payments": []any{map[string]any{"method": "cash", "amount": -100}}})
	want(t, "negative payment", code, 400)
	code, r, _ := do(t, "cashier", "POST", "/api/pos/orders/"+itoa(id)+"/pay", map[string]any{"payments": []any{map[string]any{"method": "cash", "amount": 50000}, map[string]any{"method": "card", "amount": 31375}}})
	want(t, "pay", code, 200)
	ord := r["order"].(map[string]any)
	if ord["pay_status"] != "paid" {
		t.Errorf("not paid: %v", ord)
	}
	pays := ord["payments"].([]any)
	if pays[0].(map[string]any)["by"] != "Cashier" {
		t.Errorf("payment must carry the cashier name: %v", pays[0])
	}
	code, _, _ = do(t, "cashier", "POST", "/api/pos/orders/"+itoa(id)+"/pay", map[string]any{"payments": []any{map[string]any{"method": "cash", "amount": 100}}})
	want(t, "double payment", code, 400)
	_, bs, _ := do(t, "accountant", "GET", "/api/acc/report/bs", nil)
	if bs["balanced"] != true {
		t.Errorf("balance sheet out of balance: %v", bs["difference"])
	}
	_, tb, _ := do(t, "accountant", "GET", "/api/acc/report/trial", nil)
	if tb["balanced"] != true {
		t.Error("trial balance out of balance")
	}
}

func itoa(n int64) string {
	return strings.TrimSpace(strings.Join(strings.Fields(strings.TrimSpace(jsonNum(n))), ""))
}
func jsonNum(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// ---------- security ----------

func TestDemoOffByDefault(t *testing.T) {
	t.Setenv("DEMO", "")
	if loadConfig().Demo {
		t.Error("passwordless demo sign-in must be OFF unless DEMO=1 is set")
	}
}

func TestAuthRequired(t *testing.T) {
	for _, p := range []string{"/api/me", "/api/users", "/api/r/products", "/api/pos/orders", "/api/acc/dash", "/api/db/info", "/api/events", "/api/schema", "/api/lookup/products", "/api/settings/pos"} {
		code, _, _ := do(t, "", "GET", p, nil)
		want(t, "anonymous "+p, code, 401)
	}
	code, _, _ := do(t, "", "GET", "/api/me?token=garbage", nil)
	want(t, "garbage token", code, 401)
}

func TestTokenTamperingRejected(t *testing.T) {
	a := harness(t)
	tok := a.sign(Claims{UID: 2, User: "admin", Role: RoleSuperAdmin, Iat: time.Now().Unix(), Exp: time.Now().Add(time.Hour).Unix()})
	parts := strings.Split(tok, ".")
	forged := parts[0] + "." + b64.EncodeToString([]byte(`{"uid":2,"u":"admin","r":"superadmin","b":0,"exp":9999999999,"iat":1}`)) + "." + parts[2]
	req, _ := http.NewRequest("GET", tSrv.URL+"/api/me", nil)
	req.Header.Set("Authorization", "Bearer "+forged)
	resp, _ := http.DefaultClient.Do(req)
	want(t, "forged payload", resp.StatusCode, 401)
	none := b64.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`)) + "." + parts[1] + "."
	req.Header.Set("Authorization", "Bearer "+none)
	resp, _ = http.DefaultClient.Do(req)
	want(t, "alg none", resp.StatusCode, 401)
	expired := a.sign(Claims{UID: 2, User: "admin", Role: RoleSuperAdmin, Iat: 1, Exp: 2})
	req.Header.Set("Authorization", "Bearer "+expired)
	resp, _ = http.DefaultClient.Do(req)
	want(t, "expired", resp.StatusCode, 401)
}

func TestRevocationSurvivesRestart(t *testing.T) {
	a := harness(t)
	var uid int64
	a.db.rdb.QueryRow(`SELECT id FROM users WHERE username='waiter'`).Scan(&uid)
	c := Claims{UID: uid, User: "waiter", Role: RoleWaiter, Iat: time.Now().Unix() - 5, Exp: time.Now().Add(time.Hour).Unix()}
	tok := a.sign(c)
	if _, err := a.verify(tok); err != nil {
		t.Fatal("fresh token should verify")
	}
	a.revoke(uid)
	if _, err := a.verify(tok); err == nil {
		t.Fatal("revoked token still accepted")
	}
	a.revoked.Range(func(k, _ any) bool { a.revoked.Delete(k); return true }) // "restart"
	if _, err := a.verify(tok); err != nil {
		t.Fatal("(sanity) memory cleared")
	}
	a.loadRevoked()
	if _, err := a.verify(tok); err == nil {
		t.Fatal("sign-out marker was lost after a restart")
	}
	a.db.wdb.Exec(`UPDATE users SET revoked_at=0 WHERE id=?`, uid)
	a.revoked.Delete(uid)
}

func TestWorkflowStatusCannotBeForged(t *testing.T) {
	harness(t)
	// a purchase order created already "received" must still start as a draft
	_, sup, _ := do(t, "admin", "GET", "/api/r/suppliers?limit=1", nil)
	sid := sup["rows"].([]any)[0].(map[string]any)["id"]
	_, ing, _ := do(t, "admin", "GET", "/api/r/ingredients?limit=1", nil)
	iid := ing["rows"].([]any)[0].(map[string]any)["id"]
	code, po, _ := do(t, "admin", "POST", "/api/r/purchases", map[string]any{"supplier": sid, "date": "2026-10-12", "status": "received", "branch_id": 1, "lines": []any{map[string]any{"ingredient": iid, "qty": 5, "cost": 100}}})
	want(t, "create PO", code, 201)
	if po["status"] != "draft" {
		t.Errorf("status was forged on create: %v", po["status"])
	}
	id := jsonNum(int64(po["id"].(float64)))
	_, up, _ := do(t, "admin", "PUT", "/api/r/purchases/"+id, map[string]any{"status": "received", "supplier": sid, "date": "2026-10-12", "lines": []any{map[string]any{"ingredient": iid, "qty": 5, "cost": 100}}})
	if up["status"] != "draft" {
		t.Errorf("status was forged on update: %v", up["status"])
	}
	// an employee cannot approve their own leave
	_, lt, _ := do(t, "admin", "GET", "/api/r/leave_types?limit=1", nil)
	ltid := lt["rows"].([]any)[0].(map[string]any)["id"]
	code, lv, _ := do(t, "waiter", "POST", "/api/hr/my/leave", map[string]any{"leave_type": ltid, "from": "2027-02-02", "to": "2027-02-02", "status": "approved", "reviewed_by": "me"})
	if code == 201 && lv["status"] != "pending" && lv["reviewed_by"] != "auto-approved" {
		t.Errorf("self-approved leave: %v", lv)
	}
}

func TestReadOnlyFieldsCannotBeSet(t *testing.T) {
	harness(t)
	code, c, _ := do(t, "cashier", "POST", "/api/r/customers", map[string]any{"name": "Hacker", "points": 999999, "spent": 99999999, "visits": 50, "tier": "Gold"})
	want(t, "create customer", code, 201)
	if c["points"] != float64(0) || c["spent"] != float64(0) {
		t.Errorf("client set read-only customer fields: %v", c)
	}
	code, g, _ := do(t, "admin", "POST", "/api/r/gift_cards", map[string]any{"code": "TST-1", "initial": 1000, "balance": 99999999})
	want(t, "gift card", code, 201)
	if g["balance"] != float64(1000) {
		t.Errorf("gift card balance forged: %v", g["balance"])
	}
}

func TestSecretsNeverLeave(t *testing.T) {
	harness(t)
	code, c, _ := do(t, "admin", "POST", "/api/r/couriers", map[string]any{"name": "SecretCo", "api_key": "SUPER-SECRET-KEY", "type": "custom"})
	want(t, "create courier", code, 201)
	if c["api_key"] != maskedPw {
		t.Errorf("api key echoed back: %v", c["api_key"])
	}
	for _, p := range []string{"/api/r/couriers", "/api/r/couriers/" + jsonNum(int64(c["id"].(float64))), "/api/lookup/couriers?full=1"} {
		_, _, raw := do(t, "cashier", "GET", p, nil)
		_, _, raw2 := do(t, "admin", "GET", p, nil)
		if strings.Contains(string(raw), "SUPER-SECRET-KEY") || strings.Contains(string(raw2), "SUPER-SECRET-KEY") {
			t.Errorf("secret leaked from %s", p)
		}
	}
	// saving the masked value back must not wipe the real key
	do(t, "admin", "PUT", "/api/r/couriers/"+jsonNum(int64(c["id"].(float64))), map[string]any{"name": "SecretCo2", "api_key": maskedPw})
	rec, _ := loadRec(tApp.db.rdb, int64(c["id"].(float64)))
	if rec.S("api_key") != "SUPER-SECRET-KEY" {
		t.Error("masked placeholder overwrote the stored secret")
	}
	// settings secrets
	do(t, "admin", "PUT", "/api/settings/email", map[string]any{"enabled": false, "host": "h", "port": 25, "password": "SMTP-PASS-123", "from": "a@b.c", "encryption": "none"})
	_, _, raw := do(t, "admin", "GET", "/api/settings/email", nil)
	if strings.Contains(string(raw), "SMTP-PASS-123") {
		t.Error("SMTP password leaked")
	}
	_, _, cfg := do(t, "", "GET", "/api/config", nil)
	if strings.Contains(string(cfg), "SMTP-PASS-123") || strings.Contains(string(cfg), "hash") {
		t.Error("public config leaks secrets")
	}
	_, _, usr := do(t, "admin", "GET", "/api/users", nil)
	if strings.Contains(string(usr), "pass_hash") || strings.Contains(string(usr), "$2a$") || strings.Contains(string(usr), "totp_secret") {
		t.Error("users API leaks password hashes / 2FA secrets")
	}
}

func TestBranchAdminIsContained(t *testing.T) {
	harness(t)
	for _, c := range []struct{ m, p string }{
		{"GET", "/api/db/info"}, {"POST", "/api/db/backup"}, {"POST", "/api/roles"}, {"GET", "/api/authlog"},
		{"POST", "/api/acc/accounts"}, {"POST", "/api/acc/opening"},
	} {
		code, _, _ := do(t, "admin_branch", c.m, c.p, map[string]any{})
		want(t, "branch admin "+c.m+" "+c.p, code, 403)
	}
	code, _, _ := do(t, "admin_branch", "PUT", "/api/settings/pos", map[string]any{"service_charge_pct": 50})
	want(t, "branch admin settings write", code, 403)
	code, _, _ = do(t, "admin_branch", "POST", "/api/r/products", map[string]any{"name": "X", "category": 1, "price": 100})
	want(t, "branch admin edits shared menu", code, 403)
	// their data is only their own branch
	_, d, _ := do(t, "admin_branch", "GET", "/api/pos/orders?limit=200", nil)
	for _, r := range d["rows"].([]any) {
		if r.(map[string]any)["branch_id"] != float64(2) {
			t.Fatalf("branch admin saw another branch's order: %v", r.(map[string]any)["no"])
		}
	}
	// a journal of another branch is invisible
	_, j, _ := do(t, "admin", "GET", "/api/acc/journals?limit=1&source=order", nil)
	jr := j["rows"].([]any)
	for _, x := range jr {
		m := x.(map[string]any)
		if m["branch_id"] != float64(2) {
			code, _, _ := do(t, "admin_branch", "GET", "/api/acc/journals/"+jsonNum(int64(m["id"].(float64))), nil)
			want(t, "other-branch journal", code, 404)
		}
	}
	// cannot create a super admin
	code, _, _ = do(t, "admin_branch", "POST", "/api/users", map[string]any{"username": "evil", "name": "Evil", "role": "superadmin", "password": "longenough1"})
	want(t, "grant superadmin", code, 403)
}

func TestRoleBoundaries(t *testing.T) {
	harness(t)
	cases := []struct {
		user, method, path string
		code               int
	}{
		{"cashier", "GET", "/api/acc/dash", 403}, {"cashier", "GET", "/api/users", 403}, {"cashier", "PUT", "/api/settings/pos", 403},
		{"kitchen", "GET", "/api/dash/hub", 403}, {"waiter", "GET", "/api/r/employees", 403}, {"waiter", "GET", "/api/r/payslips", 403},
		{"pos_operator", "GET", "/api/acc/dash", 403}, {"pos_operator", "GET", "/api/dash/hub", 403}, {"pos_operator", "GET", "/api/stock", 403},
		{"pos_operator", "GET", "/api/menu", 200}, {"pos_operator", "GET", "/api/pos/tables", 200},
		{"accounting_auditor", "POST", "/api/acc/journals", 403}, {"hr_officer", "GET", "/api/acc/dash", 403},
		{"manager", "POST", "/api/roles", 403}, {"manager", "GET", "/api/db/info", 403},
	}
	for _, c := range cases {
		code, _, _ := do(t, c.user, c.method, c.path, map[string]any{})
		want(t, c.user+" "+c.method+" "+c.path, code, c.code)
	}
	// the POS operator can take orders but never payments or refunds
	pid := productID(t, "Masala Chai")
	code, o, _ := do(t, "pos_operator", "POST", "/api/pos/orders", map[string]any{"channel": "takeaway", "items": []any{map[string]any{"product_id": pid, "qty": 1}}})
	want(t, "operator creates order", code, 201)
	id := jsonNum(int64(o["id"].(float64)))
	code, _, _ = do(t, "pos_operator", "POST", "/api/pos/orders/"+id+"/pay", map[string]any{"payments": []any{map[string]any{"method": "cash", "amount": 10000}}})
	want(t, "operator pays", code, 403)
	code, _, _ = do(t, "pos_operator", "POST", "/api/pos/orders/"+id+"/refund", map[string]any{})
	want(t, "operator refunds", code, 403)
	code, _, _ = do(t, "pos_operator", "GET", "/api/pos/orders/"+id, nil)
	want(t, "operator reads order for printing", code, 200)
}

func TestInjectionAndTraversal(t *testing.T) {
	harness(t)
	for _, q := range []string{"/api/pos/orders?q=" + url.QueryEscape("' OR 1=1 --"), "/api/r/products?q=" + url.QueryEscape("%' OR '1'='1"), "/api/r/products?f.name=" + url.QueryEscape("x' OR '1'='1"),
		"/api/r/products?f.name%27)%20OR%201=1--=1", "/api/rep/sales?group=" + url.QueryEscape("day'); DROP TABLE orders;--"), "/api/acc/journals?source=" + url.QueryEscape("x'--")} {
		code, _, _ := do(t, "admin", "GET", q, nil)
		if code >= 500 {
			t.Errorf("%s -> HTTP %d", q, code)
		}
	}
	_, o, _ := do(t, "admin", "GET", "/api/pos/orders?q="+url.QueryEscape("' OR 1=1 --"), nil)
	if o["total"] != float64(0) {
		t.Errorf("injection returned rows: %v", o["total"])
	}
	var n int
	harness(t).db.rdb.QueryRow(`SELECT COUNT(*) FROM orders`).Scan(&n)
	if n == 0 {
		t.Error("orders table is gone?!")
	}
	for _, p := range []string{"/media/1/..%2f..%2fsecret.key", "/api/db/backup/..%2f..%2fsecret.key", "/api/db/backup/..%5csecret.key", "/..%2f..%2fgo.mod", "/web/../go.mod"} {
		req, _ := http.NewRequest("GET", tSrv.URL+p, nil)
		req.Header.Set("Authorization", "Bearer "+tTok["superadmin"])
		if tTok["superadmin"] == "" {
			do(t, "superadmin", "GET", "/api/me", nil)
			req.Header.Set("Authorization", "Bearer "+tTok["superadmin"])
		}
		resp, _ := http.DefaultClient.Do(req)
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if strings.Contains(string(b), "module tablebill") || strings.Contains(string(b), "SQLite format") {
			t.Errorf("path traversal exposed a file via %s", p)
		}
	}
	// header injection through the export type
	req, _ := http.NewRequest("GET", tSrv.URL+"/api/acc/export?type="+url.QueryEscape("x\r\nSet-Cookie: a=b"), nil)
	do(t, "accountant", "GET", "/api/me", nil)
	req.Header.Set("Authorization", "Bearer "+tTok["accountant"])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Header.Get("Set-Cookie") != "" {
		t.Error("header injection via export type")
	}
	resp.Body.Close()
}

func TestUploadsAreSafe(t *testing.T) {
	harness(t)
	up := func(name, content string) int {
		var b bytes.Buffer
		w := multipart.NewWriter(&b)
		fw, _ := w.CreateFormFile("file", name)
		fw.Write([]byte(content))
		w.Close()
		req, _ := http.NewRequest("POST", tSrv.URL+"/api/media", &b)
		req.Header.Set("Content-Type", w.FormDataContentType())
		do(t, "admin", "GET", "/api/me", nil)
		req.Header.Set("Authorization", "Bearer "+tTok["admin"])
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for _, n := range []string{"x.html", "x.svg", "x.js", "x.exe", "x.php", "x"} {
		if c := up(n, "<script>alert(1)</script>"); c != 400 {
			t.Errorf("upload of %s allowed (HTTP %d)", n, c)
		}
	}
	if c := up("x.png", "<html><script>alert(1)</script></html>"); c != 400 {
		t.Errorf("an HTML file disguised as .png was accepted (HTTP %d)", c)
	}
	if c := up("ok.txt", "hello"); c == 201 {
		t.Error("txt is not in the allowed list but was accepted")
	}
}

func TestPasswordRules(t *testing.T) {
	harness(t)
	code, _, _ := do(t, "admin", "POST", "/api/users", map[string]any{"username": "shortpw", "name": "S", "role": "waiter", "password": "abc123"})
	want(t, "short password", code, 400)
	code, _, _ = do(t, "admin", "POST", "/api/users", map[string]any{"username": "goodpw", "name": "G", "role": "waiter", "password": "abcd12345"})
	want(t, "good password", code, 201)
	// lockout after repeated wrong passwords
	var last int
	for i := 0; i < 8; i++ {
		last, _, _ = do(t, "", "POST", "/api/auth/login", map[string]any{"username": "goodpw", "password": "wrong-wrong"})
	}
	want(t, "account lockout", last, 429)
	code, _, _ = do(t, "", "POST", "/api/auth/login", map[string]any{"username": "goodpw", "password": "abcd12345"})
	want(t, "locked account stays locked", code, 429)
}

func TestResetTokensAreHashedAndHostSafe(t *testing.T) {
	a := harness(t)
	var uid int64
	a.db.rdb.QueryRow(`SELECT id FROM users WHERE username='cashier'`).Scan(&uid)
	link := a.makeResetLink(uid, "http://x")
	raw := link[strings.LastIndex(link, "=")+1:]
	var n int
	a.db.rdb.QueryRow(`SELECT COUNT(*) FROM resets WHERE token=?`, raw).Scan(&n)
	if n != 0 {
		t.Error("reset token stored in clear text")
	}
	a.db.rdb.QueryRow(`SELECT COUNT(*) FROM resets WHERE token=?`, hashTok(raw)).Scan(&n)
	if n != 1 {
		t.Error("hashed reset token missing")
	}
	code, _, _ := do(t, "", "POST", "/api/auth/reset", map[string]any{"token": raw, "password": "brandnew-pass1"})
	want(t, "reset works", code, 200)
	code, _, _ = do(t, "", "POST", "/api/auth/reset", map[string]any{"token": raw, "password": "again-and-again1"})
	want(t, "reset is one-time", code, 400)
	a.db.wdb.Exec(`UPDATE users SET pass_hash=? WHERE id=?`, hashPassword("demo123"), uid) // restore for other tests
}

func TestQROrderingIsLimited(t *testing.T) {
	harness(t)
	_, tb, _ := do(t, "admin", "GET", "/api/r/tables?limit=1", nil)
	tok := tb["rows"].([]any)[0].(map[string]any)["qr_token"].(string)
	pid := productID(t, "Masala Chai")
	post := func(body map[string]any) int {
		b, _ := json.Marshal(body)
		r, _ := http.Post(tSrv.URL+"/api/public/qr/order", "application/json", bytes.NewReader(b))
		r.Body.Close()
		return r.StatusCode
	}
	if c := post(map[string]any{"t": "nope-nope", "items": []any{map[string]any{"product_id": pid, "qty": 1}}}); c == 201 {
		t.Error("bad QR token accepted")
	}
	got429 := false
	for i := 0; i < 12; i++ {
		if post(map[string]any{"t": tok, "items": []any{map[string]any{"product_id": pid, "qty": 1}}}) == 400 {
			got429 = true
		}
	}
	if !got429 {
		t.Error("a single table can flood the kitchen with unlimited QR orders")
	}
	// guests never see staff data
	_, _, raw := do(t, "", "GET", "/api/public/qr?t="+tok, nil)
	if strings.Contains(string(raw), "pass_hash") || strings.Contains(string(raw), `"cost"`) {
		t.Error("public menu leaks internals")
	}
}

func TestSecurityHeaders(t *testing.T) {
	harness(t)
	resp, err := http.Get(tSrv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	for _, h := range []string{"Content-Security-Policy", "X-Content-Type-Options", "X-Frame-Options", "Referrer-Policy"} {
		if resp.Header.Get(h) == "" {
			t.Errorf("missing %s header", h)
		}
	}
	if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Error("CSP must forbid framing")
	}
}

func TestCustomerDisplayNeedsKey(t *testing.T) {
	harness(t)
	code, _, _ := do(t, "", "GET", "/api/display?r=1&k=0000", nil)
	want(t, "display with a wrong key", code, 403)
}

func TestSupportTickets(t *testing.T) {
	harness(t)
	code, tk, _ := do(t, "waiter", "POST", "/api/support", map[string]any{"subject": "Printer stuck", "message": "Kitchen printer does not print", "category": "printing", "priority": "high", "page": "kds"})
	want(t, "waiter files a ticket", code, 201)
	id := jsonNum(int64(tk["id"].(float64)))
	code, _, _ = do(t, "waiter", "POST", "/api/support", map[string]any{"subject": "", "message": "x"})
	want(t, "empty ticket rejected", code, 400)
	// the reporter sees it, other staff do not
	_, mine, raw := do(t, "waiter", "GET", "/api/support/mine", nil)
	_ = mine
	if !strings.Contains(string(raw), "Printer stuck") {
		t.Error("reporter cannot see their ticket")
	}
	code, _, _ = do(t, "kitchen", "GET", "/api/support/"+id, nil)
	want(t, "another user reads the ticket", code, 404)
	code, _, _ = do(t, "kitchen", "POST", "/api/support/"+id+"/reply", map[string]any{"text": "hi"})
	want(t, "another user replies", code, 400)
	code, _, _ = do(t, "admin", "GET", "/api/support", nil)
	want(t, "admin (not super) reads the inbox", code, 200) // admin role has full rights by design
	code, _, _ = do(t, "manager", "GET", "/api/support", nil)
	want(t, "manager cannot read the inbox", code, 403)
	code, _, _ = do(t, "waiter", "POST", "/api/support/"+id+"/status", map[string]any{"status": "closed"})
	want(t, "reporter cannot close their own ticket", code, 403)
	// the super admin answers and resolves
	code, r, _ := do(t, "superadmin", "POST", "/api/support/"+id+"/reply", map[string]any{"text": "Checking now"})
	want(t, "super admin replies", code, 200)
	if r["status"] != "in_progress" {
		t.Errorf("status after first staff reply: %v", r["status"])
	}
	code, r, _ = do(t, "superadmin", "POST", "/api/support/"+id+"/status", map[string]any{"status": "resolved"})
	want(t, "resolve", code, 200)
	_, tj, _ := do(t, "waiter", "GET", "/api/support/"+id, nil)
	if len(tj["replies"].([]any)) != 1 {
		t.Errorf("reply thread wrong: %v", tj["replies"])
	}
	// the reporter says it is not fixed -> reopens
	_, r2, _ := do(t, "waiter", "POST", "/api/support/"+id+"/reply", map[string]any{"text": "still broken"})
	if r2["status"] != "open" {
		t.Errorf("a reply on a resolved ticket must reopen it, got %v", r2["status"])
	}
	code, _, _ = do(t, "superadmin", "POST", "/api/support/"+id+"/status", map[string]any{"status": "bogus"})
	want(t, "invalid status", code, 400)
	// XSS text is stored as text only (the browser escapes it) - just make sure it round-trips safely
	code, _, _ = do(t, "waiter", "POST", "/api/support", map[string]any{"subject": "<img src=x onerror=alert(1)>", "message": "<script>alert(1)</script>"})
	want(t, "html in ticket is just text", code, 201)
}

func TestPaymentModeOnBill(t *testing.T) {
	harness(t)
	pid := productID(t, "Masala Chai")
	code, o, _ := do(t, "pos_operator", "POST", "/api/pos/orders", map[string]any{"channel": "takeaway", "pay_mode": "upi", "items": []any{map[string]any{"product_id": pid, "qty": 1}}})
	want(t, "operator order with pay mode", code, 201)
	if o["pay_mode"] != "upi" {
		t.Errorf("pay mode not saved: %v", o["pay_mode"])
	}
	code, o, _ = do(t, "pos_operator", "POST", "/api/pos/orders", map[string]any{"channel": "takeaway", "pay_mode": "<script>", "items": []any{map[string]any{"product_id": pid, "qty": 1}}})
	want(t, "junk pay mode", code, 201)
	if o["pay_mode"] != "" {
		t.Errorf("unknown pay mode must be dropped, got %v", o["pay_mode"])
	}
}

func TestPrintingPermission(t *testing.T) {
	a := harness(t)
	for role, exp := range map[string]int{RoleWaiter: 0, RoleCashier: 2, RolePOSOperator: 2, RoleKitchen: 2, RoleManager: 2, RoleSuperAdmin: 3} {
		if got := a.level(role, "printing"); got != exp {
			t.Errorf("%s printing level %d, want %d", role, got, exp)
		}
	}
	_, me, _ := do(t, "waiter", "GET", "/api/me", nil)
	if me["perms"].(map[string]any)["printing"] != float64(0) {
		t.Error("waiter must not have the printing permission")
	}
	// the QR guest page is branded
	_, tb, _ := do(t, "admin", "GET", "/api/r/tables?limit=1", nil)
	tok := tb["rows"].([]any)[0].(map[string]any)["qr_token"].(string)
	_, q, _ := do(t, "", "GET", "/api/public/qr?t="+tok, nil)
	if q["restaurant"] == "" || q["table"] == "" {
		t.Errorf("guest page lacks restaurant/table: %v", q)
	}
}

func TestWaiterIsToldWhenOrderIsReady(t *testing.T) {
	harness(t)
	pid := productID(t, "Masala Chai")
	_, tb, _ := do(t, "admin", "GET", "/api/r/tables?limit=50", nil)
	tableID := tb["rows"].([]any)[7].(map[string]any)["id"]
	code, o, _ := do(t, "waiter", "POST", "/api/pos/orders", map[string]any{"channel": "dine_in", "table_id": tableID, "items": []any{map[string]any{"product_id": pid, "qty": 1}}})
	want(t, "waiter order", code, 201)
	id := jsonNum(int64(o["id"].(float64)))
	code, _, _ = do(t, "kitchen", "POST", "/api/pos/orders/"+id+"/status", map[string]any{"status": "preparing"})
	want(t, "preparing", code, 200)
	code, _, _ = do(t, "kitchen", "POST", "/api/pos/orders/"+id+"/status", map[string]any{"status": "ready"})
	want(t, "ready", code, 200)
	time.Sleep(900 * time.Millisecond) // background queue
	found := false
	for i := 0; i < 5 && !found; i++ {
		_, _, raw := do(t, "waiter", "GET", "/api/notifications", nil)
		found = strings.Contains(string(raw), "is ready")
		if !found {
			time.Sleep(1200 * time.Millisecond)
		}
	}
	if !found {
		t.Error("the waiter was not notified that the order is ready")
	}
}

func TestPOSOperatorSeesOnlyOwnBills(t *testing.T) {
	harness(t)
	pid := productID(t, "Masala Chai")
	item := []any{map[string]any{"product_id": pid, "qty": 1}}
	_, mine, _ := do(t, "pos_operator", "POST", "/api/pos/orders", map[string]any{"channel": "takeaway", "items": item})
	_, other, _ := do(t, "manager", "POST", "/api/pos/orders", map[string]any{"channel": "takeaway", "items": item})
	if mine["id"] == nil || other["id"] == nil {
		t.Fatal("could not create the two orders")
	}
	mid, oid := jsonNum(int64(mine["id"].(float64))), jsonNum(int64(other["id"].(float64)))
	code, _, _ := do(t, "pos_operator", "GET", "/api/pos/orders/"+mid, nil)
	want(t, "operator opens own bill", code, 200)
	code, _, _ = do(t, "pos_operator", "GET", "/api/pos/orders/"+oid, nil)
	want(t, "operator opens someone else's bill", code, 404)
	code, _, _ = do(t, "pos_operator", "POST", "/api/pos/orders/"+oid+"/cancel", map[string]any{"reason": "x"})
	want(t, "operator cancels someone else's bill", code, 400)
	_, list, _ := do(t, "pos_operator", "GET", "/api/pos/orders?limit=100&status=all", nil)
	for _, r := range list["rows"].([]any) {
		if r.(map[string]any)["staff"] != "POS Operator" {
			t.Fatalf("operator's list contains another person's bill: %v", r.(map[string]any)["no"])
		}
	}
	_, all, _ := do(t, "manager", "GET", "/api/pos/orders?limit=5&status=all", nil)
	if all["total"].(float64) <= list["total"].(float64) {
		t.Error("the manager must still see every bill")
	}
	// waiters keep seeing everything (they serve every table)
	code, _, _ = do(t, "waiter", "GET", "/api/pos/orders/"+oid, nil)
	want(t, "waiter reads any bill", code, 200)
}
