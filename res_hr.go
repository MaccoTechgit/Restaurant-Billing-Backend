package main

import (
	"database/sql"
	"fmt"
)

func init() {
	// ---------------- people & setup ----------------
	def("departments", "hr", "Departments", `name*^~; head:ref(employees); description; status:select(active|inactive)^=active`)
	def("designations", "hr", "Designations", `name*^~; department:ref(departments)^; level:int=1; status:select(active|inactive)^=active`)
	def("shifts", "hr", "Shifts", `name*^~; start:time*^=09:00; end:time*^=17:00; break_min|Break (min):int=60; grace_min|Grace (min):int=10; night|Night shift:bool; color:color=#f97316; status:select(active|inactive)^=active`, Scoped,
		Hint("Working hours. Late arrival is measured against the start time + grace."))
	def("doc_types", "hr", "Document types", `name*^~; required:bool^; has_expiry|Has expiry date:bool^; status:select(active|inactive)^=active`)
	def("holidays", "hr", "Holidays", `name*^~; date:date*^; kind:select(public|company|optional)=public^; note`, Scoped, Asc,
		Hint("Days off for everyone. They are skipped in leave counting and attendance."))
	def("employees", "hr", "Employees",
		`code|Employee code:text#*^; name*^~; designation:ref(designations)^; department:ref(departments)^; shift:ref(shifts); manager:ref(employees); join_date:date*^; employment_type:select(full_time|part_time|contract|intern)=full_time;
		email:email~; phone:tel^~; gender:select(male|female|other); dob|Date of birth:date; national_id|National / ID number:text; address:textarea; emergency_contact:text;
		salary_structure:ref(salary_structures); basic_salary|Basic salary (override):money; bank_name; bank_account; user|Linked login:ref(users); leave_date|Last working day:date; status:select(active|on_leave|inactive|terminated)^=active`,
		Scoped, Hint("Everyone who works for you. Link a login so they can check in themselves."),
		BeforeSave(func(a *App, tx *sql.Tx, c *Claims, r *Rec, old *Rec) error {
			if r.S("manager") != "" && r.I("manager") == r.ID && r.ID != 0 {
				return uerr("an employee cannot be their own manager")
			}
			return nil
		}))
	def("employee_docs", "hr", "Employee documents", `employee:ref(employees)*^; type:ref(doc_types)*^; number:text~^; issued:date; expires:date^; file:file; note; status:select(valid|expired)^=valid`, Scoped, Label("Doc #{id}"))

	// ---------------- leave ----------------
	def("leave_types", "hr", "Leave types", `name*^~; code#^; paid:bool=true^; days_per_year:number*=14^; carry_forward:bool; max_carry:number=0; requires_doc|Needs a document:bool; gender:select(all|male|female)=all; status:select(active|inactive)^=active`)
	def("leave_policies", "hr", "Leave policies", `name*^~; leave_type:ref(leave_types)*^; entitlement|Days per year:number*^; accrual:select(yearly|monthly)=yearly; min_service_months|Min service (months):int=0; department:ref(departments); status:select(active|inactive)^=active`,
		Hint("Override a leave type's yearly days for a department or after a minimum service period."))
	def("leave_rules", "hr", "Leave approval rules", `name*^~; leave_type:ref(leave_types); min_days:number=0; max_days:number=0; approver:select(manager|hr_officer|hr_manager|admin)=hr_manager^; auto_approve_below|Auto-approve up to (days):number=0; status:select(active|inactive)^=active`,
		Hint("Who approves which requests. Requests at or below the auto-approve limit are approved instantly."))
	def("leave_requests", "attendance", "Leave requests",
		`employee:ref(employees)*^; leave_type:ref(leave_types)*^; from:date*^; to:date*^; half_day:bool; days:number.^; reason:textarea; document:file; status:select(pending|approved|rejected|cancelled)^=pending; review_note:text.; reviewed_by:text.`,
		Scoped, Label("Leave #{id}"), Hint("Staff leave. Approving deducts the balance and marks attendance as leave."),
		BeforeSave(leaveBefore),
		AfterSave(leaveAfter),
		ActFn("approve", "Approve", LvApprove, S("pending"), "ok", leaveApprove),
		ActFn("reject", "Reject", LvApprove, S("pending"), "bad", func(a *App, tx *sql.Tx, c *Claims, r *Rec, p map[string]any) error {
			r.Status = "rejected"
			r.Set("review_note", trunc(toStr(p["text"]), 200))
			r.Set("reviewed_by", c.Name)
			return nil
		}), Prompt("reject", "Reason for rejecting (optional)"),
		ActFn("cancel", "Cancel", LvManage, S("pending", "approved"), "bad", leaveCancel))
	def("overtime", "attendance", "Overtime requests", `employee:ref(employees)*^; date:date*^; hours:number*^; reason:text; status:select(pending|approved|rejected)^=pending; reviewed_by:text.`, Scoped, Label("OT #{id}"),
		Hint("Extra hours worked. Only approved overtime is paid in payroll."),
		ActFn("approve", "Approve", LvApprove, S("pending"), "ok", func(a *App, tx *sql.Tx, c *Claims, r *Rec, p map[string]any) error {
			r.Status = "approved"
			r.Set("reviewed_by", c.Name)
			return nil
		}),
		ActFn("reject", "Reject", LvApprove, S("pending"), "bad", func(a *App, tx *sql.Tx, c *Claims, r *Rec, p map[string]any) error {
			r.Status = "rejected"
			r.Set("reviewed_by", c.Name)
			return nil
		}))
	def("geofences", "attendance", "Geofences", `name*^~; lat:number*; lng:number*; radius_m|Radius (metres):int*=150^; status:select(active|inactive)^=active`, Scoped,
		Hint("Staff can only check in when their phone is inside one of these circles (when location is required in the policy)."))

	// ---------------- payroll ----------------
	def("salary_components", "payroll", "Salary components",
		`name*^~; code#^; kind:select(earning|deduction|employer_contribution)*^; calc:select(fixed|percent_of_basic|percent_of_gross)=fixed; amount:money=0^; percent:number=0^; taxable:bool=true; status:select(active|inactive)^=active`,
		Hint("Allowances, bonuses, deductions and employer contributions used in salary structures."))
	def("salary_structures", "payroll", "Salary structures",
		`name*^~; basic:money*^; lines:lines(component:ref(salary_components)*,amount:money=0,percent:number=0); status:select(active|inactive)^=active`,
		Hint("A reusable pay package. Lines with a value override the component's default."))
	def("tax_policies", "payroll", "Tax policies", `name*^~; standard_deduction|Monthly tax-free allowance:money=0; brackets:lines(up_to|Up to (monthly, 0 = no limit):money=0,rate|Rate %:number*); default:bool^; status:select(active|inactive)^=active`,
		Hint("Progressive income tax withheld from salary each month."))
	def("social_policies", "payroll", "Social security policies", `name*^~; employee_pct|Employee %:number=0^; employer_pct|Employer %:number=0^; ceiling|Monthly salary ceiling (0 = none):money=0; default:bool^; status:select(active|inactive)^=active`)
	def("payroll_periods", "payroll", "Payroll periods", `name*^~; from:date*^; to:date*^; pay_date:date; status:select(open|closed)^=open`, Asc)
	def("payroll_runs", "payroll", "Payroll runs",
		`name*^~; period:ref(payroll_periods)*^; tax_policy:ref(tax_policies); social_policy:ref(social_policies); employees:int.^; gross:money.^; deductions:money.^; net:money.^; employer_cost:money.; status:select(draft|calculated|approved|posted|paid)^=draft; note`,
		Scoped, Hint("Calculate pay for everyone in a period, approve it, post to accounting and mark it paid."),
		ActFn("calculate", "Calculate", LvManage, S("draft", "calculated"), "pri", calcPayroll),
		ActFn("approve", "Approve", LvApprove, S("calculated"), "ok", func(a *App, tx *sql.Tx, c *Claims, r *Rec, p map[string]any) error {
			r.Status = "approved"
			tx.Exec(`UPDATE records SET status='approved' WHERE res='payslips' AND json_extract(data,'$.run')=?`, r.ID)
			return nil
		}),
		ActFn("post", "Post to accounting", LvApprove, S("approved"), "ok", postPayroll),
		ActFn("pay", "Mark paid", LvApprove, S("posted"), "ok", payPayroll),
		Confirm("post", "Post this payroll to the ledger?"), Confirm("pay", "Record the salary payment from the bank?"))
	def("payslips", "payroll", "Payslips",
		`run:ref(payroll_runs)*^; employee:ref(employees)*^; basic:money^; earnings:money^; overtime:money^; unpaid_deduction:money; social:money; tax:money; other_deductions:money; net:money^; employer_social:money; unpaid_days:number; overtime_hours:number; lines:json; status:select(draft|approved|paid)^=draft`,
		Scoped, NoCreate, Label("Payslip #{id}"))
	def("payroll_settlements", "payroll", "Final settlements",
		`employee:ref(employees)*^; last_day:date*^; reason:select(resignation|termination|end_of_contract|retirement)=resignation; unpaid_salary:money=0; leave_encashment:money=0; gratuity:money=0; other_additions:money=0; deductions:money=0; net:money.^; note; status:select(draft|approved|paid)^=draft`,
		Scoped, Label("Settlement #{id}"), Hint("Money owed to a leaving employee: last salary, unused leave, gratuity."),
		BeforeSave(func(a *App, tx *sql.Tx, c *Claims, r *Rec, old *Rec) error {
			r.Set("net", float64(r.I("unpaid_salary")+r.I("leave_encashment")+r.I("gratuity")+r.I("other_additions")-r.I("deductions")))
			return nil
		}),
		ActFn("approve", "Approve", LvApprove, S("draft"), "ok", func(a *App, tx *sql.Tx, c *Claims, r *Rec, p map[string]any) error {
			r.Status = "approved"
			if emp, err := loadRec(tx, r.I("employee")); err == nil {
				emp.Status = "terminated"
				emp.Set("leave_date", r.S("last_day"))
				saveRec(tx, emp)
			}
			return nil
		}),
		ActFn("pay", "Mark paid", LvApprove, S("approved"), "ok", func(a *App, tx *sql.Tx, c *Claims, r *Rec, p map[string]any) error {
			r.Status = "paid"
			net := r.I("net")
			if !a.autoPost() || net <= 0 {
				return nil
			}
			_, err := a.post(tx, r.Branch, today(), fmt.Sprintf("settlement:%d", r.ID), "Final settlement", c.UID,
				Dr("payroll_expense", r.I("unpaid_salary")+r.I("leave_encashment")+r.I("other_additions")), Dr("gratuity_expense", r.I("gratuity")),
				Cr("bank", net), Cr("accrued", r.I("deductions")), Dr("accrued", 0))
			return err
		}))
	def("gratuities", "payroll", "Gratuities", `employee:ref(employees)*^; as_of:date*^; days_per_year|Days of pay per year of service:number=15; years:number.^; amount:money.^; status:select(draft|approved|paid)^=draft`,
		Scoped, Label("Gratuity #{id}"), Hint("End-of-service benefit: (monthly basic ÷ 30) × days per year × years of service."),
		BeforeSave(func(a *App, tx *sql.Tx, c *Claims, r *Rec, old *Rec) error {
			emp, err := loadRec(tx, r.I("employee"))
			if err != nil {
				return uerr("choose an employee")
			}
			years := yearsBetween(emp.S("join_date"), r.S("as_of"))
			basic := a.employeeBasic(tx, emp)
			r.Set("years", years)
			r.Set("amount", float64(int64(float64(basic)/30*r.N("days_per_year")*years)))
			return nil
		}),
		ActFn("approve", "Approve", LvApprove, S("draft"), "ok", func(a *App, tx *sql.Tx, c *Claims, r *Rec, p map[string]any) error {
			r.Status = "approved"
			return a.postSimple(tx, r.Branch, today(), fmt.Sprintf("gratuity:%d", r.ID), "Gratuity accrual", "gratuity_expense", "accrued", r.I("amount"), c.UID)
		}),
		ActFn("pay", "Mark paid", LvApprove, S("approved"), "ok", func(a *App, tx *sql.Tx, c *Claims, r *Rec, p map[string]any) error {
			r.Status = "paid"
			return a.postSimple(tx, r.Branch, today(), fmt.Sprintf("gratuity:%d", r.ID), "Gratuity paid", "accrued", "bank", r.I("amount"), c.UID)
		}))
}
