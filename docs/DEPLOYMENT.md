# Deployment

## Container
```bash
docker build -f deploy/Dockerfile -t restaurant-billing-backend .
docker run -p 8080:8080 -v backend-data:/data \
  -e ADMIN_PASSWORD=... -e JWT_SECRET=... restaurant-billing-backend
```
The image is a static binary on a minimal non-root base; the database lives in the `/data` volume. Built-in health check: `/server -healthcheck`.

## Hosting panels (Dokploy, Coolify, Railway, Render ...)
1. Create an application from this repository, build type **Dockerfile**, path `deploy/Dockerfile`, context `/`.
2. Environment: `ADMIN_PASSWORD`, `JWT_SECRET` (32+ random characters), `TRUST_PROXY=1`. Leave `DEMO` unset.
3. Add a **persistent volume** mounted at `/data`. Without it the database is lost on every deploy.
4. Port **8080** (or set `PORT`). The frontend reaches it through its `BACKEND_URL`; a public domain is optional.
5. Health check path: `/healthz`. After changing environment variables always **redeploy**.

## Checklist
- [ ] Strong `ADMIN_PASSWORD` and `JWT_SECRET`
- [ ] Persistent volume on `/data`
- [ ] `TRUST_PROXY=1`
- [ ] Regular backups of `/data` (use `sqlite3 tablebill.db ".backup backup.db"`, which is safe while the app runs)
- [ ] Frontend `BACKEND_URL` points here
