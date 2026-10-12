# Architecture

One Go service, one SQLite database, one package (`internal/app`) organised by area. Files are named after what they own.

## Layers inside `internal/app`
| Area | Files | Role |
|---|---|---|
| Start-up and wiring | `main.go`, `util.go` (config, helpers), `seed.go` | build the app, load settings and roles, start background workers, HTTP server with graceful shutdown |
| Storage | `db.go` | SQLite (WAL): one serialised writer connection, a pool of readers; transactions via `a.tx(...)` |
| Identity and access | `auth.go`, `users.go`, `perms.go`, `audit.go` | JWT sessions, login lockout, 2FA (TOTP), roles x modules x levels (none / view / manage / approve), branch isolation, audit trail |
| Generic record engine | `resource.go`, `res_*.go` | field specs, validation, hooks, actions, search and exports for most master data (menu items, customers, employees ...) |
| Selling | `pos.go`, `pos_read.go`, `menu.go`, `qr.go` | orders, kitchen status, payments, receipts, public QR ordering |
| Stock | `inventory.go` | stock ledger, purchasing, wastage |
| Money | `ledger.go`, `acc.go` | double-entry journal, chart of accounts, period close, statements |
| People | `hr.go` | attendance, leave, payroll |
| Delivery | `delivery.go` | drivers, cash on delivery, public tracking |
| Insight | `reports.go`, `settings.go`, `nav.go` | reports and dashboards, settings groups, role-aware navigation |
| Platform | `hub.go` (live updates / SSE), `cache.go` (stale-while-revalidate caches), `notify.go`, `support.go`, `media.go` | cross-cutting services |

## Design decisions
- **SQLite with one writer** keeps the system simple and fast for a restaurant or a small chain; reads scale through the reader pool and caches.
  A multi-restaurant (SaaS) deployment should run one instance per restaurant or move storage to a shared database.
- **Stateless sessions** (signed tokens) with a persisted revocation time per user, so sign-out-everywhere survives restarts.
- **Resource engine**: new master data is a field-spec string, not a new handler - validation, permissions and audit come for free.
- **Money is stored in minor units** (integers); every sale posts a balanced journal entry; failures to post are recorded, never block payment.
- **Security**: strict headers, per-IP rate limits, branch scoping on every write, secret fields never returned, upload and URL validation.

## Testing
`internal/app/app_test.go` covers security (injection, traversal, workflow forgery, secret masking), roles and branch isolation, printing and
order flows. `cmd/loadtest` simulates tens of thousands of users to find bottlenecks.

## Growing the codebase
When an area outgrows its files (for example accounting), move it to its own package under `internal/` with a small interface to the rest
(`internal/accounting`, `internal/inventory`, ...). The current grouping by file names is the preparation for that step.
