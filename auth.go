package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// ---------- Stateless token (HS256 JWT) ----------
// Every API request is verified with one HMAC - no DB lookup, so
// authenticated traffic scales with CPU only.

type Claims struct {
	UID    int64  `json:"uid"`
	User   string `json:"u"`
	Name   string `json:"n"`
	Role   string `json:"r"`
	Branch int64  `json:"b"`
	Exp    int64  `json:"exp"`
	Iat    int64  `json:"iat"`
	Tmp    bool   `json:"tmp,omitempty"` // half-signed-in: waiting for the 2FA code
}

var b64 = base64.RawURLEncoding

func (a *App) sign(c Claims) string {
	head := b64.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	body, _ := json.Marshal(c)
	msg := head + "." + b64.EncodeToString(body)
	m := hmac.New(sha256.New, a.secret)
	m.Write([]byte(msg))
	return msg + "." + b64.EncodeToString(m.Sum(nil))
}

func (a *App) verify(tok string) (*Claims, error) {
	p := strings.Split(tok, ".")
	if len(p) != 3 {
		return nil, errors.New("bad token")
	}
	m := hmac.New(sha256.New, a.secret)
	m.Write([]byte(p[0] + "." + p[1]))
	sig, err := b64.DecodeString(p[2])
	if err != nil || !hmac.Equal(sig, m.Sum(nil)) {
		return nil, errors.New("bad signature")
	}
	raw, err := b64.DecodeString(p[1])
	if err != nil {
		return nil, errors.New("bad token")
	}
	var c Claims
	if json.Unmarshal(raw, &c) != nil || c.Exp < time.Now().Unix() {
		return nil, errors.New("expired")
	}
	if t, ok := a.revoked.Load(c.UID); ok && c.Iat <= t.(int64) {
		return nil, errors.New("revoked")
	}
	return &c, nil
}

// revoke signs a user out everywhere (disabled, role changed, password changed).
func (a *App) revoke(uid int64) {
	now := time.Now().Unix()
	a.revoked.Store(uid, now)
	a.db.wdb.Exec(`UPDATE users SET revoked_at=? WHERE id=?`, now, uid) // survives a restart
}

// loadRevoked restores the sign-out markers after a restart.
func (a *App) loadRevoked() {
	rows, err := a.db.rdb.Query(`SELECT id,revoked_at FROM users WHERE revoked_at>0`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var id, t int64
		rows.Scan(&id, &t)
		a.revoked.Store(id, t)
	}
}

func hashTok(t string) string { s := sha256.Sum256([]byte(t)); return hex.EncodeToString(s[:]) }

type ctxKey struct{}

func userFrom(r *http.Request) *Claims { c, _ := r.Context().Value(ctxKey{}).(*Claims); return c }

func tokenOf(r *http.Request) string {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if tok == "" {
		tok = r.URL.Query().Get("token") // EventSource can't set headers
	}
	return tok
}

// authed: any signed-in user.
func (a *App) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := a.verify(tokenOf(r))
		if err != nil || c.Tmp {
			writeErr(w, 401, "please sign in again")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, c)))
	}
}

// ---------- Login ----------

// bcrypt is deliberately slow (CPU heavy). We cap how many run at once so a
// login storm can't starve the rest of the API; extra logins wait briefly.
var bcryptSlots = make(chan struct{}, max(2, runtime.NumCPU()))

func checkPassword(hash, pass string) (bool, error) {
	select {
	case bcryptSlots <- struct{}{}:
		defer func() { <-bcryptSlots }()
	case <-time.After(5 * time.Second):
		return false, errors.New("busy")
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pass)) == nil, nil
}

var hash0 = hashPassword("timing-equaliser")

// lockout: 6 wrong passwords for one username -> locked for 10 minutes.
var (
	lockMu sync.Mutex
	locks  = map[string]*lockState{}
)

type lockState struct {
	fails int
	until time.Time
	last  time.Time
}

func lockedFor(u string) time.Duration {
	lockMu.Lock()
	defer lockMu.Unlock()
	if l := locks[strings.ToLower(u)]; l != nil && time.Now().Before(l.until) {
		return time.Until(l.until)
	}
	return 0
}

func noteFail(u string) {
	u = strings.ToLower(u)
	lockMu.Lock()
	defer lockMu.Unlock()
	if len(locks) > 20000 {
		locks = map[string]*lockState{}
	}
	l := locks[u]
	if l == nil || time.Since(l.last) > 15*time.Minute {
		l = &lockState{}
		locks[u] = l
	}
	l.fails++
	l.last = time.Now()
	if l.fails >= 6 {
		l.until = time.Now().Add(10 * time.Minute)
		l.fails = 0
	}
}

func clearFails(u string) { lockMu.Lock(); delete(locks, strings.ToLower(u)); lockMu.Unlock() }

type userRow struct {
	ID       int64
	Username string
	Name     string
	Role     string
	Branch   int64
	Hash     string
	Active   bool
	TOTPOn   bool
	TOTPKey  string
	Email    string
	Lang     string
}

func (a *App) findUser(where string, args ...any) (*userRow, error) {
	var u userRow
	err := a.db.rdb.QueryRow(`SELECT id,username,name,role,branch_id,pass_hash,active,totp_on,totp_secret,email,lang FROM users WHERE `+where, args...).
		Scan(&u.ID, &u.Username, &u.Name, &u.Role, &u.Branch, &u.Hash, &u.Active, &u.TOTPOn, &u.TOTPKey, &u.Email, &u.Lang)
	return &u, err
}

func userJSON(u *userRow) map[string]any {
	return map[string]any{"id": u.ID, "username": u.Username, "name": u.Name, "role": u.Role, "branch_id": u.Branch, "lang": u.Lang}
}

func (a *App) issue(w http.ResponseWriter, r *http.Request, u *userRow) {
	now := time.Now().Unix()
	c := Claims{UID: u.ID, User: u.Username, Name: u.Name, Role: u.Role, Branch: u.Branch, Iat: now, Exp: now + 12*3600}
	a.bgExec(`UPDATE users SET last_login=? WHERE id=?`, now, u.ID)
	a.authLog(r, u.Username, u.ID, true, "signed in")
	writeJSON(w, 200, map[string]any{"token": a.sign(c), "user": userJSON(u)})
}

func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	var in struct{ Username, Password string }
	if !readJSON(w, r, &in) {
		return
	}
	in.Username = strings.TrimSpace(in.Username)
	if d := lockedFor(in.Username); d > 0 {
		writeErr(w, 429, fmt.Sprintf("too many wrong attempts, try again in %d minutes", int(d.Minutes())+1))
		return
	}
	u, err := a.findUser(`(username=? OR (email<>'' AND email=?))`, in.Username, in.Username)
	if err != nil || !u.Active {
		checkPassword(hash0, in.Password) // keep timing similar for unknown users
		noteFail(in.Username)
		a.authLog(r, in.Username, 0, false, "unknown user")
		writeErr(w, 401, "wrong username or password")
		return
	}
	ok, err := checkPassword(u.Hash, in.Password)
	if err != nil {
		w.Header().Set("Retry-After", "3")
		writeErr(w, 503, "server is busy, please try again in a moment")
		return
	}
	if !ok {
		noteFail(in.Username)
		a.authLog(r, in.Username, u.ID, false, "wrong password")
		writeErr(w, 401, "wrong username or password")
		return
	}
	clearFails(in.Username)
	if u.TOTPOn {
		now := time.Now().Unix()
		tmp := a.sign(Claims{UID: u.ID, User: u.Username, Tmp: true, Iat: now, Exp: now + 300})
		writeJSON(w, 200, map[string]any{"need_totp": true, "tmp": tmp})
		return
	}
	a.issue(w, r, u)
}

func (a *App) handleTOTPLogin(w http.ResponseWriter, r *http.Request) {
	var in struct{ Tmp, Code string }
	if !readJSON(w, r, &in) {
		return
	}
	c, err := a.verify(in.Tmp)
	if err != nil || !c.Tmp {
		writeErr(w, 401, "please sign in again")
		return
	}
	if d := lockedFor("2fa:" + c.User); d > 0 {
		writeErr(w, 429, "too many wrong codes, try again later")
		return
	}
	u, err := a.findUser(`id=?`, c.UID)
	if err != nil || !u.Active || !totpVerify(u.TOTPKey, in.Code) {
		noteFail("2fa:" + c.User)
		a.authLog(r, c.User, c.UID, false, "wrong 2FA code")
		writeErr(w, 401, "wrong code")
		return
	}
	clearFails("2fa:" + c.User)
	a.issue(w, r, u)
}

// Demo one-click sign-in (only when DEMO=1). Never enable in production.
func (a *App) handleDemoLogin(w http.ResponseWriter, r *http.Request) {
	if !a.cfg.Demo {
		writeErr(w, 404, "not found")
		return
	}
	var in struct{ Username string }
	if !readJSON(w, r, &in) {
		return
	}
	u, err := a.findUser(`username=? AND active=1`, in.Username)
	if err != nil || !isDemoUser(u.Username) {
		writeErr(w, 404, "no such demo user")
		return
	}
	a.issue(w, r, u)
}

func isDemoUser(name string) bool {
	for _, k := range demoRoleOrder {
		if k == name {
			return true
		}
	}
	return false
}

func (a *App) handleDemoAccounts(w http.ResponseWriter, r *http.Request) {
	out := []map[string]string{}
	if a.cfg.Demo {
		rows := a.queryMaps(`SELECT username,name,role FROM users WHERE active=1 ORDER BY id`)
		for _, m := range rows {
			if u := toStr(m["username"]); isDemoUser(u) {
				out = append(out, map[string]string{"username": u, "name": toStr(m["name"]), "role": toStr(m["role"])})
			}
		}
	}
	writeJSON(w, 200, out)
}

func (a *App) handleMe(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	u, err := a.findUser(`id=?`, c.UID)
	if err != nil || !u.Active {
		writeErr(w, 401, "please sign in again")
		return
	}
	mine := map[string]int{}
	for _, m := range Modules {
		mine[m.Key] = a.level(u.Role, m.Key)
	}
	out := userJSON(u)
	out["perms"], out["totp_on"], out["email"] = mine, u.TOTPOn, u.Email
	if ro := a.role(u.Role); ro != nil {
		out["role_label"] = ro.Label
	}
	writeJSON(w, 200, out)
}

func (a *App) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	var in struct{ Old, New string }
	if !readJSON(w, r, &in) {
		return
	}
	if len(in.New) < 8 {
		writeErr(w, 400, "new password must be at least 8 characters")
		return
	}
	u, err := a.findUser(`id=?`, c.UID)
	if err != nil {
		writeErr(w, 401, "please sign in again")
		return
	}
	if ok, _ := checkPassword(u.Hash, in.Old); !ok {
		writeErr(w, 400, "current password is wrong")
		return
	}
	a.db.wdb.Exec(`UPDATE users SET pass_hash=? WHERE id=?`, hashPassword(in.New), c.UID)
	a.audit(r, "users", c.UID, "password", "changed own password")
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---------- password recovery ----------

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (a *App) makeResetLink(uid int64, base string) string {
	tok := randHex(20)
	a.db.wdb.Exec(`INSERT INTO resets(token,user_id,exp) VALUES(?,?,?)`, hashTok(tok), uid, time.Now().Add(time.Hour).Unix())
	return strings.TrimRight(base, "/") + "/login?reset=" + tok
}

func (a *App) handleForgot(w http.ResponseWriter, r *http.Request) {
	var in struct{ Login string }
	if !readJSON(w, r, &in) {
		return
	}
	// same answer whether or not the account exists (no user enumeration)
	resp := map[string]string{"message": "If this account exists and email is set up, a reset link has been sent. Otherwise ask your admin for a reset link."}
	if a.settingBool("recovery", "enabled", true) {
		if u, err := a.findUser(`(username=? OR (email<>'' AND email=?)) AND active=1`, in.Login, in.Login); err == nil {
			// Only e-mail a link when the public address is configured: building it from the
			// request's Host header would let an attacker point the victim's link at their own site.
			if base := a.settingStr("application", "app_url", ""); base != "" && u.Email != "" {
				link := a.makeResetLink(u.ID, base)
				go a.sendMail(u.Email, "Reset your password", "Hello "+u.Name+",\n\nUse this link within 1 hour to set a new password:\n"+link+"\n")
			}
			a.notify(RoleAdmin, 0, "warn", "Password reset requested", u.Username+" asked for a password reset", "#/users")
		}
	}
	writeJSON(w, 200, resp)
}

func (a *App) handleReset(w http.ResponseWriter, r *http.Request) {
	var in struct{ Token, Password string }
	if !readJSON(w, r, &in) {
		return
	}
	if len(in.Password) < 8 {
		writeErr(w, 400, "password must be at least 8 characters")
		return
	}
	var uid, exp int64
	if a.db.rdb.QueryRow(`SELECT user_id,exp FROM resets WHERE token=?`, hashTok(in.Token)).Scan(&uid, &exp) != nil || exp < nowUnix() {
		writeErr(w, 400, "this reset link is invalid or expired")
		return
	}
	a.db.wdb.Exec(`UPDATE users SET pass_hash=? WHERE id=?`, hashPassword(in.Password), uid)
	a.db.wdb.Exec(`DELETE FROM resets WHERE user_id=?`, uid)
	a.revoke(uid)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *App) baseURL(r *http.Request) string {
	if u := a.settingStr("application", "app_url", ""); u != "" {
		return u
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// ---------- two-factor (TOTP, RFC 6238) ----------

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

func totpAt(secret string, t int64) string {
	key, err := b32.DecodeString(strings.ToUpper(secret))
	if err != nil {
		return ""
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(t/30))
	m := hmac.New(sha1.New, key)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0xf
	code := (binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff) % 1000000
	return fmt.Sprintf("%06d", code)
}

func totpVerify(secret, code string) bool {
	code = strings.TrimSpace(code)
	if secret == "" || len(code) != 6 {
		return false
	}
	now := time.Now().Unix()
	for _, d := range []int64{-30, 0, 30} {
		if hmac.Equal([]byte(totpAt(secret, now+d)), []byte(code)) {
			return true
		}
	}
	return false
}

func (a *App) handle2FASetup(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	if !a.settingBool("two_factor", "enabled", true) {
		writeErr(w, 400, "two-factor authentication is turned off by the admin")
		return
	}
	b := make([]byte, 10)
	rand.Read(b)
	secret := b32.EncodeToString(b)
	a.db.wdb.Exec(`UPDATE users SET totp_secret=?,totp_on=0 WHERE id=?`, secret, c.UID)
	issuer := a.settingStr("application", "app_name", "TableBill")
	writeJSON(w, 200, map[string]string{"secret": secret,
		"uri": fmt.Sprintf("otpauth://totp/%s:%s?secret=%s&issuer=%s", issuer, c.User, secret, issuer)})
}

func (a *App) handle2FAEnable(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	var in struct{ Code string }
	if !readJSON(w, r, &in) {
		return
	}
	u, err := a.findUser(`id=?`, c.UID)
	if err != nil || !totpVerify(u.TOTPKey, in.Code) {
		writeErr(w, 400, "that code is not correct, check your authenticator app")
		return
	}
	a.db.wdb.Exec(`UPDATE users SET totp_on=1 WHERE id=?`, c.UID)
	a.audit(r, "users", c.UID, "2fa", "enabled two-factor")
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *App) handle2FADisable(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	var in struct{ Password string }
	if !readJSON(w, r, &in) {
		return
	}
	u, err := a.findUser(`id=?`, c.UID)
	if err != nil {
		writeErr(w, 401, "please sign in again")
		return
	}
	if ok, _ := checkPassword(u.Hash, in.Password); !ok {
		writeErr(w, 400, "password is wrong")
		return
	}
	a.db.wdb.Exec(`UPDATE users SET totp_on=0,totp_secret='' WHERE id=?`, c.UID)
	a.audit(r, "users", c.UID, "2fa", "disabled two-factor")
	writeJSON(w, 200, map[string]bool{"ok": true})
}
