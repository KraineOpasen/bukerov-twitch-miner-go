---
paths:
  - "internal/web/**"
---

# Web/dashboard conventions

- `server.go` sets up routing/lifecycle and optional HTTP Basic Auth (`DASHBOARD_USERNAME`/`DASHBOARD_PASSWORD`
  env vars); `handlers_*.go` implement dashboard, analytics/JSON, settings, notifications, and status endpoints;
  `status.go` broadcasts miner status over SSE; `viewmodels.go` builds page-specific view models.
- Templates (`internal/web/templates/`, Go `html/template`) and static assets (`internal/web/static/`) are
  embedded into the binary via `//go:embed` in `server.go` — new template/static files must be added under
  those existing directories to be picked up.
- CSS is built by Tailwind from `static/css/input.css` into `static/css/app.css`; don't hand-edit `app.css`.
  JS is vendored (`htmx.min.js`, `apexcharts.min.js`) — no separate JS bundler.
- Never log or render Discord tokens, OAuth tokens, or session cookies in dashboard output, even in debug mode.
- The `analytics` package must stay HTTP-free — HTTP/dashboard concerns belong here, not in `internal/analytics`.
