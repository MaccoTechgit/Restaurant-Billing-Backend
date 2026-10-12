// Load tester: simulates N restaurant staff, each holding ONE keep-alive
// connection and doing a realistic mix of actions with random think time.
//
//	go run ./loadtest -users 50000 -think 15s -duration 60s
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

var (
	users    = flag.Int("users", 1000, "concurrent users (one connection each)")
	think    = flag.Duration("think", 10*time.Second, "average pause between a user's actions")
	duration = flag.Duration("duration", 30*time.Second, "how long to run after ramp-up")
	ramp     = flag.Duration("ramp", 15*time.Second, "time to bring all users online")
	base     = flag.String("port", "8080", "server port")
	ips      = flag.Int("ips", 8, "spread connections over 127.0.0.1..N (avoids client port exhaustion)")
	conns    = flag.Int("conns", 0, "if >0: users share this many connections (OS socket limits on one test PC)")
	sseN     = flag.Int("sse", 0, "extra idle live-update (SSE) connections")
)

var local func(i int) net.Addr

type result struct {
	lat  []time.Duration
	errs map[int]int
}

var (
	mu       sync.Mutex
	all      = map[string]*result{}
	total    atomic.Int64
	failed   atomic.Int64
	online   atomic.Int64
	sseConns atomic.Int64
	errKinds sync.Map
)

func record(name string, d time.Duration, code int) {
	total.Add(1)
	mu.Lock()
	r := all[name]
	if r == nil {
		r = &result{errs: map[int]int{}}
		all[name] = r
	}
	if code >= 200 && code < 300 || code == 304 {
		r.lat = append(r.lat, d)
	} else {
		r.errs[code]++
		failed.Add(1)
	}
	mu.Unlock()
}

func main() {
	flag.Parse()
	// each source IP has its own ~16k ephemeral ports: use one per target IP
	local = func(i int) net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, byte(1+i%*ips))} }
	host := func(i int) string { return fmt.Sprintf("127.0.0.%d:%s", 1+i%*ips, *base) }

	// get a token once (login cost is tested separately)
	resp, err := http.Post("http://"+host(0)+"/api/auth/demo", "application/json", bytes.NewBufferString(`{"username":"superadmin"}`))
	if err != nil {
		panic(err)
	}
	var lr struct{ Token string }
	json.NewDecoder(resp.Body).Decode(&lr)
	resp.Body.Close()
	tok := "Bearer " + lr.Token
	var pids []int64
	{
		rq, _ := http.NewRequest("GET", "http://"+host(0)+"/api/menu", nil)
		rq.Header.Set("Authorization", tok)
		if rs, err := http.DefaultClient.Do(rq); err == nil {
			var m struct {
				Products []struct{ ID int64 } `json:"products"`
			}
			json.NewDecoder(rs.Body).Decode(&m)
			rs.Body.Close()
			for _, p := range m.Products {
				pids = append(pids, p.ID)
			}
		}
		if len(pids) == 0 {
			panic("no products - is the server seeded?")
		}
	}

	pool := make([]*http.Client, *conns)
	for k := range pool {
		addr := host(k)
		pool[k] = &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
			DialContext: func(ctx context.Context, n, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: 20 * time.Second, LocalAddr: local(k)}).DialContext(ctx, n, addr)
			},
			MaxConnsPerHost: 4, MaxIdleConns: 4, IdleConnTimeout: 5 * time.Minute,
		}}
	}
	ctx, cancel := context.WithTimeout(context.Background(), *ramp+*duration)
	defer cancel()
	var wg sync.WaitGroup
	var measuring atomic.Bool

	for i := 0; i < *users; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			time.Sleep(time.Duration(rand.Int63n(int64(*ramp)))) // staggered arrival
			var cl *http.Client
			if *conns > 0 {
				cl = pool[i%*conns]
			} else {
				addr := host(i)
				tr := &http.Transport{
					DialContext: func(ctx context.Context, n, _ string) (net.Conn, error) {
						return (&net.Dialer{Timeout: 20 * time.Second, LocalAddr: local(i)}).DialContext(ctx, n, addr)
					},
					MaxIdleConns:    1,
					IdleConnTimeout: 5 * time.Minute,
				}
				cl = &http.Client{Transport: tr, Timeout: 30 * time.Second}
				defer tr.CloseIdleConnections()
			}
			do := func(name, method, path string, body []byte) {
				var rd io.Reader
				if body != nil {
					rd = bytes.NewReader(body)
				}
				req, _ := http.NewRequestWithContext(ctx, method, "http://x"+path, rd)
				req.Header.Set("Authorization", tok)
				if body != nil {
					req.Header.Set("Content-Type", "application/json")
				}
				t := time.Now()
				res, err := cl.Do(req)
				code := 0
				if err == nil {
					io.Copy(io.Discard, res.Body)
					res.Body.Close()
					code = res.StatusCode
				} else if ctx.Err() != nil {
					return
				} else {
					k := err.Error()
					if len(k) > 90 {
						k = k[len(k)-90:]
					}
					c, _ := errKinds.LoadOrStore(k, new(atomic.Int64))
					c.(*atomic.Int64).Add(1)
				}
				if measuring.Load() {
					record(name, time.Since(t), code)
				}
			}
			online.Add(1)
			for ctx.Err() == nil {
				switch p := rand.Intn(100); {
				case p < 55:
					do("GET menu", "GET", "/api/menu", nil)
				case p < 85:
					do("GET orders", "GET", "/api/pos/orders?status=active&limit=30&items=1", nil)
				case p < 95:
					do("GET dashboard", "GET", "/api/dash/hub?tz=330", nil)
				default:
					do("POST order", "POST", "/api/pos/orders", []byte(fmt.Sprintf(`{"channel":"takeaway","items":[{"product_id":%d,"qty":%d},{"product_id":%d,"qty":1}]}`, pids[rand.Intn(len(pids))], 1+rand.Intn(3), pids[rand.Intn(len(pids))])))
				}
				select {
				case <-time.After(time.Duration(float64(*think) * (0.5 + rand.Float64()))):
				case <-ctx.Done():
				}
			}
		}(i)
	}

	for i := 0; i < *sseN; i++ {
		go func(i int) {
			time.Sleep(time.Duration(rand.Int63n(int64(*ramp))))
			addr := host(i + 3)
			cl := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, n, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: 20 * time.Second, LocalAddr: local(i + 3)}).DialContext(ctx, n, addr)
			}}}
			req, _ := http.NewRequestWithContext(ctx, "GET", "http://x/api/events?token="+lr.Token, nil)
			res, err := cl.Do(req)
			if err != nil {
				return
			}
			sseConns.Add(1)
			io.Copy(io.Discard, res.Body)
		}(i)
	}

	// progress + start measuring when everyone is online
	start := time.Now()
	go func() {
		for range time.Tick(5 * time.Second) {
			fmt.Printf("  t=%3.0fs online=%d sse=%d requests=%d failed=%d\n", time.Since(start).Seconds(), online.Load(), sseConns.Load(), total.Load(), failed.Load())
		}
	}()
	time.Sleep(*ramp + 3*time.Second)
	measuring.Store(true)
	mStart := time.Now()
	<-ctx.Done()
	measuring.Store(false)
	secs := time.Since(mStart).Seconds()
	wg.Wait()

	fmt.Printf("\n=== %d users, %d sse, think %v, measured %.0fs ===\n", *users, sseConns.Load(), *think, secs)
	var n, bad int
	names := []string{}
	for k := range all {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		r := all[k]
		sort.Slice(r.lat, func(i, j int) bool { return r.lat[i] < r.lat[j] })
		e := 0
		for _, c := range r.errs {
			e += c
		}
		n += len(r.lat) + e
		bad += e
		pct := func(p float64) time.Duration {
			if len(r.lat) == 0 {
				return 0
			}
			return r.lat[int(float64(len(r.lat)-1)*p)].Round(time.Microsecond)
		}
		fmt.Printf("%-14s ok=%-7d err=%-5d p50=%-10v p95=%-10v p99=%-10v max=%v %v\n", k, len(r.lat), e, pct(.5), pct(.95), pct(.99), pct(1), r.errs)
	}
	errKinds.Range(func(k, v any) bool { fmt.Printf("  client error x%d: %v\n", v.(*atomic.Int64).Load(), k); return true })
	fmt.Printf("TOTAL %d requests, %.0f req/s, errors %d (%.2f%%)\n", n, float64(n)/secs, bad, float64(bad)*100/float64(max(n, 1)))
}
