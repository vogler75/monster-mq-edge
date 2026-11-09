# Plan: Brandable dashboard (e.g. "WinCCMQ" with NexusMQ icons)

Status: implemented 2026-11-09 (not committed). Steps 1–4 and 6–8 are done;
step 5 (lint) was dropped in favor of the runtime text swap. Custom logo files
from `config.yaml` (`/brands/custom/*`) and on-the-fly `index.html` rewriting
are not built yet (follow-ups). The MonsterMQ pack stays at the existing paths
(`/assets/logo.png`, `/favicon*`) instead of moving to `src/brands/monstermq/`.
Plain MonsterMQ keeps the monster favicon.

Draft started 2026-11-09. Spans `../dashboard` (most of the work) and this
repo (`internal/dashboard`, `internal/config`).

## Goal

When the dashboard is embedded in the edge broker (`internal/dashboard`,
`//go:embed all:dist`), an operator can replace the product name and the
logo/icons, e.g. show **WinCCMQ** with the **NexusMQ** icon set instead of
**MonsterMQ** + the monster logo. The standalone dashboard, the Electron app
and MonsterMQ Core keep the MonsterMQ branding by default.

Non-goals: per-user theming, recoloring the Siemens iX theme, renaming
technical identifiers (GraphQL types, `MonsterMQ` in JSON schemas, topic names,
log messages, help text that refers to the actual broker products).

**No GraphQL change.** Branding is served as a static/config file next to the
dashboard. It never goes through `/graphql`, so schema parity with Core is
unaffected.

## Current state (what has to change)

- Brand name is hard-coded about 200 times in `dashboard/src`:
  - `<title>MonsterMQ - …</title>` in about 85 pages under `src/pages/*.html`
    plus `src/index.html` and `src/pages/login.html`.
  - `<ix-menu application-name="MonsterMQ">` in every page, and
    `<ix-application-header name="MonsterMQ">` in `src/index.html`.
  - `src/js/sidebar.js`: about dialog (`MonsterMQ Dashboard`, `About MonsterMQ`).
  - `src/pages/login.html`: logo, "Sign in to access your MonsterMQ dashboard".
  - Product text such as "Starlark is supported on both MonsterMQ Edge and
    MonsterMQ Core" and "MonsterMQ MCP Server" (decide case by case, see below).
- Logo/icons: `/assets/logo.png` (used in sidebar, index header, login, about),
  `favicon.ico`, `favicon-16x16.png`, `favicon-32x32.png`,
  `apple-touch-icon.png`. `icons/` holds Electron icons (stay MonsterMQ).
- The SPA (`sidebar.js`) fetches each page, swaps `#main-content` and copies
  `<title>` into `document.title`. It skips the page's own `ix-menu`, so only
  `index.html`'s menu/header are live in SPA mode. That makes runtime
  branding cheap: patch the shell once and rewrite the title on every
  navigation.
- The edge broker already serves the dist as static files and the dashboard
  already fetches `/config/config.json` at runtime. That gives us a channel
  for runtime config that needs no GraphQL.

## Design

### 1. Brand descriptor: `/config/branding.json`

```json
{
  "productName": "WinCCMQ",
  "dashboardName": "WinCCMQ Dashboard",
  "tagline": "Industrial IoT & MQTT 5.0 Gateway Platform",
  "logo": "/brands/nexusmq/logo.svg",
  "favicon": "/brands/nexusmq/favicon.ico",
  "favicon16": "/brands/nexusmq/favicon-16x16.png",
  "favicon32": "/brands/nexusmq/favicon-32x32.png",
  "appleTouchIcon": "/brands/nexusmq/apple-touch-icon.png"
}
```

All fields are optional. Anything missing falls back to the built-in MonsterMQ
defaults, so a missing or broken file always gives today's UI.

### 2. Dashboard: one `branding.js` module

New file `src/js/branding.js`, loaded synchronously early in `index.html` and
`login.html`, before `sidebar.js`:

- `window.branding = { productName, dashboardName, … }` with built-in defaults.
- `branding.load()` fetches `/config/branding.json` (`cache: 'no-store'`,
  short timeout, any failure means defaults), merges, and caches the result in
  `sessionStorage` to avoid a flash of the default brand on the next page.
- `branding.apply(root = document)`:
  - `[data-brand="name"]` sets the text to productName, and
    `[data-brand-src="logo"]` sets `src` to the logo.
  - `ix-menu[application-name]` / `ix-application-header[name]` get their
    attribute set to productName.
  - favicon `<link>` tags get their `href` replaced.
- `branding.title(str)` replaces a leading `MonsterMQ` with productName.
  `sidebar.js` calls it where it does `document.title = newTitle.textContent`.

Source edits (mechanical, scriptable):

- `index.html`, `login.html`, `sidebar-template.html`, about dialog in
  `sidebar.js`: add `data-brand` / `data-brand-src` hooks and use
  `branding.productName` in JS-built strings.
- Page `<title>`s stay `MonsterMQ - X` in source; `branding.title()` rewrites
  them at runtime. That avoids touching 85 files. A later cleanup could switch
  them to just `X` and let `branding` prefix the product name.
- Product-descriptive text ("MonsterMQ Edge and MonsterMQ Core", "MonsterMQ MCP
  Server"): wrap only the user-facing ones in `data-brand="name"` spans.
  Technical references (schemas, envelopes, comments) stay as they are.
- **Reuse the homepage's brand engine idea.** The homepage `index.html`
  ("Dynamic Multi-Branding Engine") writes all text as `MonsterMQ` and swaps
  it at runtime with a `TreeWalker` over text nodes (`swapDomText`), plus
  root classes `brand-monstermq` / `brand-nexusmq`. Adopt the same mechanism
  for the long tail of page text: after every SPA navigation `sidebar.js`
  runs `branding.swapText(#main-content)`, replacing `MonsterMQ` with
  productName. Unlike the homepage, it is scoped and conservative: it skips
  `script, style, code, pre, textarea, input, [contenteditable],
  [data-no-brand]`. Every `MonsterMQ` is replaced, including `MonsterMQ Edge`
  and `MonsterMQ Core` (decided 2026-11-09; static sentences that named both
  products were reworded to "the Edge and the Core broker"). The
  explicit `data-brand` hooks cover the chrome (first paint, attributes such as
  `application-name`), and the text swap covers the remaining text. Most of
  the source sweep in step 3 then becomes unnecessary.
- Do **not** copy the homepage's `?brand=` query and `localStorage` switcher:
  on the broker the brand is set by the operator, not chosen by the viewer.
- A small lint test (`tests/`) to stop regressions: it fails if a user-visible
  `MonsterMQ` literal shows up in `src/pages` / `src/js` outside an allowlist.

### 3. Brand packs in the dashboard build

The NexusMQ assets already exist in the homepage repo
(`github.com/vogler75/monster-mq-homepage`, repo root, commit `4d4da09`):

| Homepage file | Format | Copy to `src/brands/nexusmq/` as | Used for |
|---|---|---|---|
| `logo-nexus.svg` | SVG, 128×128 viewBox | `logo.svg` | sidebar/header/login/about logo (preferred) |
| `logo-nexus.png` | PNG 256×256 | `logo.png` | fallback / Electron-free raster |
| `logo-nexus-small.png` | PNG 100×100 | `logo-small.png` | 48px about-dialog / header slot |
| `logo-nexus.jpg` | JPEG 1024×1024 | (not copied) | marketing only, no alpha |
| `favicon-nexus.ico` | ICO, 32×32 only | `favicon.ico` | `<link rel=icon>` |
| `favicon-nexus-16x16.png` | PNG 16×16 | `favicon-16x16.png` | `<link sizes=16x16>` |
| `favicon-nexus-32x32.png` | PNG 32×32 | `favicon-32x32.png` | `<link sizes=32x32>` |
| `apple-touch-icon-nexus.png` | PNG 180×180 | `apple-touch-icon.png` | `<link rel=apple-touch-icon>` |

So the dashboard gets this layout:

```
src/brands/monstermq/   logo.png, favicon.ico, favicon-16x16.png, favicon-32x32.png, apple-touch-icon.png
                        (moved or copied from today's src/assets/logo.png and src/favicon*)
src/brands/nexusmq/     logo.svg, logo.png, logo-small.png, favicon.ico,
                        favicon-16x16.png, favicon-32x32.png, apple-touch-icon.png
```

Notes:
- The homepage already uses the NexusMQ favicons for **both** brands (its
  `BRAND_DEFS.monstermq` points at `favicon-nexus*`). That matches "generally
  use the NexusMQ icons": a rebranded broker such as WinCCMQ takes the `nexusmq`
  pack plus a free-text product name. NexusMQ is the neutral symbol set, and
  the name comes from config.
- `favicon-nexus.ico` holds only 32×32. Optionally regenerate a
  16/32/48 multi-size `.ico` from the PNGs at copy time; not required.
- The homepage also styles the NexusMQ brand with `.brand-nexusmq .logo-img`
  (cyan drop-shadow) and `.brand-name` (cyan to purple gradient text) in
  `styles.css`. Copy these as an optional `brands/nexusmq/brand.css` that is
  loaded only when the pack is active.
- License/ownership: the assets are ours (same author/org), so no
  `THIRD_PARTY_NOTICES` entry is needed.
- Keep one source of truth: add a `scripts/sync-brand-assets.sh` to the
  dashboard repo that copies the files from a homepage checkout under the
  names above, so later homepage logo updates are a single command.

Vite already copies `src/` into `dist/` (`copyDir`), so both packs ship in
every build and switching brands is a config change only, not a rebuild. The
default `src/config/branding.json` is absent or MonsterMQ, so standalone
deployments stay unchanged.

### 4. Edge broker: brand from `config.yaml`

Extend `DashboardConfig` (`internal/config/config.go`):

```yaml
Dashboard:
  Enabled: true
  Branding:
    ProductName: "WinCCMQ"
    Brand: "nexusmq"          # selects /brands/<Brand>/ icon pack
    # Optional overrides, filesystem paths served under /brands/custom/
    # Logo: /opt/winccmq/logo.svg
    # Favicon: /opt/winccmq/favicon.ico
```

In `internal/dashboard.Handler`:

- Intercept `GET /config/branding.json` and render it from the config. Return
  404 when `Branding` is unset, so the embedded file or defaults apply.
- Optional: serve `/brands/custom/*` from the configured override files so a
  customer can drop in their own logo without rebuilding.
- Optional, nice to have: rewrite `<title>` and `application-name` in
  `index.html` / `login.html` on the fly, so even the first paint and non-JS
  views carry the brand.
- Update `yaml-json-schema.json`, `config.yaml.example` and
  `winccoa/mmq.yaml.example` (the WinCC OA embedding is the main WinCCMQ use
  case, and `embed/cabi` already loads the same `config.Load`).
- Unit test in `internal/dashboard/dashboard_test.go`: branding.json comes out
  of the config, and a 404 comes back when it is unset.

Core (Kotlin) needs nothing. Without the endpoint the dashboard falls back to
MonsterMQ. Core can adopt the same file later if wanted.

### 5. Electron

Out of scope. The Electron app stays MonsterMQ (`icons/`, `electron/`).

## Work breakdown

| # | Repo | Task | Size |
|---|------|------|------|
| 1 | dashboard | Add `branding.js` and the defaults, and load it in `index.html` / `login.html` | S |
| 2 | dashboard | Hook header, menu, login, about and favicons; call `branding.title()` in the SPA title swap | S |
| 3 | dashboard | Scoped `swapText` after SPA navigation (homepage pattern), plus hooks only where the text swap can't reach (attributes, JS-built strings) | S–M |
| 4 | dashboard | Add brand packs: copy the NexusMQ assets from `monster-mq-homepage` and the existing MonsterMQ ones; add `scripts/sync-brand-assets.sh` and the optional `brand.css` | S |
| 5 | dashboard | Lint test against new hard-coded brand strings | S |
| 6 | edge | `Dashboard.Branding` config, schema and examples | S |
| 7 | edge | Serve `/config/branding.json` (and optionally `/brands/custom/*`) from the handler, with tests | S |
| 8 | both | Manual check: standalone (MonsterMQ), edge without branding (MonsterMQ), edge with WinCCMQ/nexusmq, WinCC OA embed | S |

## Open questions

1. ~~NexusMQ assets~~: resolved. They are in `monster-mq-homepage` (see §3).
   Remaining choice: should **MonsterMQ** deployments switch to the NexusMQ
   favicons too, as the homepage already does, or keep the monster favicon?
2. Should "WinCCMQ" also replace the name in descriptive text ("Starlark is
   supported on both MonsterMQ Edge and MonsterMQ Core"), or only in the
   chrome (title, header, menu, login, about)? Proposal: chrome plus generic
   "MonsterMQ broker" mentions; keep explicit Edge/Core product references.
   **Decided:** replace everything, no option.
3. Is a fixed set of packs (`Brand: nexusmq`) enough, or must customers supply
   arbitrary logo files (`Logo:` override)?
4. Should the brand also change the accent colors in `monster-theme.css`?
   Out of scope for now; it could later be a `themeCss` field in
   branding.json.
