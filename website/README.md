# Pulsys marketing site

Static [Astro](https://astro.build/) site with [Starlight](https://starlight.astro.build/) docs at `/docs`. Benchmark images and headline stats are copied from `../docs/` at build time.

Licensed under Apache-2.0, the same as the root [`LICENSE`](../LICENSE).

## Configuration

Project links live in **`src/site.config.ts`**. Override at build time:

| Variable | Default | Purpose |
|----------|---------|---------|
| `PUBLIC_REPO_URL` | `https://github.com/pulsys-io/pulsys` | GitHub links, Starlight edit URLs |
| `PUBLIC_SITE_URL` | `https://pulsys-io.github.io` | Canonical site origin |
| `PUBLIC_BASE_PATH` | `/pulsys` | Astro `base` (use `/` when serving at `pulsys.io` root) |

## Build pipeline

On `npm run dev` / `npm run build`:

1. **`scripts/sync-docs.mjs`** — copies `../docs/*.md` → `src/content/docs/docs/` (Starlight).
2. **`scripts/sync-benchmarks.mjs`** — copies `../docs/results/` → `public/results/` and writes `src/generated/headline.ts` from `headline.json`.

Edit documentation in the repo root [`docs/`](../docs/), not under `website/`. Rebuild to refresh the site.

Generated paths (`src/content/`, `src/generated/`, `public/results/`) are gitignored.

`package.json` pins `@astrojs/sitemap@3.1.6` via `overrides` (Starlight’s default sitemap requires Astro 5).

## Commands

```bash
cd website
npm install
npm run dev      # http://localhost:4321/pulsys/ (default base path)
npm run build    # output in dist/
npm run preview  # serve dist/
```

Brand assets (favicon, OG image, GitHub avatar) are rendered from the SVG mark
with `node scripts/render-brand.mjs`; outputs in `public/` are checked in.

Local dev with site root at `/`:

```bash
PUBLIC_BASE_PATH=/ PUBLIC_SITE_URL=http://localhost:4321 npm run dev
```

## Deploy (GitHub Pages)

Push to `main` triggers [`.github/workflows/pages.yml`](../.github/workflows/pages.yml):

- Project URL: `https://pulsys-io.github.io/pulsys/`
- With custom domain and `PUBLIC_SITE_URL=https://pulsys.io`, `PUBLIC_BASE_PATH=/`

## Plausible analytics

Analytics is opt-in. The layout only injects the script when `PUBLIC_PLAUSIBLE_DOMAIN` is set.

```bash
PUBLIC_PLAUSIBLE_DOMAIN=example.com
PUBLIC_PLAUSIBLE_SCRIPT_SRC=https://plausible.example.com/js/script.js  # optional
```
