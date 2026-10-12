// Command server runs the Restaurant Billing backend API.
//
// All settings come from environment variables - see README.md and internal/app/util.go (Config).
package main

import (
	"flag"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/maccotechgit/restaurant-billing-backend/internal/app"
)

func main() {
	healthcheck := flag.Bool("healthcheck", false, "probe the running server on this machine and exit 0 (healthy) or 1 - for container health checks")
	flag.Parse()
	if *healthcheck {
		os.Exit(probe())
	}
	app.Run()
}

// probe calls the local /healthz endpoint; minimal container images have no curl.
func probe() int {
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	cl := &http.Client{Timeout: 3 * time.Second}
	resp, err := cl.Get("http://" + addr + "/healthz")
	if err != nil {
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
