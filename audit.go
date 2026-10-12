package main

import (
	"database/sql"
	"net/http"
	"time"
)

// Background write queue: audit rows, login logs and notifications are
// batched into ONE transaction every 250ms instead of one write each, so
// logging never slows the order/payment path.
type bgItem struct {
	q    string
	args []any
}

func (a *App) startBG() {
	a.bg = make(chan bgItem, 8192)
	go func() {
		t := time.NewTicker(250 * time.Millisecond)
		var batch []bgItem
		flush := func() {
			if len(batch) == 0 {
				return
			}
			a.tx(func(tx *sql.Tx) error {
				for _, it := range batch {
					tx.Exec(it.q, it.args...)
				}
				return nil
			})
			batch = batch[:0]
		}
		for {
			select {
			case it := <-a.bg:
				batch = append(batch, it)
				if len(batch) >= 500 {
					flush()
				}
			case <-t.C:
				flush()
			}
		}
	}()
}

// bgExec queues a low-priority write (dropped only if the queue is full).
func (a *App) bgExec(q string, args ...any) {
	select {
	case a.bg <- bgItem{q, args}:
	default:
	}
}

// audit records "who did what" for the Activity Logs page.
func (a *App) audit(r *http.Request, res string, id int64, action, detail string) {
	c := userFrom(r)
	if c == nil {
		return
	}
	a.bgExec(`INSERT INTO audit_log(ts,user_id,user,role,branch_id,res,rec_id,action,detail,ip) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		nowUnix(), c.UID, c.User, c.Role, c.Branch, res, id, action, trunc(detail, 300), a.clientIP(r))
}

func (a *App) authLog(r *http.Request, username string, uid int64, ok bool, note string) {
	okv := 0
	if ok {
		okv = 1
	}
	a.bgExec(`INSERT INTO auth_log(ts,username,user_id,ok,ip,agent,note) VALUES(?,?,?,?,?,?,?)`,
		nowUnix(), trunc(username, 100), uid, okv, a.clientIP(r), trunc(r.UserAgent(), 200), note)
}

// notify creates an in-app notification. role "" = everyone, branch 0 = all.
func (a *App) notify(role string, branch int64, level, title, body, link string) {
	a.bgExec(`INSERT INTO notifications(ts,role,branch_id,level,title,body,link) VALUES(?,?,?,?,?,?,?)`,
		nowUnix(), role, branch, level, trunc(title, 150), trunc(body, 400), link)
	a.hub.Publish("notify")
}

// listRows is a small helper for log pages: returns rows as []map.
func (a *App) queryMaps(q string, args ...any) []map[string]any {
	rows, err := a.db.rdb.Query(q, args...)
	if err != nil {
		return []map[string]any{}
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	out := []map[string]any{}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if rows.Scan(ptrs...) != nil {
			continue
		}
		m := make(map[string]any, len(cols))
		for i, c := range cols {
			if b, ok := vals[i].([]byte); ok {
				m[c] = string(b)
			} else {
				m[c] = vals[i]
			}
		}
		out = append(out, m)
	}
	return out
}
