package main

import (
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------- Config (all via environment variables) ----------

type Config struct {
	Addr       string
	DBPath     string
	Secret     string
	Demo       bool
	TrustProxy bool
	Currency   string
	Restaurant string
	NoLimit    bool // DISABLE_RATE_LIMIT=1, for load testing only
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func loadConfig() Config {
	return Config{
		Addr:       env("ADDR", ":8080"),
		DBPath:     env("DB_PATH", "data/tablebill.db"),
		Secret:     env("JWT_SECRET", ""),
		Demo:       env("DEMO", "0") == "1", // demo = passwordless sign-in buttons: OFF unless you ask for it
		TrustProxy: env("TRUST_PROXY", "0") == "1",
		Currency:   env("CURRENCY", "₹"),
		Restaurant: env("RESTAURANT_NAME", "TableBill Restaurant"),
		NoLimit:    env("DISABLE_RATE_LIMIT", "0") == "1",
	}
}

// ---------- JSON helpers ----------

func nowUnix() int64 { return time.Now().Unix() }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeErr(w, 400, "invalid request")
		return false
	}
	return true
}

func pathID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id
}

// ---------- Rate limiter (token bucket per IP, sharded to avoid lock contention) ----------

type bucket struct {
	tokens float64
	last   time.Time
}

type shard struct {
	mu sync.Mutex
	m  map[string]*bucket
}

type Limiter struct {
	shards [64]shard
	rate   float64 // tokens per second
	burst  float64
}

func NewLimiter(rate, burst float64) *Limiter {
	l := &Limiter{rate: rate, burst: burst}
	for i := range l.shards {
		l.shards[i].m = map[string]*bucket{}
	}
	go func() { // drop idle entries
		for range time.Tick(2 * time.Minute) {
			for i := range l.shards {
				s := &l.shards[i]
				s.mu.Lock()
				for k, b := range s.m {
					if time.Since(b.last) > 5*time.Minute {
						delete(s.m, k)
					}
				}
				s.mu.Unlock()
			}
		}
	}()
	return l
}

func (l *Limiter) Allow(key string) bool {
	h := uint32(2166136261)
	for i := 0; i < len(key); i++ {
		h = (h ^ uint32(key[i])) * 16777619
	}
	s := &l.shards[h%64]
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.m[key]
	if b == nil {
		b = &bucket{tokens: l.burst, last: now}
		s.m[key] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (a *App) clientIP(r *http.Request) string {
	if a.cfg.TrustProxy {
		if x := r.Header.Get("X-Forwarded-For"); x != "" {
			return strings.TrimSpace(strings.Split(x, ",")[0])
		}
	}
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

func (a *App) limit(l *Limiter, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.cfg.NoLimit && !l.Allow(a.clientIP(r)) {
			w.Header().Set("Retry-After", "2")
			writeErr(w, 429, "too many requests, slow down a little")
			return
		}
		next(w, r)
	}
}

// ---------- Global middleware ----------

func middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if e := recover(); e != nil {
				log.Printf("panic: %v\n%s", e, debug.Stack())
				writeErr(w, 500, "something went wrong")
			}
		}()
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: blob: https:; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'; object-src 'none'")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(self)")
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		start := time.Now()
		next.ServeHTTP(w, r)
		if d := time.Since(start); d > 500*time.Millisecond && !strings.HasSuffix(r.URL.Path, "/events") {
			log.Printf("slow request %s %s took %v", r.Method, r.URL.Path, d)
		}
	})
}

// trunc trims spaces and cuts a string to n characters (not bytes).
func trunc(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
