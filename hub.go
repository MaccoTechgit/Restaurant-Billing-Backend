package main

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

// Hub pushes "something changed" events to every open screen (kitchen,
// orders list, dashboard) over Server-Sent Events. A goroutine costs ~4KB, so
// 50k open connections is fine. Slow clients are skipped, never block writers.
// Screens tied to a branch only receive that branch's events.
// SSE_PER_USER (default 10) caps live connections per signed-in user.
var sseLimit = func() int {
	if n, err := strconv.Atoi(os.Getenv("SSE_PER_USER")); err == nil && n > 0 {
		return n
	}
	return 10
}()

type sub struct {
	ch     chan string
	branch int64 // 0 = receives everything
}

type Hub struct {
	mu      sync.RWMutex
	subs    map[*sub]struct{}
	pmu     sync.Mutex
	pending map[evKey]bool
	cmu     sync.Mutex
	conns   map[int64]int
}

type evKey struct {
	branch int64 // 0 = all branches
	ev     string
}

func NewHub() *Hub {
	h := &Hub{subs: map[*sub]struct{}{}, pending: map[evKey]bool{}, conns: map[int64]int{}}
	go func() { // coalesce bursts: at most one broadcast per event per 300ms
		for range time.Tick(300 * time.Millisecond) {
			h.pmu.Lock()
			evs := h.pending
			h.pending = map[evKey]bool{}
			h.pmu.Unlock()
			for k := range evs {
				h.broadcast(k)
			}
		}
	}()
	return h
}

// Publish notifies every connected screen.
func (h *Hub) Publish(ev string) { h.PublishBranch(0, ev) }

// PublishBranch notifies screens of one branch (and head-office screens).
func (h *Hub) PublishBranch(branch int64, ev string) {
	h.pmu.Lock()
	h.pending[evKey{branch, ev}] = true
	h.pmu.Unlock()
}

func (h *Hub) broadcast(k evKey) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for s := range h.subs {
		if k.branch != 0 && s.branch != 0 && s.branch != k.branch {
			continue
		}
		select {
		case s.ch <- k.ev:
		default: // client too slow: drop, it will refetch on next event
		}
	}
}

func (h *Hub) handle(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming unsupported")
		return
	}
	c := userFrom(r)
	h.cmu.Lock()
	if h.conns[c.UID] >= sseLimit { // a runaway tab / script cannot exhaust the server
		h.cmu.Unlock()
		writeErr(w, 429, "too many open live connections for this user")
		return
	}
	h.conns[c.UID]++
	h.cmu.Unlock()
	defer func() { h.cmu.Lock(); h.conns[c.UID]--; h.cmu.Unlock() }()
	http.NewResponseController(w).SetWriteDeadline(time.Time{}) // stream may outlive WriteTimeout
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	s := &sub{ch: make(chan string, 16), branch: c.Branch}
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.subs, s)
		h.mu.Unlock()
	}()
	fmt.Fprint(w, "retry: 3000\n\n")
	fl.Flush()
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case ev := <-s.ch:
			fmt.Fprintf(w, "data: %s\n\n", ev)
			fl.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		case <-r.Context().Done():
			return
		}
	}
}
