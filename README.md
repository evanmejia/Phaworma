# Phaworma

My self-hosted, ephemeral, privacy-first form builder — a lean Microsoft
Forms alternative that runs as a single Go binary (PocketBase extended as an
application framework) on one Proxmox LXC and is served at
`https://forms.evanmejia.com` through my Nginx Proxy Manager.

## The promise

Every form I create gets its own isolated PocketBase data collection and a
lifespan (1 day up to 1 month). When the lifespan ends, a cron job drops the
dynamic table from disk and the short link dies (410 Gone). No permanent
footprint.

## Stage status

| Stage | Scope | Status |
|-------|-------|--------|
| 1 | Scaffold: directory structure, PocketBase integration, route skeletons, unified SPA shell | done |
| 2 | Form builder + JSON schema, short links + access passwords, per-form dynamic collections, respondent submissions, results panel, hourly purge cron | done |
| 3 | Multi-user: regular-user creators (`users` collection), clean `/f/<code>` URLs (no hash routing), co-creation by email invite with pending seats, owner-only actions, user-deletion privacy cascade | done |
| 4 | Schema editing for live forms, charts/datasheet polish, purge hardening, rate limiting + `TrustedProxy` | later |

## Quickstart (development)

Requirements: a recent Go toolchain. `go.mod` pins `go 1.27` (PocketBase
v0.40 requires it) — if my local Go is older, the toolchain auto-download
handles it.

```bash
# 1. build
go build -o phaworma .

# 2. create my superuser (first run; creates pb_data/)
./phaworma superuser upsert admin@forms.evanmejia.com 'change-me-please'

# 3. serve
./phaworma serve --http 0.0.0.0:8090
```

Then open:

- `http://127.0.0.1:8090/` — my SPA. The path is the state: `/` (guest
  hero / my form list), `/f/<code>` (respond), `/forms/new` (builder),
  `/forms/<id>` (edit), `/forms/<id>/results`, `/forms/<id>/share`
- `http://127.0.0.1:8090/_/` — the PocketBase admin dashboard (superuser
  login — the ONLY surface for the platform admin)

My frontend's only dependency is the official PocketBase JS SDK, vendored at
`pb_public/js/vendor/pocketbase.mjs` — no CDN, ever (privacy-first).

## HTTP endpoints

### Public (respondents — no auth)

| Method | Path | Purpose |
|--------|------|---------|
| GET | `/f/{shortCode}` | The form's canonical public page. An explicit Go route: SPA shell for live forms, 410 Gone if expired, 404 if unknown. Case-insensitive code. |
| GET | `/s/{shortCode}` | Legacy short link. 301 → `/f/<code>` (410/404 answers match `/f/`). Case-insensitive code. |
| GET | `/api/v1/public/forms/{shortCode}` | Form title + question schema. Behind the access-password gate when set (`X-Access-Password` header). |
| POST | `/api/v1/forms/{shortCode}/submissions` | `{answers: {questionId: value}}`. Unknown keys dropped; PocketBase field validation → 400 with per-field messages. |

### Creator (regular-user token in `Authorization: Bearer`)

Superuser tokens get 403 here — the platform admin lives on the built-in
dashboard, and the separation is structural. Every form-scoped route checks
PER-FORM access (owner, active co-creator, or a pending seat matching my
email); forms I can't access answer 404, never 403.

| Method | Path | Purpose |
|--------|------|---------|
| POST | `/api/v1/forms` | Create form (owner = the authenticated user, never the body) + its dynamic collection (rollback if the table fails). |
| GET | `/api/v1/forms` | List what I may see: forms I own + forms where I hold a co-creator seat (newest first, with submission counts). |
| GET | `/api/v1/forms/{id}` | Form metadata + schema (the builder's edit mode). |
| PUT | `/api/v1/forms/{id}` | Title / lifespan (re-anchors expiry) / access password. Owners AND co-creators; the schema stays frozen. |
| DELETE | `/api/v1/forms/{id}` | Owner only. Drop the dynamic table AND delete the record (short link → 404). |
| GET | `/api/v1/forms/{id}/submissions` | Raw datasheet, newest first, max 1000 rows. |
| GET | `/api/v1/forms/{id}/results` | Per-question aggregations, zero-filled option counts. |
| GET | `/api/v1/forms/{id}/collaborators` | The form's co-creator seats (anyone with form access). |
| POST | `/api/v1/forms/{id}/collaborators` | Owner only. `{"email": …}` — existing account → active seat (case-insensitive match); otherwise pending, auto-activated when that address signs up. Unique per (form, email). |
| DELETE | `/api/v1/forms/{id}/collaborators/{collabId}` | Owner only. Revoke a seat. |

Creator responses never serialize the access-password hash/salt — only an
`accessPasswordSet` boolean.

## Environment

| Variable | Default | Purpose |
|----------|---------|---------|
| `FORMS_PURGE_CRON` | `0 * * * *` (hourly) | Cron spec for the expiry purge. I use `* * * * *` when I test purging by hand. |

The purge drops each expired form's dynamic collection (SQLite `DROP TABLE`
via PocketBase's collection-delete hook) and marks the form `expired`. The
form record itself is KEPT on purpose: it holds no respondent data, and
keeping it lets `/s/<code>` answer 410 Gone ("expired") instead of a bare
404. There's also a 5-minute clock-skew buffer so a form never dies a few
seconds early, and boot-time reconciliation marks past-due forms expired as
a safety net for crashes between ticks.

## Deploying to the LXC

1. Build on the LXC (or cross-compile) and copy the binary next to
   `pb_public/` — the binary looks for `pb_public/` next to itself.
2. Create the superuser:
   `./phaworma superuser upsert admin@forms.evanmejia.com '<strong-password>'`
3. Run it under systemd (below).

### systemd unit

```ini
[Unit]
Description=Phaworma (PocketBase app)
After=network-online.target
Wants=network-online.target

[Service]
User=forms
WorkingDirectory=/opt/forms
ExecStart=/opt/forms/phaworma serve --http 0.0.0.0:8090
Restart=on-failure
# I run as a dedicated unprivileged user; /opt/forms (and the pb_data dir
# inside it) must be owned by that user.

[Install]
WantedBy=multi-user.target
```

### Nginx Proxy Manager (192.168.40.40)

- Proxy host: `forms.evanmejia.com` → `http://<forms-LXC-IP>:8090`
- Force Let's Encrypt: on
- Websockets: not needed yet (my results panel is fetch-based, no
  PocketBase realtime) — keep it off until I wire live counts.

TLS terminates at NPM. The app itself speaks plain HTTP on the Proxmox
bridge and trusts NPM's `X-Forwarded-For` / `X-Forwarded-Proto` headers.
**Never expose 8090 publicly without NPM in front.** When I add rate
limiting in Stage 3, I'll also set PocketBase's `TrustedProxy` setting to
NPM's IP so `RealIP()` resolves the real client address.

## Project layout

```
main.go              entrypoint: OnBootstrap provisioning, OnServe routes, record hooks, cron registration
schema.go            my JSON schema types + validation (text / choice / dropdown, max 50 questions)
security.go          short-code generation (crypto/rand, 32-char alphabet, collision retry)
                     + access-password hashing (per-form salt, timing-safe compare)
collections.go       forms + dyn_<formId> collection provisioning, boot reconciliation, purge helpers
users.go             users + collaborators collection provisioning, forms.owner relation,
                      signup (pending-seat activation) and deletion (owned-forms cascade) hooks
purge.go             the expiry cron body (skew buffer, drop table, mark expired)
routes_public.go     /f/{shortCode} page, /s/ 301 redirector, public schema fetch, submissions
routes_creator.go    user-gated creator API + per-form co-creator authorization
go.mod / go.sum      pinned PocketBase module (v0.40.6)
pb_public/
  index.html         SPA shell: 7 pathname views + tabbed auth <dialog> + toasts
  css/app.css        my design tokens + Grid/Flexbox layout (no framework)
  js/app.js          pathname router, API layer, view controllers, guest draft persistence,
                      inline SVG charts
  js/vendor/         vendored PocketBase JS SDK v0.28.2 (never a CDN)
pb_data/             runtime SQLite data (gitignored, never committed)
```

## Auth model

Stage 3 is multi-user, with three roles:

- **Superuser** — the platform admin. Lives ONLY on the built-in PocketBase
  dashboard (`/_/`). My creator API rejects superuser tokens with 403:
  `requireUser` checks `IsSuperuser()` explicitly, because a superuser token
  IS a valid auth record (`RequestInfo.Auth` is the superuser row) — a nil
  check alone would let the admin silently use the creator API.
- **Users** (the `users` auth collection) — the creators. Public signup
  from the SPA's auth dialog (public create rule; self-only
  list/view/update/delete), no email confirmation. Users own forms and
  invite co-creators.
- **Co-creators** (a `collaborators` row) — a per-form access grant. One
  row = one (form, email) pair; the `user` link fills in the moment the
  invited address signs up.

Form access = owner, OR an active co-creator seat, OR a pending seat whose
email matches mine (my fallback in case the signup hook missed a race). A
form I can't access answers **404, never 403** — my API doesn't leak that a
form exists. Owner-only actions (delete, invite, revoke) answer 403 to a
co-creator, who already knows the form exists.

Invites address an **EMAIL, not an account**: if the address has an account
the seat is created ACTIVE (case-insensitive match — seat rows store the
lowercased address, accounts keep their typed case); otherwise PENDING, and
my signup hook flips it active the moment that address registers. The
unique (form, email) index makes re-invites a no-op/409.

**Deletion is privacy-safe.** When a user is deleted (self-delete via the
API, or the superuser doing it in the dashboard), my `OnRecordDelete` hook
runs the FULL delete for every form they own — dynamic table dropped, then
form record — BEFORE the user row goes. The relation cascade alone would
orphan my `dyn_*` tables, and respondent data outliving its creator is
exactly what this product exists to prevent. A co-creator's deletion only
removes their seat rows (the form and its data survive).

Respondents have no accounts and never touch the native PocketBase REST API.
Every collection I create (`forms`, every `dyn_<formId>`, and
`collaborators`) carries the deny rule `@request.auth.role = 'admin'` on all
five rule slots — my `/api/v1` endpoints do the real authorization work.

## Notes for my future self

- Every inline comment in this repo is first person ("I", "my") on purpose —
  they're my scratchpad: architecture choices and the security edge cases I
  must not forget (timing-safe password compare, shortCode → collection
  mapping only server-side, expiry clock-skew buffer, idempotent boot-time
  provisioning).
- PocketBase v0.40 lifecycle is `OnBootstrap → OnServe → OnTerminate` (the
  old `OnBeforeServe` hook is gone) — my boot-time collection provisioning
  lives on `OnBootstrap`, my routes on `OnServe`.
- **v0.40 gotcha — collection names are EXACT in the native API.** My
  superuser collection is named `_superusers`, so the auth endpoint is
  `POST /api/collections/_superusers/auth-with-password` and the frontend
  must use `pb.collection("_superusers")`. (`/api/collections/superusers/…`
  → 404 "Missing collection context".)
- **v0.40 gotcha — no implicit `created`/`updated`.** Timestamps are regular
  `AutodateField`s I must declare myself (`OnCreate` / `OnUpdate`), and
  `record.Set("created", …)` is a deliberate no-op. My `forms` and `dyn_*`
  collections declare them explicitly.
- **v0.40 gotcha — table sync diffs against the CACHED collection.** If I
  mutate a cached `*core.Collection` in place and `app.Save()` it, the
  internal `SyncRecordTableSchema` sees old == new and adds NO columns
  (the field lands in `_collections` JSON only). I snapshot the JSON first
  and re-run the sync against the true pre-mutation state — see
  `tableHasColumn` / `stampCreatedField` in `collections.go`.
- **v0.40 list-rule semantics: rules act as WHERE filters.** An
  unauthenticated native list against a denied collection answers 200 with
  `{"items":[]}` — not 401/403. Guests see nothing, which is the lockdown
  I want; don't "fix" the 200.
- **Router `errData` goes through `safeErrorsData`** — only ozzo
  `validation.Errors` renders sensibly; any other map gets each value
  replaced with a `validation_invalid_value` error. I pass `nil` for plain
  401s/500s and log the real error server-side instead of shipping it.
- `app.Save(record)` returns ozzo `validation.Errors` directly on field
  failures (not an `ApiError`) — I convert with `errors.As` → 400.
- Internal query sorts use the web format: `"-created"`, never
  `"created DESC"`.
- Sort/cron internals: my purge cron and my boot reconciliation both key off
  `expires_at` + `status`, so a form can never outlive its date in the gap
  between two ticks.
- **v0.40 auth endpoints dropped the `/records` segment.** Sign-in is
  `POST /api/collections/{name}/auth-with-password` (the old
  `.../records/auth-with-password` 404s). The vendored SDK v0.28.2 already
  targets the new shape.
- **Auth-create responds `{token, record}`, and the SDK's `create()` does
  NOT save it.** Only `authWithPassword()` auto-saves. My signup handler
  calls `create()` and then `pb.authStore.save(result.token, result.record)`
  explicitly — the guest publish flow depends on that session existing.
- **`RequestInfo.Auth` is the SUPERUSER record for superuser tokens.** A
  nil-check cannot separate the roles; `requireUser` must also reject
  `info.Auth.IsSuperuser()` (and verify the collection is `users`).
- **Record hooks are GLOBAL in v0.40** — `app.OnRecordDelete()` fires for
  every record in every collection, including dashboard-initiated changes.
  My hooks filter `re.Record.Collection().Name` in the handler body. The
  pre-`OnRecordDelete` hook runs BEFORE the delete transaction, so nested
  `app.Delete(form)` calls in my cascade are safe (own transactions).
- **`CascadeDelete` flows referenced → referencing.** Deleting a `forms` row
  cascades into `collaborators` rows (the `form` relation); deleting a
  `users` row cascades into their `collaborators` seats (the `user`
  relation). It does NOT propagate the other way — which is why
  `forms.owner` must stay `CascadeDelete=false` (my hook owns that path).
- **Email case is a live bug.** PocketBase stores auth emails in the case
  typed at signup, and `FindAuthRecordByEmail` is case-SENSITIVE unless the
  collection's email index is NOCASE (the stock `users` index here isn't).
  My seat rows store lowercased addresses, so the invite path uses my own
  `findUserByEmail` (`COLLATE NOCASE` expression). Never assume
  case-insensitivity from the stock helpers.
- **The PocketBase logger batches to the aux DB** and only prints to stdout
  in dev mode — `app.Logger().Info(...)` from my handlers is invisible in a
  plain `serve`. Debug by returning the data in the response body.
- **Static serving already falls back to `index.html`** for unknown GET
  paths, which is what makes my `/forms/...` deep links work on refresh.
  My explicit `/f/{shortCode}` route beats the catch-all (Echo prefers a
  param segment over the wildcard). Don't register a second catch-all.
- My test artifacts live in `/tmp/opencode/`: `fs_stage3_tests.sh`
  (curl-based, 66 checks, self-cleaning — timestamped emails, ends by
  deleting its own users through the privacy cascade) and
  `fs_stage2_tests.sh` (41 checks, historical). I run the current suite
  against the live server before I call a stage done.
