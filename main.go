package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	_ "time/tzdata"
)

type App struct {
	cfg     Config
	db      *DB
	secret  []byte
	hub     *Hub
	roles   roleCache
	revoked sync.Map // user id -> unix time of last forced sign-out
	bg      chan bgItem

	cfgCache  settingsCache
	cfgPublic atomic.Pointer[[]byte]
	chart     atomic.Pointer[chart]
	menuSnap  atomic.Pointer[menuSnap]

	lookups     *flightCache
	ordersCache *flightCache
	dashCache   *flightCache
	notifCache  *flightCache
}

// loadSecret: JWT_SECRET from env, else a random key saved next to the DB so
// restarts keep everyone signed in.
func loadSecret(cfg Config) []byte {
	if cfg.Secret != "" {
		return []byte(cfg.Secret)
	}
	p := filepath.Join(filepath.Dir(cfg.DBPath), "secret.key")
	if b, err := os.ReadFile(p); err == nil && len(b) >= 32 {
		return b
	}
	b := make([]byte, 32)
	rand.Read(b)
	key := []byte(hex.EncodeToString(b))
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, key, 0o600)
	return key
}

func main() {
	cfg := loadConfig()
	db, err := openDB(cfg.DBPath)
	if err != nil {
		log.Fatal("database: ", err)
	}
	app := &App{cfg: cfg, db: db, secret: loadSecret(cfg), hub: NewHub(),
		lookups: newFlightCache(500), ordersCache: newFlightCache(500), dashCache: newFlightCache(500), notifCache: newFlightCache(500)}
	app.startBG()
	app.loadSettings()
	for _, r := range defaultRoles {
		db.wdb.Exec(`INSERT OR IGNORE INTO roles(key,label,perms,builtin) VALUES(?,?,?,1)`, r.Key, r.Label, string(jsonBytes(r.Perms)))
	}
	app.migratePrintingPerm()
	app.loadRoles()
	app.loadChart()
	app.loadRevoked()
	app.bootstrap()
	app.warnDefaults()
	app.checkNav()

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           middleware(app.routes()),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	if os.Getenv("STATS") == "1" { // print connection stats every 5s
		var opened, closed, active atomic.Int64
		srv.ConnState = func(_ net.Conn, st http.ConnState) {
			switch st {
			case http.StateNew:
				opened.Add(1)
				active.Add(1)
			case http.StateClosed, http.StateHijacked:
				closed.Add(1)
				active.Add(-1)
			}
		}
		go func() {
			for range time.Tick(5 * time.Second) {
				log.Printf("stats: conns active=%d opened=%d closed=%d goroutines=%d", active.Load(), opened.Load(), closed.Load(), runtime.NumGoroutine())
			}
		}()
	}
	go func() {
		log.Printf("TableBill running on %s (demo mode: %v)", cfg.Addr, cfg.Demo)
		var err error
		if cert, key := os.Getenv("TLS_CERT"), os.Getenv("TLS_KEY"); cert != "" && key != "" {
			err = srv.ListenAndServeTLS(cert, key)
		} else {
			err = srv.ListenAndServe()
		}
		if err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
	go app.janitor()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Println("shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
	time.Sleep(400 * time.Millisecond) // let the background log queue flush
	db.wdb.Close()
	db.rdb.Close()
}

// bootstrap prepares a brand-new database.
func (a *App) bootstrap() {
	var n int
	a.db.rdb.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	if n > 0 {
		return
	}
	if a.cfg.Demo {
		a.seed("demo123")
		return
	}
	pass := os.Getenv("ADMIN_PASSWORD")
	if pass == "" {
		pass = randHex(5)
		log.Printf("=== first run: sign in as  superadmin  /  %s   (change it after signing in) ===", pass)
	}
	for _, r := range defaultRoles {
		a.db.wdb.Exec(`INSERT OR IGNORE INTO roles(key,label,perms,builtin) VALUES(?,?,?,1)`, r.Key, r.Label, string(jsonBytes(r.Perms)))
	}
	a.tx(func(tx *sql.Tx) error {
		a.seedChart(tx)
		mk(tx, "branches", 0, map[string]any{"name": "Main Branch", "code": "MAIN", "status": "active"})
		mk(tx, "taxes", 0, map[string]any{"name": "GST 5%", "rate": 5.0, "default": true, "status": "active"})
		tx.Exec(`INSERT INTO users(username,name,role,pass_hash,created_at) VALUES('superadmin','Super Admin','superadmin',?,?)`, hashPassword(pass), nowUnix())
		return nil
	})
	a.loadRoles()
	a.loadChart()
}

func (a *App) janitor() {
	for range time.Tick(time.Hour) {
		a.db.wdb.Exec(`DELETE FROM resets WHERE exp<?`, nowUnix())
		a.db.wdb.Exec(`PRAGMA wal_checkpoint(PASSIVE)`)
	}
}

// checkNav warns at start-up if the menu points at a screen that does not exist.
func (a *App) checkNav() {
	var walk func(items []NavItem)
	walk = func(items []NavItem) {
		for _, it := range items {
			if it.Kind == "res" && registry[it.Target] == nil {
				log.Printf("WARNING: menu item %q points to unknown resource %q", it.Title, it.Target)
			}
			walk(it.Children)
		}
	}
	for _, r := range navTree {
		walk(r.Items)
	}
}

// needAny: signed in AND level >= lvl on at least one of the modules.
func (a *App) needAny(lvl int, modules ...string) func(http.HandlerFunc) http.HandlerFunc {
	return func(next http.HandlerFunc) http.HandlerFunc {
		return a.authed(func(w http.ResponseWriter, r *http.Request) {
			c := userFrom(r)
			for _, m := range modules {
				if a.level(c.Role, m) >= lvl {
					next(w, r)
					return
				}
			}
			writeErr(w, 403, "you don't have permission for this")
		})
	}
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	api := NewLimiter(60, 200)   // per IP: 60 req/s sustained, bursts of 200
	login := NewLimiter(0.5, 10) // per IP: 1 login / 2s, burst 10
	public := NewLimiter(5, 40)  // public (QR / tracking) endpoints
	L := func(h http.HandlerFunc) http.HandlerFunc { return a.limit(api, h) }
	any_ := a.authed
	mod := a.need

	// ---------- public ----------
	mux.HandleFunc("GET /api/config", a.limit(api, a.handleConfig))
	mux.HandleFunc("POST /api/auth/login", a.limit(login, a.handleLogin))
	mux.HandleFunc("POST /api/auth/totp", a.limit(login, a.handleTOTPLogin))
	mux.HandleFunc("POST /api/auth/demo", a.limit(login, a.handleDemoLogin))
	mux.HandleFunc("POST /api/auth/forgot", a.limit(login, a.handleForgot))
	mux.HandleFunc("POST /api/auth/reset", a.limit(login, a.handleReset))
	mux.HandleFunc("GET /api/auth/demo-accounts", L(a.handleDemoAccounts))
	mux.HandleFunc("GET /api/public/qr", a.limit(public, a.handleQRMenu))
	mux.HandleFunc("POST /api/public/qr/order", a.limit(public, a.handleQROrder))
	mux.HandleFunc("GET /api/public/qr/order", a.limit(public, a.handleQROrderStatus))
	mux.HandleFunc("POST /api/public/qr/call", a.limit(public, a.handleQRCall))
	mux.HandleFunc("GET /api/public/track", a.limit(public, a.handleTrack))
	mux.HandleFunc("GET /api/display", a.limit(public, a.handleDisplayGet))
	mux.HandleFunc("GET /media/{id}/{name}", a.handleMediaFile)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /manifest.webmanifest", a.handleManifest)

	// ---------- any signed-in user ----------
	mux.HandleFunc("GET /api/me", L(any_(a.handleMe)))
	mux.HandleFunc("POST /api/me/password", L(any_(a.handleChangePassword)))
	mux.HandleFunc("POST /api/me/2fa/setup", L(any_(a.handle2FASetup)))
	mux.HandleFunc("POST /api/me/2fa/enable", L(any_(a.handle2FAEnable)))
	mux.HandleFunc("POST /api/me/2fa/disable", L(any_(a.handle2FADisable)))
	mux.HandleFunc("GET /api/me/prefs/{key}", L(any_(a.handlePrefGet)))
	mux.HandleFunc("PUT /api/me/prefs/{key}", L(any_(a.handlePrefPut)))
	mux.HandleFunc("GET /api/schema", L(any_(a.handleSchema)))
	mux.HandleFunc("GET /api/events", any_(a.hub.handle))
	mux.HandleFunc("GET /api/notifications", L(any_(a.handleNotifications)))
	mux.HandleFunc("GET /api/menu", L(any_(a.handleMenu)))
	mux.HandleFunc("GET /api/qrcode", L(any_(a.handleQRImage)))
	mux.HandleFunc("GET /api/lookup/{res}", L(any_(a.rLookup)))
	mux.HandleFunc("GET /api/users/brief", L(any_(a.handleUsersBrief)))
	mux.HandleFunc("GET /api/hr/my", L(any_(a.handleMyHR)))
	mux.HandleFunc("POST /api/hr/my/leave", L(any_(a.handleMyLeave)))
	mux.HandleFunc("POST /api/hr/checkin", L(any_(a.handleCheckIn)))
	mux.HandleFunc("POST /api/hr/checkout", L(any_(a.handleCheckOut)))

	// ---------- generic resources (permissions checked per resource) ----------
	mux.HandleFunc("GET /api/r/{res}", L(any_(a.rList)))
	mux.HandleFunc("POST /api/r/{res}", L(any_(a.rCreate)))
	mux.HandleFunc("GET /api/r/{res}/{id}", L(any_(a.rGet)))
	mux.HandleFunc("PUT /api/r/{res}/{id}", L(any_(a.rUpdate)))
	mux.HandleFunc("DELETE /api/r/{res}/{id}", L(any_(a.rDelete)))
	mux.HandleFunc("POST /api/r/{res}/{id}/{action}", L(any_(a.rAction)))

	// ---------- settings & administration ----------
	mux.HandleFunc("GET /api/settings/{group}", L(any_(a.handleGetSetting)))
	mux.HandleFunc("PUT /api/settings/{group}", L(any_(a.handlePutSetting)))
	mux.HandleFunc("GET /api/services", L(mod("settings", LvView)(a.handleServices)))
	mux.HandleFunc("GET /api/roles", L(mod("admin", LvView)(a.handleRoles)))
	mux.HandleFunc("POST /api/roles", L(mod("admin", LvApprove)(a.global(a.handleSaveRole))))
	mux.HandleFunc("PUT /api/roles/{key}", L(mod("admin", LvApprove)(a.global(a.handleSaveRole))))
	mux.HandleFunc("DELETE /api/roles/{key}", L(mod("admin", LvApprove)(a.global(a.handleDeleteRole))))
	mux.HandleFunc("GET /api/users", L(mod("admin", LvView)(a.handleUsers)))
	mux.HandleFunc("POST /api/users", L(mod("admin", LvManage)(a.handleCreateUser)))
	mux.HandleFunc("PUT /api/users/{id}", L(mod("admin", LvManage)(a.handleUpdateUser)))
	mux.HandleFunc("POST /api/users/{id}/reset-link", L(mod("admin", LvManage)(a.handleUserResetLink)))
	mux.HandleFunc("GET /api/audit", L(mod("admin", LvView)(a.handleAuditLog)))
	mux.HandleFunc("GET /api/authlog", L(mod("admin", LvView)(a.global(a.handleAuthLog))))
	mux.HandleFunc("POST /api/media", L(a.needAny(LvManage, "menu", "admin", "settings", "hr")(a.handleUpload)))
	mux.HandleFunc("GET /api/media", L(a.needAny(LvView, "menu", "admin", "settings")(a.handleMediaList)))
	mux.HandleFunc("DELETE /api/media/{id}", L(mod("admin", LvManage)(a.handleMediaDelete)))
	mux.HandleFunc("GET /api/db/info", L(mod("admin", LvApprove)(a.global(a.handleDBInfo))))
	mux.HandleFunc("POST /api/db/backup", L(mod("admin", LvApprove)(a.global(a.handleDBBackup))))
	mux.HandleFunc("POST /api/db/optimize", L(mod("admin", LvApprove)(a.global(a.handleDBOptimize))))
	mux.HandleFunc("GET /api/db/backup/{name}", mod("admin", LvApprove)(a.global(a.handleDBDownload)))
	mux.HandleFunc("DELETE /api/db/backup/{name}", mod("admin", LvApprove)(a.global(a.handleDBDelete)))

	// ---------- POS / orders / kitchen ----------
	view := a.needAny(LvView, "pos", "orders", "kitchen")
	mux.HandleFunc("POST /api/pos/orders", L(mod("pos", LvManage)(a.handleCreateOrder)))
	mux.HandleFunc("GET /api/pos/orders", L(view(a.handleOrders)))
	mux.HandleFunc("GET /api/pos/orders/{id}", L(view(a.handleGetOrder)))
	mux.HandleFunc("POST /api/pos/orders/{id}/pay", L(mod("orders", LvManage)(a.handlePay)))
	mux.HandleFunc("POST /api/pos/orders/{id}/status", L(a.needAny(LvManage, "pos", "kitchen", "orders")(a.handleOrderStatus)))
	mux.HandleFunc("POST /api/pos/orders/{id}/cancel", L(mod("pos", LvManage)(a.handleCancel)))
	mux.HandleFunc("POST /api/pos/orders/{id}/refund", L(mod("orders", LvApprove)(a.handleRefund)))
	mux.HandleFunc("POST /api/pos/items/{id}/status", L(a.needAny(LvManage, "pos", "kitchen")(a.handleItemStatus)))
	mux.HandleFunc("POST /api/pos/items/{id}/void", L(mod("pos", LvManage)(a.handleVoidItem)))
	mux.HandleFunc("GET /api/pos/kds", L(a.needAny(LvView, "kitchen", "pos")(a.handleKDS)))
	mux.HandleFunc("GET /api/pos/tables", L(a.needAny(LvView, "pos", "floor", "reservations")(a.handleTableBoard)))
	mux.HandleFunc("POST /api/pos/tables/{id}/status", L(mod("pos", LvManage)(a.handleTableStatus)))
	mux.HandleFunc("GET /api/pos/session", L(mod("pos", LvView)(a.handleSessionGet)))
	mux.HandleFunc("POST /api/pos/session/open", L(mod("pos", LvManage)(a.handleSessionOpen)))
	mux.HandleFunc("POST /api/pos/session/close", L(mod("pos", LvManage)(a.handleSessionClose)))
	mux.HandleFunc("GET /api/pos/payments", L(mod("orders", LvView)(a.handlePayments)))
	mux.HandleFunc("GET /api/pos/display-key", L(mod("pos", LvView)(a.handleDisplayKey)))
	mux.HandleFunc("POST /api/pos/display", L(mod("pos", LvManage)(a.handleDisplayPush)))
	mux.HandleFunc("GET /api/dash/hub", L(a.authed(func(w http.ResponseWriter, r *http.Request) {
		c := userFrom(r)
		if a.level(c.Role, "orders") < LvManage && a.level(c.Role, "reports") < LvView && a.level(c.Role, "accounting") < LvView {
			writeErr(w, 403, "you don't have permission for this")
			return
		}
		a.handleHubDash(w, r)
	})))

	// ---------- inventory & customers ----------
	mux.HandleFunc("GET /api/stock", L(a.needAny(LvView, "inventory", "purchasing", "accounting")(a.handleStock)))
	mux.HandleFunc("GET /api/stock/moves", L(mod("inventory", LvView)(a.handleStockMoves)))
	mux.HandleFunc("GET /api/inventory/overview", L(mod("inventory", LvView)(a.handleInventoryOverview)))
	mux.HandleFunc("POST /api/customers/{id}/points", L(mod("customers", LvManage)(a.handlePointsAdjust)))
	mux.HandleFunc("GET /api/customers/analytics", L(mod("customers", LvView)(a.handleCustomerAnalytics)))

	// ---------- HR ----------
	mux.HandleFunc("GET /api/hr/dash", L(a.needAny(LvView, "hr", "attendance", "payroll")(a.handleHRDash)))
	mux.HandleFunc("GET /api/hr/attendance", L(mod("attendance", LvView)(a.handleAttList)))
	mux.HandleFunc("POST /api/hr/attendance", L(mod("attendance", LvManage)(a.handleAttSave)))
	mux.HandleFunc("DELETE /api/hr/attendance/{id}", L(mod("attendance", LvManage)(a.handleAttDelete)))
	mux.HandleFunc("POST /api/hr/attendance/generate", L(mod("attendance", LvManage)(a.handleAttGenerate)))
	mux.HandleFunc("GET /api/hr/attendance/summary", L(mod("attendance", LvView)(a.handleAttSummary)))
	mux.HandleFunc("GET /api/hr/leave-balances", L(mod("attendance", LvView)(a.handleLeaveBalances)))
	mux.HandleFunc("GET /api/hr/reports/leave", L(mod("attendance", LvView)(a.handleLeaveReport)))
	mux.HandleFunc("GET /api/hr/payroll-analytics", L(mod("payroll", LvView)(a.handlePayrollAnalytics)))

	// ---------- delivery ----------
	mux.HandleFunc("GET /api/delivery/dash", L(mod("delivery", LvView)(a.handleDeliveryDash)))
	mux.HandleFunc("GET /api/delivery/finance", L(mod("delivery", LvView)(a.handleDeliveryFinance)))
	mux.HandleFunc("GET /api/delivery/live", L(mod("delivery", LvView)(a.handleDeliveryLive)))
	mux.HandleFunc("POST /api/delivery/{id}/assign", L(mod("delivery", LvManage)(a.handleDeliveryAssign)))
	mux.HandleFunc("POST /api/delivery/{id}/status", L(mod("delivery", LvManage)(a.handleDeliveryStatus)))
	mux.HandleFunc("POST /api/delivery/location", L(mod("delivery", LvManage)(a.handleDriverLocation)))

	// ---------- accounting ----------
	mux.HandleFunc("GET /api/acc/dash", L(mod("accounting", LvView)(a.handleAccDash)))
	mux.HandleFunc("GET /api/acc/accounts", L(a.needAny(LvView, "accounting")(a.handleAccounts)))
	mux.HandleFunc("POST /api/acc/accounts", L(mod("accounting", LvApprove)(a.global(a.handleSaveAccount))))
	mux.HandleFunc("PUT /api/acc/accounts/{id}", L(mod("accounting", LvApprove)(a.global(a.handleSaveAccount))))
	mux.HandleFunc("GET /api/acc/journals", L(mod("accounting", LvView)(a.handleJournals)))
	mux.HandleFunc("GET /api/acc/journals/{id}", L(mod("accounting", LvView)(a.handleJournal)))
	mux.HandleFunc("POST /api/acc/journals", L(mod("accounting", LvManage)(a.handleCreateJournal)))
	mux.HandleFunc("POST /api/acc/journals/{id}/post", L(mod("accounting", LvManage)(a.handlePostJournal)))
	mux.HandleFunc("POST /api/acc/journals/{id}/reverse", L(mod("accounting", LvApprove)(a.handleReverseJournal)))
	mux.HandleFunc("DELETE /api/acc/journals/{id}", L(mod("accounting", LvManage)(a.handleDeleteJournal)))
	mux.HandleFunc("POST /api/acc/opening", L(mod("accounting", LvApprove)(a.global(a.handleOpening))))
	mux.HandleFunc("GET /api/acc/report/pnl", L(mod("accounting", LvView)(a.handleReportPnL)))
	mux.HandleFunc("GET /api/acc/report/bs", L(mod("accounting", LvView)(a.handleReportBS)))
	mux.HandleFunc("GET /api/acc/report/cash", L(mod("accounting", LvView)(a.handleReportCash)))
	mux.HandleFunc("GET /api/acc/report/trial", L(mod("accounting", LvView)(a.handleTrialBalance)))
	mux.HandleFunc("GET /api/acc/report/ledger", L(mod("accounting", LvView)(a.handleLedger)))
	mux.HandleFunc("GET /api/acc/budget-vs-actual", L(mod("accounting", LvView)(a.handleBudgetActual)))
	mux.HandleFunc("GET /api/acc/foodcost", L(mod("accounting", LvView)(a.handleFoodCost)))
	mux.HandleFunc("GET /api/acc/menu-profit", L(mod("accounting", LvView)(a.handleMenuProfit)))
	mux.HandleFunc("GET /api/acc/tips", L(mod("accounting", LvView)(a.handleTips)))
	mux.HandleFunc("GET /api/acc/supplier-statements", L(mod("accounting", LvView)(a.handleSupplierStatements)))
	mux.HandleFunc("GET /api/acc/inventory-valuation", L(mod("accounting", LvView)(a.handleInvValuation)))
	mux.HandleFunc("GET /api/acc/consolidation", L(mod("accounting", LvView)(a.handleConsolidation)))
	mux.HandleFunc("GET /api/acc/unposted", L(mod("accounting", LvView)(a.handleUnposted)))
	mux.HandleFunc("POST /api/acc/retry-post", L(mod("accounting", LvManage)(a.handleRetryPost)))
	mux.HandleFunc("GET /api/acc/export", mod("accounting", LvView)(a.handleAccExport))
	mux.HandleFunc("POST /api/acc/bank-import", L(mod("accounting", LvManage)(a.handleBankImport)))
	mux.HandleFunc("POST /api/acc/e-invoice", L(mod("accounting", LvManage)(a.handleEInvoiceFromOrder)))

	// ---------- support tickets ----------
	mux.HandleFunc("POST /api/support", L(any_(a.handleSupportCreate)))
	mux.HandleFunc("GET /api/support/mine", L(any_(a.handleSupportMine)))
	mux.HandleFunc("GET /api/support", L(mod("support", LvView)(a.handleSupportList)))
	mux.HandleFunc("GET /api/support/{id}", L(any_(a.handleSupportGet)))
	mux.HandleFunc("POST /api/support/{id}/reply", L(any_(a.handleSupportReply)))
	mux.HandleFunc("POST /api/support/{id}/status", L(mod("support", LvManage)(a.handleSupportStatus)))

	// ---------- reports ----------
	mux.HandleFunc("GET /api/rep/{kind}", L(mod("reports", LvView)(a.handleReport)))

	// ---------- no screens here: this program is the API only ----------
	// The screens live in ../frontend (its own program). For a one-process setup you can
	// point FRONTEND_DIR at that folder and this server will hand the files out as well.
	mux.Handle("/", a.frontendOrInfo())
	return mux
}

func (a *App) handleManifest(w http.ResponseWriter, r *http.Request) {
	p := a.settingGroup("pwa")
	name := a.settingStr("application", "app_name", a.cfg.Restaurant)
	m := map[string]any{"name": name, "short_name": a.settingStr("pwa", "short_name", "TableBill"), "start_url": "/app", "scope": "/", "display": "standalone",
		"background_color": "#ffffff", "theme_color": toStr(p["theme_color"]), "icons": []any{map[string]any{"src": "/icon.svg", "sizes": "any", "type": "image/svg+xml", "purpose": "any maskable"}}}
	w.Header().Set("Content-Type", "application/manifest+json")
	json.NewEncoder(w).Encode(m)
}

// warnDefaults: loud warnings when the demo logins are still wide open.
func (a *App) warnDefaults() {
	if a.cfg.Demo {
		log.Println("!!! DEMO MODE IS ON: anyone who can reach this server can sign in as any demo user without a password. Never use DEMO=1 on a real restaurant or on the internet.")
		return
	}
	go func() {
		rows := a.queryMaps(`SELECT username,pass_hash FROM users WHERE active=1`)
		for _, m := range rows {
			if isDemoUser(toStr(m["username"])) {
				if ok, _ := checkPassword(toStr(m["pass_hash"]), "demo123"); ok {
					log.Printf("WARNING: user %q still has the demo password. Change it (Users page) or disable the account.", m["username"])
					a.notify(RoleAdmin, 0, "warn", "Demo password still active", "User "+toStr(m["username"])+" still uses the demo password 'demo123'. Change it now.", "#/users")
				}
			}
		}
	}()
}

// migratePrintingPerm (runs once): databases created before the "Printing" permission
// existed keep working - cashier, manager, kitchen and POS operator get it; waiters do not.
func (a *App) migratePrintingPerm() {
	var done string
	if a.db.rdb.QueryRow(`SELECT value FROM settings WHERE key='migrated_printing'`).Scan(&done) == nil {
		return
	}
	for _, r := range defaultRoles {
		if r.Perms["printing"] == 0 {
			continue
		}
		var pj string
		if a.db.rdb.QueryRow(`SELECT perms FROM roles WHERE key=?`, r.Key).Scan(&pj) != nil {
			continue
		}
		cur := map[string]int{}
		json.Unmarshal([]byte(pj), &cur)
		if _, has := cur["printing"]; !has {
			cur["printing"] = r.Perms["printing"]
			a.db.wdb.Exec(`UPDATE roles SET perms=? WHERE key=?`, string(jsonBytes(cur)), r.Key)
		}
	}
	a.db.wdb.Exec(`INSERT OR IGNORE INTO settings(key,value) VALUES('migrated_printing','1')`)
}

func (a *App) frontendOrInfo() http.Handler {
	dir := os.Getenv("FRONTEND_DIR")
	if dir == "" {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				writeErr(w, 404, "unknown API route")
				return
			}
			writeJSON(w, 200, map[string]any{"service": "TableBill API", "status": "running",
				"note": "This is the backend only. Start the frontend (folder 'frontend') and open it in the browser."})
		})
	}
	files := http.FileServer(http.Dir(dir))
	pages := map[string]string{"/login": "/login.html", "/app": "/app.html", "/qr": "/qr.html", "/track": "/track.html", "/display": "/display.html"}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeErr(w, 404, "unknown API route")
			return
		}
		if p, ok := pages[r.URL.Path]; ok {
			r.URL.Path = p
		}
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
}
