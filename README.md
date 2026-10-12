# Restaurant Billing Backend (API)

The Go API behind the restaurant software: POS, kitchen display, orders, inventory, purchasing, customers & loyalty, floor & tables,
delivery, HR (attendance, leave, payroll), double-entry accounting, reports, roles & permissions, audit log and support tickets.
It serves JSON on `/api/...` only - the screens live in the **frontend** repository (Restaurant-Billing-Admin).

## Run
```bash
ADMIN_PASSWORD=<strong password> JWT_SECRET=<long random text> go run .
```
Listens on `:8080`. Data is stored in SQLite (`data/tablebill.db`). Without `ADMIN_PASSWORD` a random one is printed on first start.

| Variable | Meaning | Default |
|---|---|---|
| `ADDR` | listen address | `:8080` |
| `DB_PATH` | SQLite file (put it on a persistent disk) | `data/tablebill.db` |
| `JWT_SECRET` | signing secret, 32+ random characters | random per start (everyone is logged out on restart - set it) |
| `ADMIN_PASSWORD` | first super-admin password | random, printed once |
| `TRUST_PROXY` | `1` when behind a reverse proxy / the frontend, so real client IPs are used | off |
| `TLS_CERT`, `TLS_KEY` | serve HTTPS directly | - |
| `DEMO` | `1` enables one-click demo logins (never in production) | `0` |
| `ALLOW_PRIVATE_URLS` | allow private addresses in integrations | off |

## Connect the frontend
The frontend (Restaurant-Billing-Admin) forwards `/api`, `/media` and live updates to this backend. Deploy this service, then set the frontend's
`BACKEND_URL` to this service's address (for example `https://api.example.com` or a private `http://backend:8080`). Set `TRUST_PROXY=1` here.

## Tests and load test
```bash
go test ./...
go run ./loadtest -users 50000 -conns 9000 -sse 4000 -think 15s   # needs SSE_PER_USER set high on the server
```

## Docker
```bash
docker build -t restaurant-backend .
docker run -p 8080:8080 -v backend-data:/data -e ADMIN_PASSWORD=... -e JWT_SECRET=... restaurant-backend
```
