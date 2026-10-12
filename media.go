package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var safeName = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func (a *App) uploadDir() string {
	d := filepath.Join(filepath.Dir(a.cfg.DBPath), "uploads")
	os.MkdirAll(d, 0o755)
	return d
}

// POST /api/media  (multipart, field "file")
func (a *App) handleUpload(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	maxMB := int64(a.settingNum("filesystem", "max_upload_mb", 5))
	r.Body = http.MaxBytesReader(w, r.Body, maxMB<<20+4096)
	if err := r.ParseMultipartForm(maxMB << 20); err != nil {
		writeErr(w, 400, fmt.Sprintf("file is too big (max %d MB)", maxMB))
		return
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		writeErr(w, 400, "choose a file")
		return
	}
	defer f.Close()
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(hdr.Filename), "."))
	allowed := map[string]bool{}
	for _, e := range strings.Split(a.settingStr("filesystem", "allowed", "png,jpg,jpeg,webp,gif,pdf"), ",") {
		allowed[strings.TrimSpace(strings.ToLower(e))] = true
	}
	if !allowed[ext] || ext == "svg" || ext == "html" || ext == "htm" || ext == "js" {
		writeErr(w, 400, "this file type is not allowed")
		return
	}
	head := make([]byte, 512)
	n, _ := f.Read(head)
	mime := http.DetectContentType(head[:n])
	if strings.Contains(mime, "html") || strings.Contains(mime, "javascript") || strings.Contains(mime, "xml") {
		writeErr(w, 400, "this file looks unsafe")
		return
	}
	f.Seek(0, io.SeekStart)
	stored := randHex(12) + "." + ext
	out, err := os.Create(filepath.Join(a.uploadDir(), stored))
	if err != nil {
		writeErr(w, 500, "could not save the file")
		return
	}
	size, _ := io.Copy(out, f)
	out.Close()
	name := safeName.ReplaceAllString(hdr.Filename, "_")
	res, err := a.db.wdb.Exec(`INSERT INTO files(name,mime,size,path,created_by,created_at) VALUES(?,?,?,?,?,?)`, trunc(name, 100), mime, size, stored, c.UID, nowUnix())
	if err != nil {
		writeErr(w, 500, "could not save the file")
		return
	}
	id, _ := res.LastInsertId()
	a.audit(r, "media", id, "upload", name)
	writeJSON(w, 201, map[string]any{"id": id, "name": name, "url": fmt.Sprintf("/media/%d/%s", id, name), "mime": mime, "size": size})
}

// GET /media/{id}/{name}  (public: product images and logos)
func (a *App) handleMediaFile(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var path, mime string
	if a.db.rdb.QueryRow(`SELECT path,mime FROM files WHERE id=?`, id).Scan(&path, &mime) != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src 'self'; style-src 'unsafe-inline'")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeFile(w, r, filepath.Join(a.uploadDir(), filepath.Base(path)))
}

func (a *App) handleMediaList(w http.ResponseWriter, r *http.Request) {
	where, args := "1=1", []any{}
	if q := qStr(r, "q"); q != "" {
		where += " AND name LIKE ?"
		args = append(args, "%"+q+"%")
	}
	limit := int(qInt(r, "limit"))
	if limit < 1 || limit > 200 {
		limit = 40
	}
	page := int(max(qInt(r, "page"), 1))
	var total int64
	a.db.rdb.QueryRow(`SELECT COUNT(*) FROM files WHERE `+where, args...).Scan(&total)
	rows := a.queryMaps(`SELECT id,name,mime,size,created_at FROM files WHERE `+where+` ORDER BY id DESC LIMIT ? OFFSET ?`, append(args, limit, (page-1)*limit)...)
	for _, m := range rows {
		m["url"] = fmt.Sprintf("/media/%d/%s", toInt(m["id"]), toStr(m["name"]))
	}
	writeJSON(w, 200, map[string]any{"rows": rows, "total": total, "page": page, "limit": limit})
}

func (a *App) handleMediaDelete(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var path string
	if a.db.rdb.QueryRow(`SELECT path FROM files WHERE id=?`, id).Scan(&path) == nil {
		os.Remove(filepath.Join(a.uploadDir(), filepath.Base(path)))
		a.db.wdb.Exec(`DELETE FROM files WHERE id=?`, id)
		a.audit(r, "media", id, "delete", path)
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---------- database tools ----------

func (a *App) backupDir() string {
	d := filepath.Join(filepath.Dir(a.cfg.DBPath), "backups")
	os.MkdirAll(d, 0o755)
	return d
}

func (a *App) handleDBInfo(w http.ResponseWriter, r *http.Request) {
	size := int64(0)
	if st, err := os.Stat(a.cfg.DBPath); err == nil {
		size = st.Size()
	}
	wal := int64(0)
	if st, err := os.Stat(a.cfg.DBPath + "-wal"); err == nil {
		wal = st.Size()
	}
	tables := []map[string]any{}
	for _, t := range []string{"records", "orders", "order_items", "payments", "journals", "journal_lines", "stock_moves", "attendance", "audit_log", "auth_log", "users", "files"} {
		var n int64
		a.db.rdb.QueryRow(`SELECT COUNT(*) FROM ` + t).Scan(&n)
		tables = append(tables, map[string]any{"name": t, "rows": n})
	}
	var ver string
	a.db.rdb.QueryRow(`SELECT sqlite_version()`).Scan(&ver)
	backups := []map[string]any{}
	if ents, err := os.ReadDir(a.backupDir()); err == nil {
		for i := len(ents) - 1; i >= 0; i-- {
			if info, err := ents[i].Info(); err == nil && !ents[i].IsDir() {
				backups = append(backups, map[string]any{"name": ents[i].Name(), "size": info.Size(), "at": info.ModTime().Unix()})
			}
		}
	}
	writeJSON(w, 200, map[string]any{"size": size, "wal": wal, "sqlite": ver, "tables": tables, "backups": backups, "path": a.cfg.DBPath})
}

func (a *App) handleDBBackup(w http.ResponseWriter, r *http.Request) {
	name := "tablebill-" + time.Now().Format("20060102-150405") + ".db"
	path := filepath.Join(a.backupDir(), name)
	if _, err := a.db.rdb.Exec(`VACUUM INTO ?`, path); err != nil {
		writeErr(w, 500, "backup failed: "+err.Error())
		return
	}
	a.audit(r, "database", 0, "backup", name)
	writeJSON(w, 200, map[string]string{"name": name})
}

func (a *App) handleDBDownload(w http.ResponseWriter, r *http.Request) {
	name := filepath.Base(r.PathValue("name"))
	if !strings.HasSuffix(name, ".db") {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	http.ServeFile(w, r, filepath.Join(a.backupDir(), name))
	a.audit(r, "database", 0, "download", name)
}

func (a *App) handleDBDelete(w http.ResponseWriter, r *http.Request) {
	name := filepath.Base(r.PathValue("name"))
	os.Remove(filepath.Join(a.backupDir(), name))
	a.audit(r, "database", 0, "delete-backup", name)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *App) handleDBOptimize(w http.ResponseWriter, r *http.Request) {
	a.db.wdb.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	a.db.wdb.Exec(`PRAGMA optimize`)
	// keep logs from growing forever: drop entries older than 180 days
	cut := time.Now().AddDate(0, 0, -180).Unix()
	a.db.wdb.Exec(`DELETE FROM audit_log WHERE ts<?`, cut)
	a.db.wdb.Exec(`DELETE FROM auth_log WHERE ts<?`, cut)
	a.db.wdb.Exec(`DELETE FROM notifications WHERE ts<?`, cut)
	a.audit(r, "database", 0, "optimize", "")
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---------- log pages ----------

func (a *App) handleAuditLog(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	where, args := "1=1", []any{}
	if c.Branch != 0 {
		where += " AND branch_id=?"
		args = append(args, c.Branch)
	}
	if v := qStr(r, "res"); v != "" {
		where += " AND res=?"
		args = append(args, v)
	}
	if v := qStr(r, "user"); v != "" {
		where += " AND user LIKE ?"
		args = append(args, "%"+v+"%")
	}
	if v := qStr(r, "action"); v != "" {
		where += " AND action=?"
		args = append(args, v)
	}
	if v := qInt(r, "from"); v > 0 {
		where += " AND ts>=?"
		args = append(args, v)
	}
	if v := qInt(r, "to"); v > 0 {
		where += " AND ts<?"
		args = append(args, v)
	}
	if v := qStr(r, "q"); v != "" {
		where += " AND detail LIKE ?"
		args = append(args, "%"+v+"%")
	}
	limit := int(qInt(r, "limit"))
	if limit < 1 || limit > 200 {
		limit = 40
	}
	page := int(max(qInt(r, "page"), 1))
	var total int64
	a.db.rdb.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE `+where, args...).Scan(&total)
	rows := a.queryMaps(`SELECT id,ts,user,role,res,rec_id,action,detail,ip FROM audit_log WHERE `+where+` ORDER BY id DESC LIMIT ? OFFSET ?`, append(args, limit, (page-1)*limit)...)
	writeJSON(w, 200, map[string]any{"rows": rows, "total": total, "page": page, "limit": limit})
}

func (a *App) handleAuthLog(w http.ResponseWriter, r *http.Request) {
	where, args := "1=1", []any{}
	if v := qStr(r, "q"); v != "" {
		where += " AND (username LIKE ? OR ip LIKE ?)"
		args = append(args, "%"+v+"%", "%"+v+"%")
	}
	if v := qStr(r, "ok"); v != "" {
		where += " AND ok=?"
		args = append(args, v)
	}
	limit := int(qInt(r, "limit"))
	if limit < 1 || limit > 200 {
		limit = 40
	}
	page := int(max(qInt(r, "page"), 1))
	var total int64
	a.db.rdb.QueryRow(`SELECT COUNT(*) FROM auth_log WHERE `+where, args...).Scan(&total)
	rows := a.queryMaps(`SELECT id,ts,username,ok,ip,agent,note FROM auth_log WHERE `+where+` ORDER BY id DESC LIMIT ? OFFSET ?`, append(args, limit, (page-1)*limit)...)
	writeJSON(w, 200, map[string]any{"rows": rows, "total": total, "page": page, "limit": limit})
}
