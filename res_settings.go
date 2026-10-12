package main

import (
	"database/sql"
	"net/http"
	"time"
)

func init() {
	def("taxes", "settings", "Taxes", `name*^~; rate|Rate %:number*^; kind:select(percent)=percent; default|Default for new products:bool^; status:select(active|inactive)^=active`,
		Hint("Tax rates you can attach to products (GST, VAT, service tax ...). The default one is used when a product has none."))
	def("currencies", "settings", "Currency rates", `code#*^~; name*^; symbol; rate|Rate to base currency:number*^=1; as_of:date; status:select(active|inactive)^=active`,
		Label("{code}"), Hint("Exchange rates against your base currency, used by the foreign-exchange controls in Accounting."))
	def("translations", "settings", "Translations", `lang:select(en|hi|ar)*^; key*^~; text:textarea*^~`, Label("{lang}:{key}"),
		Hint("Override any screen text for a language. Use the key shown in the app, e.g. nav.pos."),
		BeforeSave(func(a *App, tx *sql.Tx, c *Claims, r *Rec, old *Rec) error {
			r.Ref = r.S("lang") + "|" + r.S("key")
			return nil
		}),
		AfterSave(func(a *App, tx *sql.Tx, c *Claims, r *Rec, old *Rec) error {
			time.AfterFunc(600*time.Millisecond, func() { a.cfgPublic.Store(nil) })
			return nil
		}))
	def("notif_templates", "settings", "Notification templates",
		`event:select(new_order|low_stock|leave_request|leave_approved|payment_failed|delivery_delay|password_reset|reservation_confirmed|daily_summary)*^; channel:select(email|sms|push|in_app|whatsapp)*^; subject:text^; body:textarea*; status:select(active|inactive)^=active`,
		Label("{event} / {channel}"), Hint("Wording for each notification. Use {name}, {order}, {amount} placeholders."))
	def("printers", "settings", "Printers", `name*^~; kind:select(receipt|kitchen|label)*^; connection:select(browser|network|usb|bluetooth)=browser; address:text; paper_width|Paper width (mm):int=80; station:ref(stations); status:select(active|inactive)^=active`, Scoped,
		Hint("Receipt and kitchen printers. 'browser' prints through the browser's print dialog."))
	def("print_agents", "settings", "Print agents", `name*^~; token:text.; last_seen:datetime.; status:select(online|offline)^=offline`, Scoped,
		Hint("Small helper programs on a shop PC that print silently. Each agent gets its own key."),
		BeforeSave(func(a *App, tx *sql.Tx, c *Claims, r *Rec, old *Rec) error {
			if old == nil {
				r.Set("token", randHex(16))
			}
			return nil
		}))
	def("exports", "reports", "Export history", `name*^~; resource; rows:int^; user^; status:select(done)=done`, NoCreate, Label("{name}"))
}

func (a *App) recordExport(c *Claims, res string, rows int) {
	mk(a.db.wdb, "exports", c.Branch, map[string]any{"name": res + ".csv", "resource": res, "rows": float64(rows), "user": c.User})
}

// Services & integrations page: a quick view of which channels are on.
func (a *App) handleServices(w http.ResponseWriter, r *http.Request) {
	s := a.settingGroup("services")
	e := a.settingGroup("email")
	rows := []map[string]any{
		{"key": "email", "name": "Email", "desc": "Send emails using the SMTP details you provide.", "type": "SMTP", "active": toBool(s["email_on"]) && toBool(e["enabled"]), "configured": toStr(e["host"]) != "", "link": "email"},
		{"key": "inapp", "name": "In-app", "desc": "Notifications inside the bell icon. No provider needed.", "type": "Built-in", "active": toBool(s["inapp_on"]), "configured": true},
		{"key": "push", "name": "Push notification", "desc": "Phone/browser pushes through Firebase Cloud Messaging.", "type": "FCM", "active": toBool(s["push_on"]), "configured": toStr(s["fcm_server_key"]) != ""},
		{"key": "sms", "name": "SMS", "desc": "Text messages through your SMS provider.", "type": toStr(s["sms_provider"]), "active": toBool(s["sms_on"]), "configured": toStr(s["sms_api_key"]) != ""},
		{"key": "whatsapp", "name": "WhatsApp", "desc": "Order updates on WhatsApp Business.", "type": "WhatsApp", "active": toBool(s["whatsapp_on"]), "configured": toStr(s["whatsapp_token"]) != ""},
		{"key": "webhook", "name": "Order webhook", "desc": "Call your URL when a new order arrives.", "type": "HTTP", "active": toStr(s["webhook_url"]) != "", "configured": toStr(s["webhook_url"]) != ""},
	}
	writeJSON(w, 200, rows)
}
