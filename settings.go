package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
)

// Settings are small key/value groups edited on the Settings screens.
// Each group is declared with the same compact field spec as resources.

type SettingGroup struct {
	Key    string  `json:"key"`
	Title  string  `json:"title"`
	Hint   string  `json:"hint,omitempty"`
	Module string  `json:"module"`
	Fields []Field `json:"fields"`
}

var (
	settingGroups = map[string]*SettingGroup{}
	settingOrder  []string
)

func setting(key, title, module, hint, spec string) {
	g := &SettingGroup{Key: key, Title: title, Module: module, Hint: hint, Fields: parseSpec(spec)}
	settingGroups[key] = g
	settingOrder = append(settingOrder, key)
}

type settingsCache struct {
	p atomic.Pointer[map[string]map[string]any]
}

func (a *App) loadSettings() {
	all := map[string]map[string]any{}
	for k, g := range settingGroups {
		m := map[string]any{}
		for _, f := range g.Fields {
			if f.Def != nil {
				m[f.Key] = f.Def
			}
		}
		all[k] = m
	}
	rows, err := a.db.rdb.Query(`SELECT key,value FROM settings`)
	if err == nil {
		for rows.Next() {
			var k, v string
			rows.Scan(&k, &v)
			var m map[string]any
			if json.Unmarshal([]byte(v), &m) == nil {
				if all[k] == nil {
					all[k] = map[string]any{}
				}
				for kk, vv := range m {
					all[k][kk] = vv
				}
			}
		}
		rows.Close()
	}
	a.cfgCache.p.Store(&all)
	a.cfgPublic.Store(nil)
}

func (a *App) settingGroup(g string) map[string]any {
	if m := a.cfgCache.p.Load(); m != nil {
		if x := (*m)[g]; x != nil {
			return x
		}
	}
	return map[string]any{}
}

func (a *App) settingStr(g, k, def string) string {
	if v, ok := a.settingGroup(g)[k]; ok && toStr(v) != "" {
		return toStr(v)
	}
	return def
}
func (a *App) settingNum(g, k string, def float64) float64 {
	if v, ok := a.settingGroup(g)[k]; ok {
		return toFloat(v)
	}
	return def
}
func (a *App) settingBool(g, k string, def bool) bool {
	if v, ok := a.settingGroup(g)[k]; ok {
		return toBool(v)
	}
	return def
}

const maskedPw = "********"

func (a *App) handleGetSetting(w http.ResponseWriter, r *http.Request) {
	g := settingGroups[r.PathValue("group")]
	c := userFrom(r)
	if g == nil || a.level(c.Role, g.Module) < LvView {
		writeErr(w, 404, "not found")
		return
	}
	out := map[string]any{}
	for k, v := range a.settingGroup(g.Key) {
		out[k] = v
	}
	for _, f := range g.Fields {
		if f.Type == "password" && toStr(out[f.Key]) != "" {
			out[f.Key] = maskedPw
		}
	}
	writeJSON(w, 200, out)
}

func (a *App) handlePutSetting(w http.ResponseWriter, r *http.Request) {
	g := settingGroups[r.PathValue("group")]
	c := userFrom(r)
	if g == nil || a.level(c.Role, g.Module) < LvManage || c.Branch != 0 {
		writeErr(w, 403, "you don't have permission for this")
		return
	}
	var body map[string]any
	if !readJSON(w, r, &body) {
		return
	}
	old := a.settingGroup(g.Key)
	for _, f := range g.Fields {
		if f.Type == "password" && toStr(body[f.Key]) == maskedPw {
			body[f.Key] = old[f.Key]
		}
	}
	clean, err := cleanFields(g.Fields, body, old)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	b, _ := json.Marshal(clean)
	if _, err := a.db.wdb.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, g.Key, string(b)); err != nil {
		writeErr(w, 500, "could not save")
		return
	}
	a.loadSettings()
	a.audit(r, "settings", 0, "update", g.Title)
	a.hub.Publish("settings")
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// publicConfig is the non-secret subset every screen needs (cached).
func (a *App) publicConfig() []byte {
	if b := a.cfgPublic.Load(); b != nil {
		return *b
	}
	ap, cur, pos := a.settingGroup("application"), a.settingGroup("currency"), a.settingGroup("pos")
	ar := a.settingGroup("appearance")
	m := map[string]any{
		"restaurant": a.settingStr("application", "app_name", a.cfg.Restaurant),
		"tagline":    ap["tagline"], "language": a.settingStr("application", "default_language", "en"),
		"timezone": a.settingStr("application", "timezone", "Asia/Kolkata"),
		"currency": map[string]any{"code": a.settingStr("currency", "code", "INR"), "symbol": a.settingStr("currency", "symbol", a.cfg.Currency),
			"position": cur["position"], "decimals": a.settingNum("currency", "decimals", 2)},
		"color": a.settingStr("appearance", "primary_color", "#f97316"), "logo": ar["logo"], "dark": ar["dark_mode"],
		"demo": a.cfg.Demo,
		"pos": map[string]any{"tips": pos["tip_options"], "hold": pos["enable_hold"], "discount": pos["allow_discount"],
			"service": pos["service_charge_pct"], "inclusive": pos["tax_inclusive"], "max_discount": pos["max_discount_pct"],
			"show_cashier": a.settingBool("pos", "show_cashier", true), "show_customer": a.settingBool("pos", "show_customer", true), "bill_barcode": a.settingBool("pos", "bill_barcode", true), "bill_qr": a.settingBool("pos", "bill_qr", true), "qr_text": pos["bill_qr_text"], "header": pos["receipt_header"], "footer": pos["receipt_footer"], "autoprint": pos["auto_print_receipt"]},
		"app":        map[string]any{"phone": ap["phone"], "address": ap["address"], "tax_id": ap["tax_id"], "footer": ap["footer_text"]},
		"kitchen":    map[string]any{"warn": a.settingNum("kitchen", "timer_warn_min", 10), "late": a.settingNum("kitchen", "timer_late_min", 20), "sound": a.settingBool("kitchen", "sound", true), "auto_print": a.settingBool("kitchen", "auto_print", false), "show_timer": a.settingBool("kitchen", "show_timer", false)},
		"pdf":        map[string]any{"paper": a.settingStr("pdf", "paper", "80mm")},
		"hr_geo":     a.settingBool("hr", "geofence_required", false),
		"qr_enabled": a.settingBool("qr", "enabled", true),
		"pwa":        a.settingGroup("pwa"),
		"recovery":   a.settingBool("recovery", "enabled", true),
		"passkeys":   a.settingBool("passkeys", "enabled", false),
		"tz_offset":  a.tzOffsetMin(),
	}
	dict := map[string]string{}
	lang := a.settingStr("application", "default_language", "en")
	for _, tr := range recs(a.db.rdb, "translations", 0) {
		if tr.S("lang") == lang {
			dict[tr.S("key")] = tr.S("text")
		}
	}
	m["i18n"] = dict
	b, _ := json.Marshal(m)
	a.cfgPublic.Store(&b)
	return b
}

func (a *App) handleConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=30")
	w.Write(a.publicConfig())
}

// ---------- group definitions ----------

func init() {
	setting("application", "Application", "settings", "Your restaurant's name and basic details.",
		`app_name|Restaurant name:text*=TableBill Restaurant; tagline:text; app_url|Public website URL:url; phone:tel; email:email; address:textarea; tax_id|Tax / GST number:text;
		timezone:select(Asia/Kolkata|Asia/Dubai|Asia/Amman|Asia/Riyadh|Asia/Karachi|Asia/Dhaka|Europe/London|America/New_York|UTC)=Asia/Kolkata;
		default_language:select(en|hi|ar)=en; date_format:select(DD/MM/YYYY|MM/DD/YYYY|YYYY-MM-DD)=DD/MM/YYYY; footer_text:text=Thank you! Visit again.`)
	setting("appearance", "Appearance & Logo", "settings", "Colours and logo shown across the app and on receipts.",
		`primary_color|Brand colour:color=#f97316; logo|Logo (image URL or upload id):file; favicon:file; dark_mode:select(auto|light|dark)=auto; login_banner|Login page message:text`)
	setting("currency", "Currency", "settings", "How money is shown. Amounts are stored with 2 decimals.",
		`code:text*=INR; symbol:text*=₹; position:select(before|after)=before; decimals:int=2`)
	setting("pos", "Point of Sale", "settings", "Billing rules used on the POS screen.",
		`tax_inclusive|Prices already include tax:bool=false; service_charge_pct|Service charge %:number=0; allow_discount:bool=true; max_discount_pct|Max discount % (cashier/waiter):number=20;
		enable_hold|Allow holding orders:bool=true; tip_options|Tip buttons (comma separated %):text=5,10,15; require_customer_for_delivery:bool=true;
		auto_print_receipt:bool=false; show_cashier|Print cashier name on the bill:bool=true; show_customer|Print customer name on the bill:bool=true; bill_barcode|Print a barcode of the order number on the bill:bool=true; bill_qr|Print a QR code on the bill:bool=true; bill_qr_text|QR content (e.g. your review / website link; empty = order number):text; round_off|Round total to nearest whole unit:bool=false; allow_negative_stock|Allow selling when stock is 0:bool=true;
		loyalty_earn|Loyalty points per 100 spent:number=5; loyalty_value|Value of 1 point (in money):number=1; receipt_header:textarea; receipt_footer:textarea=Thank you! Visit again.`)
	setting("kitchen", "Kitchen (KDS)", "settings", "Kitchen display behaviour.",
		`timer_warn_min|Warn after (minutes):int=10; timer_late_min|Late after (minutes):int=20; sound|Play sound for new tickets:bool=true; show_options|Show item options on ticket:bool=true; auto_ready|Mark order ready when all items are ready:bool=true; auto_print|Auto-print new kitchen tickets (default for kitchen screens):bool=false; show_timer|Show minutes on kitchen tickets (otherwise only the order number):bool=false`)
	setting("qr", "Table QR Ordering", "settings", "Guests scan a QR code at the table and order from their phone.",
		`enabled:bool=true; require_phone|Ask for phone number:bool=false; auto_accept|Send straight to kitchen (no staff approval):bool=true; welcome_text:text=Welcome! Choose your dishes and we will bring them to your table.; allow_call_waiter:bool=true`)
	setting("pwa", "PWA (install as app)", "settings", "Lets staff install TableBill on a phone or tablet.",
		`installable:bool=true; short_name:text=TableBill; theme_color:color=#f97316`)
	setting("two_factor", "Two-factor authentication", "settings", "Staff can protect their login with an authenticator app (Google Authenticator, Authy ...).",
		`enabled|Allow two-factor:bool=true`)
	setting("passkeys", "Passkeys", "settings", "Sign in with fingerprint / face / security key (WebAuthn).",
		`enabled|Allow passkey sign-in:bool=false; rp_name|Name shown to users:text=TableBill`)
	setting("recovery", "Password recovery", "settings", "Lets staff request a password-reset link.",
		`enabled|Allow "Forgot password":bool=true; token_ttl_min|Link valid for (minutes):int=60`)
	setting("filesystem", "Filesystem & uploads", "settings", "Where uploaded images are stored.",
		`driver:select(local)=local; max_upload_mb|Max upload size (MB):int=5; allowed|Allowed types:text=png,jpg,jpeg,webp,gif,pdf`)
	setting("pdf", "PDF & receipts", "settings", "Layout used when printing invoices and reports.",
		`paper|Paper (receipts print on the roll size):select(80mm|58mm|A5|A4)=80mm; orientation:select(portrait|landscape)=portrait; show_logo:bool=true; header:textarea; footer:textarea`)
	setting("email", "Email (SMTP)", "settings", "Needed to e-mail password resets and reports.",
		`enabled:bool=false; host:text; port:int=587; username:text; password:password; from|From address:email; encryption:select(none|starttls|tls)=starttls`)
	setting("services", "Services & Integrations", "settings", "Turn notification channels on or off.",
		`email_on|Email:bool=false; inapp_on|In-app:bool=true; push_on|Push (Firebase):bool=false; sms_on|SMS:bool=false; whatsapp_on|WhatsApp:bool=false;
		fcm_server_key|Firebase server key:password; sms_provider:select(none|twilio|msg91|custom)=none; sms_api_key:password; sms_sender:text; whatsapp_token:password; webhook_url|Webhook for new orders:url`)
	setting("notifications", "Notification settings", "settings", "Which events create notifications.",
		`new_order:bool=true; order_ready|Tell the waiter when an order is ready:bool=true; low_stock:bool=true; leave_request:bool=true; payment_failed:bool=true; delivery_delay:bool=true; daily_summary|E-mail a daily sales summary:bool=false`)
	setting("reservation", "Reservation settings", "settings", "Rules for the booking screen.",
		`slot_minutes|Table duration (minutes):int=90; max_party|Largest party:int=12; advance_days|Book up to (days ahead):int=30; auto_confirm:bool=false; hold_minutes|Hold table for late guests (min):int=15`)
	setting("printing", "Printing", "settings", "Receipt and kitchen ticket printing.",
		`default_printer:text; auto_print_kitchen|Print kitchen ticket automatically:bool=false; copies:int=1; paper_width|Paper width (mm):int=80`)
	setting("accounting", "Accounting settings", "accounting", "Defaults for the ledger. Auto-posting turns every sale, purchase and payroll into journal entries.",
		`auto_post|Auto-post sales, purchases and payroll:bool=true; post_cogs|Post cost of goods sold with each sale:bool=true; costing:select(average)=average; fiscal_start_month:int=1; base_currency:text=INR; require_balanced:bool=true`)
	setting("hr", "HR & attendance policy", "attendance", "Working-time rules used by attendance and payroll.",
		`work_hours|Standard hours per day:number=8; late_grace_min|Late grace (minutes):int=10; half_day_hours|Half-day below (hours):number=4; overtime_rate|Overtime multiplier:number=1.5;
		weekly_off|Weekly off day:select(Sunday|Monday|Tuesday|Wednesday|Thursday|Friday|Saturday)=Friday; geofence_required|Require location at check-in:bool=false;
		days_in_month|Payroll days in month (0 = actual days):int=30; unpaid_leave_deduct|Deduct unpaid leave & absence:bool=true`)
	setting("delivery", "Delivery settings", "delivery", "Fees and service levels for deliveries.",
		`base_fee:money=0; free_above|Free delivery above:money=0; per_km_fee|Fee per km:money=0; max_radius_km|Max distance (km):number=10; auto_assign|Auto-assign nearest free driver:bool=false;
		sla_pickup_min|Pickup target (minutes):int=15; sla_delivery_min|Delivery target (minutes):int=45; cod_enabled|Cash on delivery:bool=true; default_method:select(internal|partner)=internal; tracking_interval_sec:int=15`)
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

var _ = strings.TrimSpace
