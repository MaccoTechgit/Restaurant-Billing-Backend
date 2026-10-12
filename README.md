# Restaurant Billing Backend (API)

The Go API behind the restaurant software: POS, kitchen display, orders, inventory, purchasing, customers & loyalty, floor & tables,
delivery, HR (attendance, leave, payroll), double-entry accounting, reports, roles & permissions, audit log and support tickets.
It serves JSON on `/api/...` only - the screens live in the **frontend** repository (Restaurant-Billing-Admin).

[![CI](https://github.com/MaccoTechgit/Restaurant-Billing-Backend/actions/workflows/ci.yml/badge.svg)](https://github.com/MaccoTechgit/Restaurant-Billing-Backend/actions/workflows/ci.yml)

## Quick start
```bash
ADMIN_PASSWORD=<strong password> JWT_SECRET=<long random text> make run     # or: go run ./cmd/server
```
Listens on `:8080`. Data is stored in SQLite (`data/tablebill.db`). Requirements: Go (see `go.mod`); no C compiler needed.

## Project layout
```
cmd/server/        program entry point (flags, then app.Run)
cmd/loadtest/      load generator: simulates many users, order writes and live-update connections
internal/app/      the application (one package, grouped by area - see docs/ARCHITECTURE.md) + tests
deploy/            Dockerfile
docs/              architecture and deployment notes
.github/workflows/ CI (format, vet, test, build, image)
```

## Configuration (environment variables)
| Variable | Meaning | Default |
|---|---|---|
| `ADDR` / `PORT` | listen address / port (`PORT` is used when `ADDR` is not set) | `:8080` |
| `DB_PATH` | SQLite file - keep it on a persistent disk | `data/tablebill.db` |
| `JWT_SECRET` | signing secret, 32+ random characters | random per start (everyone is signed out on restart - set it) |
| `ADMIN_PASSWORD` | first super-admin password | random, printed once |
| `TRUST_PROXY` | `1` when behind the frontend / a reverse proxy, so real client IPs are used | `0` |
| `TLS_CERT`, `TLS_KEY` | serve HTTPS directly | - |
| `DEMO` | `1` enables one-click demo logins (never in production) | `0` |
| `RESTAURANT_NAME`, `CURRENCY` | defaults shown in the app | `MacBill System`, `₹` |
| `SSE_PER_USER` | live-update connections allowed per user | `10` |
| `ALLOW_PRIVATE_URLS` | allow private addresses in integrations | off |
| `DISABLE_RATE_LIMIT` | `1` only for load tests | `0` |

## Connect the frontend
The frontend forwards `/api`, `/media` and live updates here. Deploy this service, then set the frontend's `BACKEND_URL` to this service's address
(for example `https://api.example.com` or a private `http://backend:8080`). Start this service with `TRUST_PROXY=1`.
Health check: `GET /healthz`.

## Development
```bash
make check      # gofmt, go vet, go test - what CI runs
make build      # static binary in bin/server
make docker     # container image
make loadtest   # needs a running server started with DISABLE_RATE_LIMIT=1 SSE_PER_USER=100000
```
More: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md), [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md).
