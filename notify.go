package main

import (
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/smtp"
	"strings"
	"time"
)

// sendMail sends a plain-text email using the SMTP details in Settings -> Email.
// If email is not configured it only logs (so nothing breaks).
func (a *App) sendMail(to, subject, body string) {
	e := a.settingGroup("email")
	if !toBool(e["enabled"]) || toStr(e["host"]) == "" || to == "" {
		log.Printf("email not sent (not configured): to=%s subject=%s", to, subject)
		return
	}
	host, port := toStr(e["host"]), int(toInt(e["port"]))
	if port == 0 {
		port = 587
	}
	from := toStr(e["from"])
	if from == "" {
		from = toStr(e["username"])
	}
	addr := fmt.Sprintf("%s:%d", host, port)
	msg := "From: " + from + "\r\nTo: " + to + "\r\nSubject: " + strings.ReplaceAll(subject, "\n", " ") +
		"\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + body
	var auth smtp.Auth
	if toStr(e["username"]) != "" {
		auth = smtp.PlainAuth("", toStr(e["username"]), toStr(e["password"]), host)
	}
	var err error
	if toStr(e["encryption"]) == "tls" { // implicit TLS (port 465)
		var conn *tls.Conn
		conn, err = tls.DialWithDialer(&net.Dialer{Timeout: 15 * time.Second}, "tcp", addr, &tls.Config{ServerName: host})
		if err == nil {
			var c *smtp.Client
			if c, err = smtp.NewClient(conn, host); err == nil {
				defer c.Close()
				if auth != nil {
					err = c.Auth(auth)
				}
				if err == nil {
					err = c.Mail(from)
				}
				if err == nil {
					err = c.Rcpt(to)
				}
				if err == nil {
					var wc interface {
						Write([]byte) (int, error)
						Close() error
					}
					if wc, err = c.Data(); err == nil {
						wc.Write([]byte(msg))
						err = wc.Close()
					}
				}
			}
		}
	} else {
		err = smtp.SendMail(addr, auth, from, []string{to}, []byte(msg))
	}
	if err != nil {
		log.Printf("email to %s failed: %v", to, err)
	}
}

// ---------- notification bell ----------

func (a *App) handleNotifications(w http.ResponseWriter, r *http.Request) {
	c := userFrom(r)
	key := fmt.Sprintf("n|%s|%d|%d", c.Role, c.Branch, c.UID)
	data, err := a.notifCache.get(key, 2*time.Second, func() ([]byte, error) {
		rows := a.queryMaps(`SELECT id,ts,level,title,body,link FROM notifications
			WHERE user_id=? OR (user_id=0 AND (role='' OR role=? OR ?='superadmin') AND (branch_id=0 OR branch_id=?))
			ORDER BY id DESC LIMIT 30`, c.UID, c.Role, c.Role, c.Branch)
		return jsonBytes(rows), nil
	})
	if err != nil {
		writeErr(w, 503, "please retry")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}
