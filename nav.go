package main

import (
	"net/http"
)

// Navigation mirrors the left rail + sidebar of the product:
// Hub | Delivery | HRMS | Accounting | Reports | Settings

type NavItem struct {
	Title    string    `json:"title"`
	Kind     string    `json:"kind,omitempty"`   // res | page | group
	Target   string    `json:"target,omitempty"` // resource key or page key
	Module   string    `json:"module,omitempty"` // permission module for pages
	Level    int       `json:"level,omitempty"`
	Icon     string    `json:"icon,omitempty"`
	Children []NavItem `json:"children,omitempty"`
}

type Rail struct {
	Key   string    `json:"key"`
	Title string    `json:"title"`
	Icon  string    `json:"icon"`
	Items []NavItem `json:"items"`
}

func R(title, res string) NavItem { return NavItem{Title: title, Kind: "res", Target: res} }
func P(title, page, module string) NavItem {
	return NavItem{Title: title, Kind: "page", Target: page, Module: module, Level: LvView}
}
func G(title string, kids ...NavItem) NavItem {
	return NavItem{Title: title, Kind: "group", Children: kids}
}

var navTree = []Rail{
	{"hub", "Hub", "🏠", []NavItem{
		P("Dashboard", "dashboard", "reports"),
		P("POS", "pos", "pos"),
		P("KDS (Kitchen)", "kds", "kitchen"),
		G("Operations",
			P("Orders overview", "orders", "orders"),
			P("Invoices", "invoices", "orders"),
			P("Payments", "payments", "orders"),
			R("Reasons", "reasons"),
			G("Point of Sale", R("Registers", "registers"), R("Sessions", "pos_sessions"), R("Cash movements", "cash_moves")),
			G("Reservations", R("Reservations", "reservations"), R("Waitlist", "waitlist"), R("Table blocks", "table_blocks")),
			G("Floor & Tables", P("Tables", "tables", "floor"), R("Table merges", "table_merges"), R("Zones", "zones"), R("Floors", "floors")),
		),
		G("Menu & Catalog",
			G("Menus", R("All menus", "menus")),
			R("Categories", "categories"), R("Products", "products"), R("Options", "options"), R("Kitchen stations", "stations"), R("Recipes", "recipes"),
		),
		G("Inventory",
			P("Overview", "inventory", "inventory"), R("Ingredients", "ingredients"), P("Stock", "stock", "inventory"),
			P("Stock movements", "stock_moves", "inventory"), R("Stock transfers", "transfers"), R("Inventory counts", "counts"), R("Waste log", "waste"),
			G("Purchasing", R("Purchases", "purchases"), R("Suppliers", "suppliers")), R("Units", "units"),
		),
		G("Customers & Growth",
			R("Customers", "customers"),
			G("Promotions", R("Discounts", "discounts"), R("Vouchers", "vouchers")),
			G("Loyalty", R("Programs", "loyalty_programs"), R("Tiers", "loyalty_tiers"), R("Rewards", "loyalty_rewards"), P("Customer points", "loyalty", "customers")),
			G("Gift cards", R("Cards", "gift_cards"), R("Transactions", "gift_txns"), R("Breakage batches", "gift_batches")),
			P("Customer analytics", "customer_analytics", "customers"),
		),
		G("Administration",
			R("Branches", "branches"),
			G("Users & Access", P("Users", "users", "admin"), P("Roles", "roles", "admin")),
			G("Activity", P("Activity logs", "audit", "admin"), P("Authentication logs", "authlog", "admin")),
			P("Media", "media", "admin"), P("Database", "database", "admin"), P("Support inbox", "support_admin", "support"),
		),
	}},
	{"delivery", "Delivery", "🛵", []NavItem{
		P("Delivery dashboard", "delivery_dash", "delivery"),
		G("Operations", P("Deliveries", "deliveries", "delivery"), P("Dispatch monitor", "dispatch", "delivery"), P("Live tracking", "tracking", "delivery")),
		G("Driver team", R("Drivers", "drivers"), R("Driver earning rules", "driver_rules")),
		G("Financials", P("Financial analytics", "delivery_fin", "delivery"), R("COD settlements", "cod_settlements"), R("Provider settlements", "provider_settlements"),
			R("Driver earnings", "driver_earnings"), R("Payout batches", "payout_batches"), R("Payout issues", "payout_issues")),
		G("External courier", R("Courier providers", "couriers"), R("Quote history", "courier_quotes"), R("Dispatch request log", "dispatch_log")),
		G("Configuration", P("Delivery settings", "set:delivery", "delivery"), R("Delivery zones", "delivery_zones"), R("Pricing rules", "pricing_rules"), R("Peak surcharges", "peak_surcharges")),
	}},
	{"hrms", "HRMS", "👥", []NavItem{
		P("Dashboard", "hr_dash", "hr"),
		R("Employees", "employees"), R("Departments", "departments"), R("Designations", "designations"),
		R("Shifts", "shifts"), R("Document types", "doc_types"), R("Employee documents", "employee_docs"), R("Holidays", "holidays"),
		P("My attendance & leave", "my_hr", ""),
		G("Leave", R("Leave requests", "leave_requests"), P("Leave approvals", "leave_approvals", "attendance"), P("Leave reports", "leave_reports", "attendance"),
			P("Leave balances", "leave_balances", "attendance"), R("Leave types", "leave_types"), R("Leave policies", "leave_policies"), R("Approval rules", "leave_rules")),
		G("Attendance", P("Records", "attendance", "attendance"), P("Check in / out", "checkin", "attendance"), R("Overtime requests", "overtime"), R("Geofences", "geofences"),
			P("Attendance reports", "attendance_reports", "attendance"), P("Policies", "set:hr", "attendance")),
		G("Payroll", R("Payroll periods", "payroll_periods"), R("Payroll runs", "payroll_runs"), R("Payslips", "payslips"), R("Settlements", "payroll_settlements"), R("Gratuities", "gratuities"),
			P("Payroll analytics", "payroll_analytics", "payroll"), R("Tax policies", "tax_policies"), R("Social security", "social_policies"),
			R("Components", "salary_components"), R("Salary structures", "salary_structures")),
	}},
	{"accounting", "Accounting", "📒", []NavItem{
		P("Dashboard", "acc_dash", "accounting"),
		G("Restaurant accounting", R("Sales summaries", "sales_summaries"), R("Production batches", "production_batches"), R("Channel settlements", "channel_settlements"),
			P("Food cost", "food_cost", "accounting"), P("Menu profitability", "menu_profit", "accounting"), P("Tips & service charges", "tips_report", "accounting"), R("Summary policies", "summary_policies")),
		G("AR, AP & Treasury", R("Customer accounts", "customer_accounts"), R("Cash collections", "cash_collections"), P("Supplier statements", "supplier_statements", "accounting"),
			R("Payment proposals", "payment_proposals"), R("Expense claims", "expense_claims"), R("Petty cash", "petty_cash"), R("Cheques", "cheques")),
		G("Purchasing & inventory", R("Supplier bills", "supplier_bills"), R("Supplier payments", "supplier_payments"), R("Supplier credit notes", "credit_notes"),
			P("Inventory valuation", "inventory_valuation", "accounting"), P("Inventory postings", "inv_postings", "accounting")),
		G("Banking & cash", R("Cash transfers", "cash_transfers"), R("Bank imports", "bank_imports"), R("Bank reconciliations", "bank_recs"), R("Financial accounts", "fin_accounts"),
			P("Journal entries", "journals", "accounting")),
		G("Reporting & exports", P("Financial reports", "fin_reports", "accounting"), P("Trial balance", "trial_balance", "accounting"), P("General ledger", "ledger", "accounting"), P("Accounting exports", "acc_exports_page", "accounting")),
		G("Financial calendar", R("Fiscal years", "fiscal_years"), R("Periods", "periods")),
		G("Tax compliance", R("Tax returns", "tax_returns"), R("Withholding rules", "withholding_rules"), R("Withholding tax", "withholding_tax"), R("Electronic invoices", "e_invoices")),
		G("Planning & assets", R("Plans & forecasts", "forecasts"), R("Budgets", "budgets"), R("Allocations", "allocations"), R("Asset policies", "asset_policies"), R("Fixed assets", "fixed_assets"),
			R("Costing policies", "costing_policies"), R("Accruals & prepayments", "accruals"), R("Landed costs", "landed_costs"), R("Inventory provisions", "inv_provisions"),
			R("Foreign exchange", "fx_controls"), R("Intercompany reconciliation", "intercompany"), P("Consolidation", "consolidation", "accounting")),
		G("Accounting controls", R("Posting exceptions", "posting_exceptions"), P("Posting log", "posting_log", "accounting"), P("Unposted events", "unposted", "accounting")),
		G("Configuration", P("Chart of accounts", "accounts", "accounting"), R("Account mappings", "account_maps"), R("Dimensions", "dimensions"), R("Dimension rules", "dimension_rules"),
			R("Revenue centers", "revenue_centers"), R("Legal entities", "legal_entities"), P("Initial setup", "acc_setup", "accounting"), P("Accounting settings", "set:accounting", "accounting")),
	}},
	{"reports", "Reports", "📈", []NavItem{
		P("Sales report", "rep_sales", "reports"), P("Product report", "rep_products", "reports"), P("Category report", "rep_categories", "reports"),
		P("Payment methods", "rep_payments", "reports"), P("Tax report", "rep_tax", "reports"), P("Staff performance", "rep_staff", "reports"),
		P("Branch comparison", "rep_branches", "reports"), P("Hourly sales", "rep_hours", "reports"), P("Cancellations & refunds", "rep_voids", "reports"),
		P("Inventory report", "rep_inventory", "reports"), P("Delivery report", "rep_delivery", "reports"), P("Export history", "exports", "reports"),
	}},
	{"settings", "Settings", "⚙️", []NavItem{
		P("Services & integrations", "services", "settings"),
		G("Localization", R("Translations", "translations"), R("Currency rates", "currencies"), R("Taxes", "taxes")),
		G("Notifications", P("Settings", "set:notifications", "settings"), R("Templates", "notif_templates")),
		P("Reservation settings", "set:reservation", "settings"),
		G("Printing", R("Printers", "printers"), R("Print agents", "print_agents"), P("Settings", "set:printing", "settings")),
		G("General", P("Application", "set:application", "settings"), P("Appearance & logo", "set:appearance", "settings"), P("Currency", "set:currency", "settings"),
			P("Point of Sale", "set:pos", "settings"), P("Kitchen", "set:kitchen", "settings"), P("Table QR ordering", "set:qr", "settings"), P("PWA", "set:pwa", "settings"),
			P("Two-factor authentication", "set:two_factor", "settings"), P("Passkeys", "passkeys", "settings"), P("Password recovery", "set:recovery", "settings"),
			P("Email (SMTP)", "set:email", "settings"), P("Filesystem", "set:filesystem", "settings"), P("PDF", "set:pdf", "settings")),
	}},
}

// filterNav keeps only what this user is allowed to see.
func (a *App) filterNav(role string, items []NavItem) []NavItem {
	out := []NavItem{}
	for _, it := range items {
		switch it.Kind {
		case "group":
			kids := a.filterNav(role, it.Children)
			if len(kids) > 0 {
				it.Children = kids
				out = append(out, it)
			}
		case "res":
			if res := registry[it.Target]; res != nil && a.level(role, res.Module) >= res.Read {
				out = append(out, it)
			}
		default:
			if it.Module == "" || a.level(role, it.Module) >= max(it.Level, LvView) {
				out = append(out, it)
			}
		}
	}
	return out
}

func (a *App) handleSchema(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	rails := []Rail{}
	for _, rl := range navTree {
		items := a.filterNav(c.Role, rl.Items)
		if len(items) > 0 {
			rails = append(rails, Rail{rl.Key, rl.Title, rl.Icon, items})
		}
	}
	res := map[string]any{}
	for _, k := range resOrder {
		rs := registry[k]
		lv := a.level(c.Role, rs.Module)
		if lv < rs.Read && !lookupOpen[k] {
			continue
		}
		acts := []Action{}
		for _, ac := range rs.Actions {
			if lv >= ac.Level {
				acts = append(acts, ac)
			}
		}
		res[k] = map[string]any{
			"key": rs.Key, "title": rs.Title, "module": rs.Module, "fields": rs.Fields, "label": rs.Label,
			"statuses": rs.Statuses, "actions": acts, "scoped": rs.Scoped, "hint": rs.Hint,
			"can_read": lv >= rs.Read, "can_create": lv >= rs.Write && !rs.NoCreate, "can_edit": lv >= rs.Write && !rs.NoEdit,
			"can_delete": lv >= rs.Write && !rs.NoDelete,
		}
	}
	sets := map[string]any{}
	for _, k := range settingOrder {
		g := settingGroups[k]
		if a.level(c.Role, g.Module) >= LvView {
			sets[k] = map[string]any{"key": g.Key, "title": g.Title, "hint": g.Hint, "fields": g.Fields, "can_edit": a.level(c.Role, g.Module) >= LvManage}
		}
	}
	writeJSON(w, 200, map[string]any{"rails": rails, "resources": res, "settings": sets})
}
