package main

import (
	"net/http"
	"strconv"
	"strings"
)

func (a *App) handleUsers(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	where, args := "1=1", []any{}
	if b := scopeBranch(c, r); b != 0 {
		where += " AND (branch_id=? OR branch_id=0)"
		args = append(args, b)
	}
	if q := strings.ToLower(qStr(r, "q")); q != "" {
		where += " AND (lower(name) LIKE ? OR lower(username) LIKE ? OR lower(email) LIKE ?)"
		args = append(args, "%"+q+"%", "%"+q+"%", "%"+q+"%")
	}
	rows := a.queryMaps(`SELECT id,username,name,email,phone,role,branch_id,active,totp_on,last_login,created_at FROM users WHERE `+where+` ORDER BY id`, args...)
	writeJSON(w, 200, rows)
}

type userIn struct {
	Username string `json:"username"`
	Name     string `json:"name"`
	Email    string `json:"email"`
	Phone    string `json:"phone"`
	Role     string `json:"role"`
	BranchID int64  `json:"branch_id"`
	Password string `json:"password"`
	Active   *bool  `json:"active"`
}

// only a super admin may hand out admin-level roles
func (a *App) canGrant(actor, role string) bool {
	if a.role(role) == nil {
		return false
	}
	if role == RoleSuperAdmin || role == RoleAdmin {
		return actor == RoleSuperAdmin
	}
	return true
}

func (a *App) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	var in userIn
	if !readJSON(w, r, &in) {
		return
	}
	in.Username, in.Name = strings.TrimSpace(in.Username), strings.TrimSpace(in.Name)
	if in.Username == "" || in.Name == "" || len(in.Password) < 8 {
		writeErr(w, 400, "name, username and a password of 8+ characters are required")
		return
	}
	if !a.canGrant(c.Role, in.Role) {
		writeErr(w, 403, "you cannot create this role")
		return
	}
	if c.Branch != 0 {
		in.BranchID = c.Branch
	}
	if in.Role == RoleAdminBranch && in.BranchID == 0 {
		writeErr(w, 400, "a branch admin must belong to a branch")
		return
	}
	res, err := a.db.wdb.Exec(`INSERT INTO users(username,name,email,phone,role,branch_id,pass_hash,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		in.Username, in.Name, strings.TrimSpace(in.Email), strings.TrimSpace(in.Phone), in.Role, in.BranchID, hashPassword(in.Password), nowUnix())
	if err != nil {
		writeErr(w, 409, "this username is already taken")
		return
	}
	id, _ := res.LastInsertId()
	a.audit(r, "users", id, "create", in.Username+" ("+in.Role+")")
	writeJSON(w, 201, map[string]any{"id": id})
}

func (a *App) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	var in userIn
	if !readJSON(w, r, &in) {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var curRole string
	var curBranch int64
	if a.db.rdb.QueryRow(`SELECT role,branch_id FROM users WHERE id=?`, id).Scan(&curRole, &curBranch) != nil {
		writeErr(w, 404, "user not found")
		return
	}
	if c.Branch != 0 && curBranch != c.Branch {
		writeErr(w, 403, "this user belongs to another branch")
		return
	}
	if (curRole == RoleSuperAdmin || curRole == RoleAdmin) && c.Role != RoleSuperAdmin {
		writeErr(w, 403, "only a super admin can change this user")
		return
	}
	if in.Role != "" && !a.canGrant(c.Role, in.Role) {
		writeErr(w, 403, "you cannot assign this role")
		return
	}
	if id == c.UID && ((in.Active != nil && !*in.Active) || (in.Role != "" && in.Role != curRole)) {
		writeErr(w, 400, "you cannot disable or demote yourself")
		return
	}
	sets, args := []string{}, []any{}
	add := func(col string, v any) { sets = append(sets, col+"=?"); args = append(args, v) }
	if in.Name != "" {
		add("name", strings.TrimSpace(in.Name))
	}
	add("email", strings.TrimSpace(in.Email))
	add("phone", strings.TrimSpace(in.Phone))
	revoke := false
	if in.Role != "" && in.Role != curRole {
		add("role", in.Role)
		revoke = true
	}
	if c.Branch == 0 && in.BranchID != curBranch {
		add("branch_id", in.BranchID)
		revoke = true
	}
	if in.Active != nil {
		add("active", *in.Active)
		if !*in.Active {
			revoke = true
		}
	}
	if in.Password != "" {
		if len(in.Password) < 8 {
			writeErr(w, 400, "password must be 8+ characters")
			return
		}
		add("pass_hash", hashPassword(in.Password))
		revoke = true
	}
	args = append(args, id)
	a.db.wdb.Exec(`UPDATE users SET `+strings.Join(sets, ",")+` WHERE id=?`, args...)
	if revoke && id != c.UID {
		a.revoke(id) // force re-login so new role / branch / disable applies at once
	}
	a.audit(r, "users", id, "update", in.Username+in.Name)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *App) handleUserResetLink(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	u, err := a.findUser(`id=?`, id)
	if err != nil {
		writeErr(w, 404, "user not found")
		return
	}
	link := a.makeResetLink(u.ID, a.baseURL(r))
	a.audit(r, "users", id, "reset-link", u.Username)
	writeJSON(w, 200, map[string]string{"link": link})
}
