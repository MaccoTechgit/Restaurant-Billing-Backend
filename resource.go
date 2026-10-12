package main

import (
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
)

// =====================================================================
//  Generic resource engine
//
//  A "resource" (departments, vouchers, suppliers, leave types ...) is
//  declared once in Go with a compact field spec. The engine then gives it:
//  list (search/filter/paging), create, edit, delete, status actions,
//  CSV export, lookups for dropdowns, permissions, branch scoping, audit log.
//  The browser builds its screens from /api/schema - no per-page code.
// =====================================================================

type Field struct {
	Key    string   `json:"key"`
	Label  string   `json:"label"`
	Type   string   `json:"type"`
	Req    bool     `json:"req,omitempty"`
	Opts   []string `json:"opts,omitempty"`
	Ref    string   `json:"ref,omitempty"`
	Def    any      `json:"def,omitempty"`
	Sub    []Field  `json:"sub,omitempty"`
	List   bool     `json:"list,omitempty"`
	Search bool     `json:"-"`
	Code   bool     `json:"code,omitempty"` // stored in the unique `ref` column
	RO     bool     `json:"ro,omitempty"`
	Help   string   `json:"help,omitempty"`
}

type Action struct {
	Key     string   `json:"key"`
	Label   string   `json:"label"`
	Level   int      `json:"level"`
	From    []string `json:"from,omitempty"`
	To      string   `json:"to,omitempty"`
	Confirm string   `json:"confirm,omitempty"`
	Style   string   `json:"style,omitempty"`  // pri ok bad
	Prompt  string   `json:"prompt,omitempty"` // ask the user for a text value (sent as params.text)
}

// Hook runs inside the write transaction. old is nil on create.
type Hook func(a *App, tx *sql.Tx, c *Claims, r *Rec, old *Rec) error

// ActionFn runs inside the write transaction; it may change r (saved afterwards).
type ActionFn func(a *App, tx *sql.Tx, c *Claims, r *Rec, p map[string]any) error

type Resource struct {
	Key       string              `json:"key"`
	Module    string              `json:"module"`
	Title     string              `json:"title"`
	Group     string              `json:"group,omitempty"`
	Fields    []Field             `json:"fields"`
	Label     string              `json:"label"` // lookup label template: "{code} - {name}"
	Statuses  []string            `json:"statuses,omitempty"`
	Actions   []Action            `json:"actions,omitempty"`
	Scoped    bool                `json:"scoped"`
	NoCreate  bool                `json:"no_create,omitempty"`
	NoEdit    bool                `json:"no_edit,omitempty"`
	NoDelete  bool                `json:"no_delete,omitempty"`
	Read      int                 `json:"-"`
	Write     int                 `json:"-"`
	Asc       bool                `json:"-"`
	Before    Hook                `json:"-"`
	After     Hook                `json:"-"`
	BeforeDel Hook                `json:"-"`
	OnAction  map[string]ActionFn `json:"-"`
	Hint      string              `json:"hint,omitempty"`

	byKey map[string]*Field
}

var (
	registry = map[string]*Resource{}
	resOrder []string
)

// def registers a resource. spec is a ';'-separated field list, see parseSpec.
func def(key, module, title, spec string, opts ...func(*Resource)) *Resource {
	r := &Resource{Key: key, Module: module, Title: title, Read: LvView, Write: LvManage}
	r.Fields = parseSpec(spec)
	r.byKey = map[string]*Field{}
	for i := range r.Fields {
		f := &r.Fields[i]
		r.byKey[f.Key] = f
		if f.Key == "status" && f.Type == "select" {
			r.Statuses = f.Opts
		}
	}
	for _, o := range opts {
		o(r)
	}
	if r.Label == "" {
		if _, ok := r.byKey["name"]; ok {
			r.Label = "{name}"
		} else if len(r.Fields) > 0 {
			r.Label = "{" + r.Fields[0].Key + "}"
		}
	}
	registry[key] = r
	resOrder = append(resOrder, key)
	return r
}

func Scoped(r *Resource)             { r.Scoped = true }
func NoDelete(r *Resource)           { r.NoDelete = true }
func NoCreate(r *Resource)           { r.NoCreate = true }
func NoEdit(r *Resource)             { r.NoEdit = true }
func Label(l string) func(*Resource) { return func(r *Resource) { r.Label = l } }
func Hint(h string) func(*Resource)  { return func(r *Resource) { r.Hint = h } }
func Group(g string) func(*Resource) { return func(r *Resource) { r.Group = g } }
func WriteLv(l int) func(*Resource)  { return func(r *Resource) { r.Write = l } }
func ReadLv(l int) func(*Resource)   { return func(r *Resource) { r.Read = l } }
func Asc(r *Resource)                { r.Asc = true }
func Act(key, label string, lvl int, from []string, to string, style string) func(*Resource) {
	return func(r *Resource) {
		r.Actions = append(r.Actions, Action{Key: key, Label: label, Level: lvl, From: from, To: to, Style: style})
	}
}
func ActFn(key, label string, lvl int, from []string, style string, fn ActionFn) func(*Resource) {
	return func(r *Resource) {
		r.Actions = append(r.Actions, Action{Key: key, Label: label, Level: lvl, From: from, Style: style})
		if r.OnAction == nil {
			r.OnAction = map[string]ActionFn{}
		}
		r.OnAction[key] = fn
	}
}
func Confirm(actionKey, msg string) func(*Resource) {
	return func(r *Resource) {
		for i := range r.Actions {
			if r.Actions[i].Key == actionKey {
				r.Actions[i].Confirm = msg
			}
		}
	}
}
func Prompt(actionKey, msg string) func(*Resource) {
	return func(r *Resource) {
		for i := range r.Actions {
			if r.Actions[i].Key == actionKey {
				r.Actions[i].Prompt = msg
			}
		}
	}
}
func BeforeSave(h Hook) func(*Resource)   { return func(r *Resource) { r.Before = h } }
func AfterSave(h Hook) func(*Resource)    { return func(r *Resource) { r.After = h } }
func BeforeDelete(h Hook) func(*Resource) { return func(r *Resource) { r.BeforeDel = h } }

func S(v ...string) []string { return v }

// ---------- field spec parser ----------
//
//	key|Label:type(args)flags=default
//	flags: * required  ^ show in list  ~ searchable  # unique code  . read-only
//	types: text textarea number int money bool date datetime time email tel url color
//	       select(a|b) ref(res) multiref(res) lines(sub;sub) json file
func parseSpec(spec string) []Field {
	var out []Field
	for _, part := range splitDepth(spec, ';') {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		out = append(out, parseField(part))
	}
	return out
}

func splitDepth(s string, sep rune) []string {
	var parts []string
	depth, start := 0, 0
	for i, c := range s {
		switch {
		case c == '(':
			depth++
		case c == ')':
			depth--
		case c == sep && depth == 0:
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	return append(parts, s[start:])
}

func parseField(s string) Field {
	f := Field{}
	i := strings.IndexByte(s, ':')
	if i < 0 { // "name*^~" = a plain text field with flags
		j := 0
		for j < len(s) && (s[j] >= 'a' && s[j] <= 'z' || s[j] >= '0' && s[j] <= '9' || s[j] == '_') {
			j++
		}
		s = s[:j] + ":text" + s[j:]
		i = j
	}
	head, rest := s[:i], s[i+1:]
	if j := strings.IndexByte(head, '|'); j >= 0 {
		f.Key, f.Label = head[:j], head[j+1:]
	} else {
		f.Key = head
	}
	if f.Label == "" {
		f.Label = niceLabel(f.Key)
	}
	k := 0
	for k < len(rest) && (rest[k] >= 'a' && rest[k] <= 'z' || rest[k] >= '0' && rest[k] <= '9' || rest[k] == '_') {
		k++
	}
	f.Type, rest = rest[:k], rest[k:]
	var args string
	if strings.HasPrefix(rest, "(") {
		depth, e := 0, 0
		for e = 0; e < len(rest); e++ {
			if rest[e] == '(' {
				depth++
			} else if rest[e] == ')' {
				depth--
				if depth == 0 {
					break
				}
			}
		}
		args, rest = rest[1:e], rest[e+1:]
	}
	for len(rest) > 0 && strings.ContainsRune("*^~#.", rune(rest[0])) {
		switch rest[0] {
		case '*':
			f.Req = true
		case '^':
			f.List = true
		case '~':
			f.Search = true
		case '#':
			f.Code, f.Search = true, true
		case '.':
			f.RO = true
		}
		rest = rest[1:]
	}
	defv := ""
	hasDef := false
	if strings.HasPrefix(rest, "=") {
		defv, hasDef = rest[1:], true
	}
	switch f.Type {
	case "select":
		f.Opts = strings.Split(args, "|")
	case "ref", "multiref":
		f.Ref = args
	case "lines":
		f.Sub = parseSubs(args)
	}
	if hasDef {
		switch f.Type {
		case "number", "money", "int":
			n, _ := strconv.ParseFloat(defv, 64)
			f.Def = n
		case "bool":
			f.Def = defv == "true" || defv == "1"
		default:
			f.Def = defv
		}
	}
	return f
}

func parseSubs(args string) []Field {
	var out []Field
	for _, p := range splitDepth(args, ',') {
		if strings.TrimSpace(p) != "" {
			out = append(out, parseField(strings.TrimSpace(p)))
		}
	}
	return out
}

func niceLabel(k string) string {
	k = strings.TrimSuffix(k, "_id")
	k = strings.ReplaceAll(k, "_", " ")
	if k == "" {
		return k
	}
	r := []rune(k)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// ---------- record ----------

type Rec struct {
	ID        int64
	Res       string
	Branch    int64
	Status    string
	Ref       string
	Data      map[string]any
	CreatedBy int64
	CreatedAt int64
	UpdatedAt int64
}

func toStr(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		if x == math.Trunc(x) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	case int64:
		return strconv.FormatInt(x, 10)
	case int:
		return strconv.Itoa(x)
	case bool:
		return strconv.FormatBool(x)
	}
	return fmt.Sprint(v)
}

func toFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int64:
		return float64(x)
	case int:
		return float64(x)
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f
	case bool:
		if x {
			return 1
		}
	}
	return 0
}

func toInt(v any) int64 { return int64(math.Round(toFloat(v))) }

func toBool(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return x == "true" || x == "1" || x == "yes"
	case float64:
		return x != 0
	}
	return false
}

func (r *Rec) S(k string) string  { return toStr(r.Data[k]) }
func (r *Rec) N(k string) float64 { return toFloat(r.Data[k]) }
func (r *Rec) I(k string) int64   { return toInt(r.Data[k]) }
func (r *Rec) B(k string) bool    { return toBool(r.Data[k]) }
func (r *Rec) Set(k string, v any) {
	if r.Data == nil {
		r.Data = map[string]any{}
	}
	r.Data[k] = v
}
func (r *Rec) L(k string) []map[string]any {
	out := []map[string]any{}
	if arr, ok := r.Data[k].([]any); ok {
		for _, x := range arr {
			if m, ok := x.(map[string]any); ok {
				out = append(out, m)
			}
		}
	}
	return out
}
func (r *Rec) IDs(k string) []int64 {
	var out []int64
	if arr, ok := r.Data[k].([]any); ok {
		for _, x := range arr {
			out = append(out, toInt(x))
		}
	}
	return out
}

// JSON flattens meta + data for the browser.
func (r *Rec) JSON() map[string]any {
	m := make(map[string]any, len(r.Data)+7)
	for k, v := range r.Data {
		m[k] = v
	}
	m["id"], m["branch_id"], m["status"], m["ref"] = r.ID, r.Branch, r.Status, r.Ref
	m["created_by"], m["created_at"], m["updated_at"] = r.CreatedBy, r.CreatedAt, r.UpdatedAt
	return m
}

const recCols = `id,res,branch_id,status,ref,data,created_by,created_at,updated_at`

func scanRec(s interface{ Scan(...any) error }) (*Rec, error) {
	var r Rec
	var d string
	if err := s.Scan(&r.ID, &r.Res, &r.Branch, &r.Status, &r.Ref, &d, &r.CreatedBy, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return nil, err
	}
	r.Data = map[string]any{}
	json.Unmarshal([]byte(d), &r.Data)
	return &r, nil
}

func loadRec(q Querier, id int64) (*Rec, error) {
	return scanRec(q.QueryRow(`SELECT `+recCols+` FROM records WHERE id=?`, id))
}

// recs loads every record of a resource (used for small master-data caches).
func recs(q Querier, res string, branch int64) []*Rec {
	query := `SELECT ` + recCols + ` FROM records WHERE res=?`
	args := []any{res}
	if branch != 0 {
		query += ` AND (branch_id=? OR branch_id=0)`
		args = append(args, branch)
	}
	rows, err := q.Query(query+` ORDER BY id`, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []*Rec
	for rows.Next() {
		if r, err := scanRec(rows); err == nil {
			out = append(out, r)
		}
	}
	return out
}

func (r *Rec) searchText() string {
	res := registry[r.Res]
	var sb strings.Builder
	sb.WriteString(strconv.FormatInt(r.ID, 10))
	sb.WriteByte(' ')
	sb.WriteString(r.Ref)
	if res != nil {
		for _, f := range res.Fields {
			if f.Search {
				sb.WriteByte(' ')
				sb.WriteString(toStr(r.Data[f.Key]))
			}
		}
	}
	return strings.ToLower(sb.String())
}

// saveRec inserts (ID==0) or updates a record.
func saveRec(q Querier, r *Rec) error {
	now := nowUnix()
	if r.Data == nil {
		r.Data = map[string]any{}
	}
	b, _ := json.Marshal(r.Data)
	r.UpdatedAt = now
	if r.ID == 0 {
		r.CreatedAt = now
		res, err := q.Exec(`INSERT INTO records(res,branch_id,status,ref,data,search,created_by,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`,
			r.Res, r.Branch, r.Status, r.Ref, string(b), "", r.CreatedBy, now, now)
		if err != nil {
			return err
		}
		r.ID, _ = res.LastInsertId()
	}
	_, err := q.Exec(`UPDATE records SET branch_id=?,status=?,ref=?,data=?,search=?,updated_at=? WHERE id=?`,
		r.Branch, r.Status, r.Ref, string(b), r.searchText(), now, r.ID)
	return err
}

// mk creates a record directly (seed / internal use, no validation).
func mk(q Querier, res string, branch int64, data map[string]any) int64 {
	r := &Rec{Res: res, Branch: branch, Data: data}
	if s, ok := data["status"]; ok {
		r.Status = toStr(s)
		delete(data, "status")
	}
	if def := registry[res]; def != nil {
		if r.Status == "" && len(def.Statuses) > 0 {
			r.Status = def.Statuses[0]
		}
		for _, f := range def.Fields {
			if f.Code {
				r.Ref = toStr(data[f.Key])
			}
		}
	}
	if err := saveRec(q, r); err != nil {
		return 0
	}
	return r.ID
}

// ---------- validation ----------

var dateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

func cleanVal(f Field, v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	switch f.Type {
	case "text", "email", "tel", "url", "color", "file", "password", "textarea":
		s := strings.TrimSpace(toStr(v))
		max := 500
		if f.Type == "textarea" {
			max = 10000
		}
		if r := []rune(s); len(r) > max {
			s = string(r[:max])
		}
		if f.Type == "email" && s != "" && !strings.Contains(s, "@") {
			return nil, errors.New(f.Label + " must be a valid email")
		}
		return s, nil
	case "number":
		return toFloat(v), nil
	case "int", "money":
		return toInt(v), nil
	case "bool":
		return toBool(v), nil
	case "date":
		s := strings.TrimSpace(toStr(v))
		if s != "" && !dateRe.MatchString(s) {
			return nil, errors.New(f.Label + " must be a date (YYYY-MM-DD)")
		}
		return s, nil
	case "datetime":
		s := strings.TrimSpace(toStr(v))
		if s != "" {
			ok := false
			for _, l := range []string{"2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02T15:04:05", time.RFC3339} {
				if t, err := time.Parse(l, s); err == nil {
					s, ok = t.Format("2006-01-02T15:04"), true
					break
				}
			}
			if !ok {
				return nil, errors.New(f.Label + " must be a date and time")
			}
		}
		return s, nil
	case "time":
		s := strings.TrimSpace(toStr(v))
		if s != "" {
			if _, err := time.Parse("15:04", s); err != nil {
				return nil, errors.New(f.Label + " must be a time (HH:MM)")
			}
		}
		return s, nil
	case "select":
		s := strings.TrimSpace(toStr(v))
		if s == "" {
			return "", nil
		}
		for _, o := range f.Opts {
			if o == s {
				return s, nil
			}
		}
		return nil, errors.New(f.Label + " has an invalid choice")
	case "ref":
		return toInt(v), nil
	case "multiref":
		var ids []any
		if arr, ok := v.([]any); ok {
			for _, x := range arr {
				if n := toInt(x); n > 0 {
					ids = append(ids, float64(n))
				}
			}
		}
		return ids, nil
	case "lines":
		arr, ok := v.([]any)
		if !ok {
			return []any{}, nil
		}
		if len(arr) > 500 {
			return nil, errors.New(f.Label + " has too many lines")
		}
		out := make([]any, 0, len(arr))
		for _, x := range arr {
			m, ok := x.(map[string]any)
			if !ok {
				continue
			}
			row, err := cleanFields(f.Sub, m, nil)
			if err != nil {
				return nil, err
			}
			out = append(out, row)
		}
		return out, nil
	}
	return v, nil // json
}

func isEmpty(f Field, v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case float64:
		return f.Type == "ref" && x == 0
	case int64:
		return f.Type == "ref" && x == 0
	}
	return false
}

// cleanFields validates body against fields; absent keys keep old values.
func cleanFields(fields []Field, body, old map[string]any) (map[string]any, error) {
	out := map[string]any{}
	for _, f := range fields {
		v, present := body[f.Key]
		if f.RO { // computed by the system - never accepted from a browser
			present = false
		}
		if f.Type == "password" && present && toStr(v) == maskedPw { // untouched secret
			present = false
		}
		var val any
		switch {
		case present:
			var err error
			if val, err = cleanVal(f, v); err != nil {
				return nil, err
			}
		case old != nil:
			if ov, ok := old[f.Key]; ok {
				val = ov
			} else {
				val = f.Def
			}
		default:
			val = f.Def
		}
		if f.Req && isEmpty(f, val) {
			return nil, errors.New(f.Label + " is required")
		}
		if val != nil {
			out[f.Key] = val
		}
	}
	return out, nil
}

func (a *App) buildRec(res *Resource, body map[string]any, old *Rec) (*Rec, error) {
	var oldData map[string]any
	r := &Rec{Res: res.Key}
	if old != nil {
		*r = *old
		oldData = map[string]any{}
		for k, v := range old.Data {
			oldData[k] = v
		}
		oldData["status"] = old.Status
	}
	data, err := cleanFields(res.Fields, body, oldData)
	if err != nil {
		return nil, err
	}
	if st, ok := data["status"]; ok {
		r.Status = toStr(st)
		delete(data, "status")
	}
	if r.Status == "" && len(res.Statuses) > 0 {
		r.Status = res.Statuses[0]
	}
	for _, f := range res.Fields {
		if f.Code {
			r.Ref = strings.TrimSpace(toStr(data[f.Key]))
		}
	}
	r.Data = data
	// Records that move through a workflow (draft -> approved -> paid ...) can only
	// change status through their action buttons, never by editing / posting a status.
	if len(res.Actions) > 0 && len(res.Statuses) > 0 {
		if old != nil {
			r.Status = old.Status
		} else {
			r.Status = res.Statuses[0]
		}
	}
	return r, nil
}

// publicJSON is Rec.JSON with secret fields hidden.
func publicJSON(res *Resource, rec *Rec) map[string]any {
	m := rec.JSON()
	for _, f := range res.Fields {
		if f.Type == "password" && toStr(m[f.Key]) != "" {
			m[f.Key] = maskedPw
		}
	}
	return m
}

// branch-bound users may only change shared (all-branch) data in these areas
var branchWritable = map[string]bool{"customers": true, "gift_cards": true, "gift_txns": true, "exports": true}

func writeBlocked(c *Claims, res *Resource) bool {
	return c.Branch != 0 && !res.Scoped && !branchWritable[res.Key]
}

// ---------- helpers ----------

func qInt(r *http.Request, k string) int64 {
	n, _ := strconv.ParseInt(r.URL.Query().Get(k), 10, 64)
	return n
}
func qStr(r *http.Request, k string) string { return strings.TrimSpace(r.URL.Query().Get(k)) }

var tplRe = regexp.MustCompile(`\{(\w+)\}`)

func labelOf(res *Resource, r *Rec) string {
	s := tplRe.ReplaceAllStringFunc(res.Label, func(m string) string {
		k := m[1 : len(m)-1]
		if k == "id" {
			return strconv.FormatInt(r.ID, 10)
		}
		if k == "ref" {
			return r.Ref
		}
		return r.S(k)
	})
	if strings.TrimSpace(s) == "" {
		return "#" + strconv.FormatInt(r.ID, 10)
	}
	return s
}

// resource version counters invalidate lookup caches when data changes.
var resVer sync.Map

func bumpRes(key string) {
	v, _ := resVer.LoadOrStore(key, new(atomic.Int64))
	v.(*atomic.Int64).Add(1)
}
func verOf(key string) int64 {
	v, _ := resVer.LoadOrStore(key, new(atomic.Int64))
	return v.(*atomic.Int64).Load()
}

func (a *App) resFor(w http.ResponseWriter, r *http.Request, lvl int) (*Resource, *Claims) {
	res := registry[r.PathValue("res")]
	c := userFrom(r)
	if res == nil {
		writeErr(w, 404, "unknown resource")
		return nil, nil
	}
	if a.level(c.Role, res.Module) < lvl {
		writeErr(w, 403, "you don't have permission for this")
		return nil, nil
	}
	return res, c
}

// canTouch: scoped users may only touch their own branch (or global rows).
func canTouch(c *Claims, res *Resource, r *Rec) bool {
	return !res.Scoped || c.Branch == 0 || r.Branch == 0 || r.Branch == c.Branch
}

// ---------- handlers ----------

func (a *App) rList(w http.ResponseWriter, r *http.Request) {
	res, c := a.resFor(w, r, 0)
	if res == nil {
		return
	}
	if a.level(c.Role, res.Module) < res.Read {
		writeErr(w, 403, "you don't have permission for this")
		return
	}
	where, args := a.listFilter(res, c, r)
	limit := int(qInt(r, "limit"))
	csvOut := qStr(r, "format") == "csv"
	if limit < 1 || limit > 5000 {
		limit = 25
	}
	if csvOut {
		limit = 50000
	}
	page := int(qInt(r, "page"))
	if page < 1 {
		page = 1
	}
	order := "DESC"
	if res.Asc || qStr(r, "sort") == "asc" {
		order = "ASC"
	}
	var total int64
	if !csvOut {
		a.db.rdb.QueryRow(`SELECT COUNT(*) FROM records WHERE `+where, args...).Scan(&total)
	}
	rows, err := a.db.rdb.Query(`SELECT `+recCols+` FROM records WHERE `+where+` ORDER BY id `+order+` LIMIT ? OFFSET ?`,
		append(args, limit, (page-1)*limit)...)
	if err != nil {
		writeErr(w, 500, "database error")
		return
	}
	defer rows.Close()
	list := []*Rec{}
	for rows.Next() {
		if rec, err := scanRec(rows); err == nil {
			list = append(list, rec)
		}
	}
	if csvOut {
		a.exportCSV(w, res, c, list)
		return
	}
	out := make([]map[string]any, len(list))
	for i, rec := range list {
		out[i] = publicJSON(res, rec)
	}
	writeJSON(w, 200, map[string]any{"rows": out, "total": total, "page": page, "limit": limit})
}

var keyRe = regexp.MustCompile(`^[a-z0-9_]+$`)

func (a *App) listFilter(res *Resource, c *Claims, r *http.Request) (string, []any) {
	where := []string{"res=?"}
	args := []any{res.Key}
	if res.Scoped {
		if b := scopeBranch(c, r); b != 0 {
			where = append(where, "(branch_id=? OR branch_id=0)")
			args = append(args, b)
		}
	}
	q := r.URL.Query()
	if s := q.Get("status"); s != "" {
		parts := strings.Split(s, ",")
		ph := make([]string, len(parts))
		for i, p := range parts {
			ph[i] = "?"
			args = append(args, p)
		}
		where = append(where, "status IN ("+strings.Join(ph, ",")+")")
	}
	if s := strings.ToLower(strings.TrimSpace(q.Get("q"))); s != "" {
		where = append(where, "search LIKE ?")
		args = append(args, "%"+s+"%")
	}
	if n, _ := strconv.ParseInt(q.Get("from"), 10, 64); n > 0 {
		where = append(where, "created_at>=?")
		args = append(args, n)
	}
	if n, _ := strconv.ParseInt(q.Get("to"), 10, 64); n > 0 {
		where = append(where, "created_at<?")
		args = append(args, n)
	}
	if s := q.Get("ids"); s != "" {
		var ids []any
		var ph []string
		for _, p := range strings.Split(s, ",") {
			if n, err := strconv.ParseInt(p, 10, 64); err == nil {
				ids = append(ids, n)
				ph = append(ph, "?")
			}
		}
		if len(ids) > 0 {
			where = append(where, "id IN ("+strings.Join(ph, ",")+")")
			args = append(args, ids...)
		}
	}
	for k, v := range q { // f.<field>=value filters on data fields
		if strings.HasPrefix(k, "f.") && len(v) > 0 && v[0] != "" {
			fk := k[2:]
			if keyRe.MatchString(fk) && res.byKey[fk] != nil {
				where = append(where, "json_extract(data,'$."+fk+"')=?")
				if res.byKey[fk].Type == "bool" {
					args = append(args, v[0] == "true" || v[0] == "1")
				} else if t := res.byKey[fk].Type; t == "ref" || t == "number" || t == "int" || t == "money" {
					n, _ := strconv.ParseFloat(v[0], 64)
					args = append(args, n)
				} else {
					args = append(args, v[0])
				}
			}
		}
	}
	return strings.Join(where, " AND "), args
}

func (a *App) rGet(w http.ResponseWriter, r *http.Request) {
	res, c := a.resFor(w, r, 0)
	if res == nil {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	rec, err := loadRec(a.db.rdb, id)
	if err != nil || rec.Res != res.Key || !canTouch(c, res, rec) || a.level(c.Role, res.Module) < res.Read {
		writeErr(w, 404, "not found")
		return
	}
	writeJSON(w, 200, publicJSON(res, rec))
}

func (a *App) rCreate(w http.ResponseWriter, r *http.Request) {
	res, c := a.resFor(w, r, 0)
	if res == nil {
		return
	}
	if res.NoCreate || a.level(c.Role, res.Module) < res.Write || writeBlocked(c, res) {
		writeErr(w, 403, "you don't have permission for this")
		return
	}
	var body map[string]any
	if !readJSON(w, r, &body) {
		return
	}
	rec, err := a.buildRec(res, body, nil)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	rec.CreatedBy = c.UID
	if res.Scoped {
		rec.Branch = c.Branch
		if c.Branch == 0 {
			rec.Branch = toInt(body["branch_id"])
		}
	}
	err = a.tx(func(tx *sql.Tx) error {
		if res.Before != nil {
			if err := res.Before(a, tx, c, rec, nil); err != nil {
				return err
			}
		}
		if err := saveRec(tx, rec); err != nil {
			return err
		}
		if res.After != nil {
			return res.After(a, tx, c, rec, nil)
		}
		return nil
	})
	if err != nil {
		writeErr(w, 400, friendlyErr(err))
		return
	}
	bumpRes(res.Key)
	a.hub.Publish("res:" + res.Key)
	a.audit(r, res.Key, rec.ID, "create", labelOf(res, rec))
	writeJSON(w, 201, publicJSON(res, rec))
}

func friendlyErr(err error) string {
	if strings.Contains(err.Error(), "UNIQUE") {
		return "this code already exists, please use a different one"
	}
	if strings.Contains(err.Error(), "database is locked") {
		return "server is busy, please try again"
	}
	var ue *UserErr
	if errors.As(err, &ue) {
		return ue.Msg
	}
	return err.Error()
}

// UserErr is an error whose message is safe to show to users.
type UserErr struct{ Msg string }

func (e *UserErr) Error() string    { return e.Msg }
func uerr(f string, a ...any) error { return &UserErr{fmt.Sprintf(f, a...)} }

func (a *App) rUpdate(w http.ResponseWriter, r *http.Request) {
	res, c := a.resFor(w, r, 0)
	if res == nil {
		return
	}
	if res.NoEdit || a.level(c.Role, res.Module) < res.Write || writeBlocked(c, res) {
		writeErr(w, 403, "you don't have permission for this")
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var body map[string]any
	if !readJSON(w, r, &body) {
		return
	}
	var out *Rec
	err := a.tx(func(tx *sql.Tx) error {
		old, err := loadRec(tx, id)
		if err != nil || old.Res != res.Key || !canTouch(c, res, old) {
			return uerr("record not found")
		}
		rec, err := a.buildRec(res, body, old)
		if err != nil {
			return err
		}
		if res.Scoped && c.Branch == 0 {
			if _, ok := body["branch_id"]; ok {
				rec.Branch = toInt(body["branch_id"])
			}
		}
		if res.Before != nil {
			if err := res.Before(a, tx, c, rec, old); err != nil {
				return err
			}
		}
		if err := saveRec(tx, rec); err != nil {
			return err
		}
		if res.After != nil {
			if err := res.After(a, tx, c, rec, old); err != nil {
				return err
			}
		}
		out = rec
		return nil
	})
	if err != nil {
		writeErr(w, 400, friendlyErr(err))
		return
	}
	bumpRes(res.Key)
	a.hub.Publish("res:" + res.Key)
	a.audit(r, res.Key, id, "update", labelOf(res, out))
	writeJSON(w, 200, publicJSON(res, out))
}

func (a *App) rDelete(w http.ResponseWriter, r *http.Request) {
	res, c := a.resFor(w, r, 0)
	if res == nil {
		return
	}
	if res.NoDelete || a.level(c.Role, res.Module) < res.Write || writeBlocked(c, res) {
		writeErr(w, 403, "you don't have permission for this")
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var label string
	err := a.tx(func(tx *sql.Tx) error {
		old, err := loadRec(tx, id)
		if err != nil || old.Res != res.Key || !canTouch(c, res, old) {
			return uerr("record not found")
		}
		if res.BeforeDel != nil {
			if err := res.BeforeDel(a, tx, c, old, nil); err != nil {
				return err
			}
		}
		label = labelOf(res, old)
		_, err = tx.Exec(`DELETE FROM records WHERE id=?`, id)
		return err
	})
	if err != nil {
		writeErr(w, 400, friendlyErr(err))
		return
	}
	bumpRes(res.Key)
	a.hub.Publish("res:" + res.Key)
	a.audit(r, res.Key, id, "delete", label)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *App) rAction(w http.ResponseWriter, r *http.Request) {
	res, c := a.resFor(w, r, 0)
	if res == nil {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var act *Action
	for i := range res.Actions {
		if res.Actions[i].Key == r.PathValue("action") {
			act = &res.Actions[i]
		}
	}
	if act == nil {
		writeErr(w, 404, "unknown action")
		return
	}
	if a.level(c.Role, res.Module) < act.Level || writeBlocked(c, res) {
		writeErr(w, 403, "you don't have permission for this step")
		return
	}
	var params map[string]any
	if r.ContentLength != 0 {
		readJSON(w, r, &params)
	}
	var out *Rec
	err := a.tx(func(tx *sql.Tx) error {
		rec, err := loadRec(tx, id)
		if err != nil || rec.Res != res.Key || !canTouch(c, res, rec) {
			return uerr("record not found")
		}
		if len(act.From) > 0 {
			ok := false
			for _, s := range act.From {
				if s == rec.Status {
					ok = true
				}
			}
			if !ok {
				return uerr("this record is %s, so you cannot %s it", rec.Status, strings.ToLower(act.Label))
			}
		}
		if fn := res.OnAction[act.Key]; fn != nil {
			if err := fn(a, tx, c, rec, params); err != nil {
				return err
			}
		}
		if act.To != "" {
			rec.Status = act.To
		}
		if err := saveRec(tx, rec); err != nil {
			return err
		}
		out = rec
		return nil
	})
	if err != nil {
		writeErr(w, 400, friendlyErr(err))
		return
	}
	bumpRes(res.Key)
	a.hub.Publish("res:" + res.Key)
	a.audit(r, res.Key, id, act.Key, labelOf(res, out))
	writeJSON(w, 200, publicJSON(res, out))
}

// ---------- lookups (dropdown sources) ----------

func (a *App) rLookup(w http.ResponseWriter, r *http.Request) {
	res := registry[r.PathValue("res")]
	c := userFrom(r)
	if res == nil {
		writeErr(w, 404, "unknown resource")
		return
	}
	// dropdowns are readable by anyone who can see *something* in the module or is signed in to POS-like modules
	if a.level(c.Role, res.Module) < LvView && !lookupOpen[res.Key] {
		writeErr(w, 403, "you don't have permission for this")
		return
	}
	b := scopeBranch(c, r)
	if res.Key == "users" { // typed table: names only, never hashes
		w.Header().Set("Content-Type", "application/json")
		where, args := "active=1", []any{}
		if b != 0 {
			where += " AND (branch_id=? OR branch_id=0)"
			args = append(args, b)
		}
		rows := a.queryMaps(`SELECT id,name,username,role FROM users WHERE `+where+` ORDER BY name`, args...)
		out := make([]map[string]any, 0, len(rows))
		for _, u := range rows {
			out = append(out, map[string]any{"id": u["id"], "label": toStr(u["name"]) + " (" + toStr(u["username"]) + ")", "status": "active", "d": map[string]any{"role": u["role"]}})
		}
		w.Write(jsonBytes(out))
		return
	}
	if res.Key == "accounts" { // typed table, served from the chart cache
		w.Header().Set("Content-Type", "application/json")
		out := []map[string]any{}
		if ch := a.chart.Load(); ch != nil {
			ch.mu.RLock()
			for _, ac := range ch.byID {
				out = append(out, map[string]any{"id": ac.ID, "label": ac.Code + " · " + ac.Name, "status": "active", "d": map[string]any{"code": ac.Code, "type": ac.Type}})
			}
			ch.mu.RUnlock()
		}
		sort.Slice(out, func(i, j int) bool { return toStr(out[i]["label"]) < toStr(out[j]["label"]) })
		w.Write(jsonBytes(out))
		return
	}
	full := qStr(r, "full") == "1"
	status := qStr(r, "status")
	key := fmt.Sprintf("%s|%d|%s|%v|%d", res.Key, b, status, full, verOf(res.Key))
	data, err := a.lookups.get(key, 3*time.Second, func() ([]byte, error) {
		where := []string{"res=?"}
		args := []any{res.Key}
		if res.Scoped && b != 0 {
			where = append(where, "(branch_id=? OR branch_id=0)")
			args = append(args, b)
		}
		if status != "" {
			where = append(where, "status=?")
			args = append(args, status)
		}
		rows, err := a.db.rdb.Query(`SELECT `+recCols+` FROM records WHERE `+strings.Join(where, " AND ")+` ORDER BY id LIMIT 3000`, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := []map[string]any{}
		for rows.Next() {
			rec, err := scanRec(rows)
			if err != nil {
				continue
			}
			m := map[string]any{"id": rec.ID, "label": labelOf(res, rec), "status": rec.Status}
			if full {
				d := make(map[string]any, len(rec.Data))
				for k, v := range rec.Data {
					d[k] = v
				}
				for _, f := range res.Fields {
					if f.Type == "password" {
						delete(d, f.Key)
					}
				}
				m["d"] = d
			}
			out = append(out, m)
		}
		return json.Marshal(out)
	})
	if err != nil {
		writeErr(w, 503, "please retry")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

// resources whose dropdowns every signed-in user may load (needed by POS screens)
var lookupOpen = map[string]bool{"branches": true, "zones": true, "floors": true, "tables": true, "categories": true,
	"products": true, "customers": true, "registers": true, "reasons": true, "stations": true, "units": true, "taxes": true,
	"departments": true, "designations": true, "employees": true, "shifts": true, "leave_types": true, "drivers": true,
	"suppliers": true, "ingredients": true, "options": true, "menus": true, "currencies": true, "delivery_zones": true, "discounts": true, "vouchers": true, "couriers": true, "accounts": true, "dimensions": true, "users": true}

// ---------- CSV export ----------

func (a *App) exportCSV(w http.ResponseWriter, res *Resource, c *Claims, list []*Rec) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+res.Key+`.csv"`)
	w.Write([]byte("\xef\xbb\xbf"))
	cw := csv.NewWriter(w)
	head := []string{"ID"}
	var fs []Field
	for _, f := range res.Fields {
		if f.Type == "lines" || f.Type == "json" || f.Type == "password" {
			continue
		}
		fs = append(fs, f)
		head = append(head, f.Label)
	}
	head = append(head, "Created")
	cw.Write(head)
	labels := map[string]map[int64]string{}
	for _, f := range fs {
		if f.Type == "ref" && labels[f.Ref] == nil {
			labels[f.Ref] = a.labelMap(f.Ref)
		}
	}
	for _, rec := range list {
		row := []string{strconv.FormatInt(rec.ID, 10)}
		for _, f := range fs {
			var v any
			if f.Key == "status" {
				v = rec.Status
			} else {
				v = rec.Data[f.Key]
			}
			switch f.Type {
			case "money":
				row = append(row, fmt.Sprintf("%.2f", toFloat(v)/100))
			case "ref":
				row = append(row, labels[f.Ref][toInt(v)])
			case "multiref":
				var parts []string
				for _, id := range rec.IDs(f.Key) {
					parts = append(parts, strconv.FormatInt(id, 10))
				}
				row = append(row, strings.Join(parts, " "))
			default:
				row = append(row, csvSafe(toStr(v)))
			}
		}
		row = append(row, time.Unix(rec.CreatedAt, 0).Format("2006-01-02 15:04"))
		cw.Write(row)
	}
	cw.Flush()
	a.bgExec(`INSERT INTO audit_log(ts,user_id,user,role,res,action,detail) VALUES(?,?,?,?,?,?,?)`,
		nowUnix(), c.UID, c.User, c.Role, res.Key, "export", fmt.Sprintf("%d rows", len(list)))
	go a.recordExport(c, res.Key, len(list))
}

// csvSafe stops spreadsheet formula injection (=, +, -, @ at the start of a cell).
func csvSafe(s string) string {
	if s != "" && strings.ContainsRune("=+-@", rune(s[0])) {
		return "'" + s
	}
	return s
}

func (a *App) labelMap(resKey string) map[int64]string {
	m := map[int64]string{}
	res := registry[resKey]
	if res == nil {
		return m
	}
	if resKey == "users" {
		for _, u := range a.queryMaps(`SELECT id,name,username FROM users WHERE active=1`) {
			m[toInt(u["id"])] = toStr(u["name"]) + " (" + toStr(u["username"]) + ")"
		}
		return m
	}
	if resKey == "accounts" {
		if ch := a.chart.Load(); ch != nil {
			ch.mu.RLock()
			for id, ac := range ch.byID {
				m[id] = ac.Code + " · " + ac.Name
			}
			ch.mu.RUnlock()
		}
		return m
	}
	for _, rec := range recs(a.db.rdb, resKey, 0) {
		m[rec.ID] = labelOf(res, rec)
	}
	return m
}
