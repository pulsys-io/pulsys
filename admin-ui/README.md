# Pulsys Admin UI

Next.js console for Pulsys deployments. Sign-in uses **OIDC + PKCE entirely in
the browser**; the Go backend verifies the resulting `id_token` and sets an
HttpOnly session cookie.

## Quick start (recommended)

From the repo root:

```bash
docker compose up --build
```

Open http://localhost:3000 and sign in with the dev IdP user documented in
[DEVELOPMENT.md](../DEVELOPMENT.md#local-development).

## Native development

Requires Node.js 20+, Postgres, an external OIDC IdP, and `pulsys` with
`PULSYS_DB_DSN` (admin API on port 6060 by default).

Set the redirect URI to match this app:

```text
http://localhost:3001/auth/oidc/callback
```

```bash
cd admin-ui
cp .env.example .env.local   # optional; defaults to http://127.0.0.1:6060
npm install
npm run dev
```

Open [http://localhost:3001](http://localhost:3001).

API requests are proxied to the pulsys admin listener via `next.config.ts`
rewrites (`PULSYS_ADMIN_API`).

## Production (AMI)

The AMI build exports a static console (`PULSYS_STATIC_EXPORT=1`) served
by nginx on port 80.  API routes `/auth/*` and `/admin/*` proxy to pulsys
admin listener — same origin, CSRF cookies work without Next.js rewrites.

```bash
PULSYS_STATIC_EXPORT=1 npm run build   # outputs to out/
```

The static export is served by the proxy's admin plane. See the root
[README](../README.md) for deployment options (Docker and Helm).

## Pages

| Route | Purpose |
|-------|---------|
| `/login` | SSO sign-in |
| `/auth/oidc/callback` | PKCE callback + session establishment |
| `/` | Overview dashboard |
| `/models` | Cache-derived model listing + Import-from-HF trigger |
| `/imports` | Background import jobs (cancel running, delete terminal) |
| `/tokens` | PAT create / revoke |
| `/audit` | Audit log |
| `/users` | User directory (admin role) |
