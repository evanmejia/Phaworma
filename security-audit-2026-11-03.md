# Security & Bug Audit — 2026-11-03

Scope: full read of every Go file (`main.go`, `schema.go`, `security.go`,
`collections.go`, `users.go`, `purge.go`, `routes_public.go`,
`routes_creator.go`), the SPA (`pb_public/index.html`, `pb_public/js/app.js`,
all 17 `innerHTML` sites), the vendored PocketBase JS SDK, and `go.mod`.
Verified against the pinned dependency APIs (PocketBase v0.40.x router /
collection model, ozzo-validation v4.3.0).

Status: **FIXED** = patched in the same pass as this report · **OPEN** = not
yet addressed.

## Security

### 1. OPEN — Access passwords are brute-forceable in flight (no rate limiting + fast hash)
`security.go` hashes with a single `sha256(salt + ":" + password)` — fast,
and re-checked on every request to `GET /api/v1/public/forms/{code}` and
`POST /api/v1/forms/{code}/submissions` with no throttle anywhere. An
attacker who knows a short code can hammer the password gate at network
speed. If access passwords will ever guard sensitive forms, move to
argon2id/bcrypt; pair with rate limiting (Stage 4).

### 2. OPEN — Email invites are claimable by anyone who knows the address
Signup is open with no email confirmation, and a pending seat activates on
email match. Anyone who sees an invited address (forwarded invite, shared
inbox like `team@company.com`, leaked email) can register it and become a
co-creator — which includes reading the **full datasheet of every respondent
answer**. Mitigations: email verification, one-time invite tokens, or
narrowing co-creator read scope.

### 3. OPEN — `TrustedProxy` unset → one global IP bucket behind NPM
Everything appears as NPM's IP, so PocketBase's built-in auth rate limiter
becomes one global bucket: a login brute-force from anywhere can lock out
the real superuser dashboard login. Setting `TrustedProxies` to NPM's IP is
a precondition for any IP-based defense (rate limiting, login throttling),
not just correct logging.

### 4. FIXED — Boot reconciliation never re-asserted API rules on pre-existing collections
The create paths applied `denyAllRules` / self-only rules, but the reconcile
paths only fixed timestamp fields and the `name` field. A pre-existing
collection with looser (or hand-edited) rules stayed open on the **public
native REST API** — e.g. a public list rule on `users` lets anyone enumerate
every account email with no auth, while my `/api/v1` endpoints do their own
authorization and never notice. (In PocketBase a nil or empty rule means
"public", so nil counted as a deviation too.)

Fix: `assertDenyAllRules()` (collections.go) re-asserts the superuser-only
rule on all five rule slots of `forms`, every `dyn_<formId>` (boot loop),
and `collaborators` on every boot; `assertUsersRules()` (users.go)
re-asserts public-create + self-only on `users`. Saves happen only when
something actually changed.

### 5. OPEN — No per-user form quota + open signup = resource exhaustion
Each form creates a real SQLite table, there is no cap on forms per user,
signup is unthrottled and unverified (#2), and `ensureFormCollections`
walks all forms on every boot. One bot account can create thousands of
tables. A per-user form cap (and a global collection-count check) is cheap
insurance.

### 6. OPEN — No explicit request-body size limit on the public submission endpoint
`e.BindBody(&body)` in `handleSubmission` — verify what cap PocketBase
applies by default; if uncapped, a multi-GB JSON POST to a public endpoint
is a memory spike. Add an explicit `http.MaxBytesReader` (~1 MiB) before
binding to remove the doubt.

### 7. OPEN — Missing security headers (CSP / frame-ancestors / nosniff)
No CSP, `X-Frame-Options`, or `X-Content-Type-Options` anywhere. The SPA is
iframe-able → the auth dialog (email + password entry) is a clickjacking
target. With zero external origins (vendored SDK, no CDN), a strict
`Content-Security-Policy: default-src 'self'; frame-ancestors 'none'` plus
`X-Content-Type-Options: nosniff` is a big, free win — easiest in NPM.

## Bugs & edge cases

### 8. FIXED — Byte-length validation rejected legitimate multibyte text
`len()` counts **bytes**, but every "characters" promise in the product is
in characters:

- `schema.go`: label `> 200`, option `> 100` — a 200-character Japanese
  label (~600 bytes) failed with a confusing error while the frontend
  `maxlength="200"` (character-based) happily allowed it.
- `routes_creator.go`: form title `> 200` in create and update.

Fix: rune-based checks via `utf8.RuneCountInString` in `validateSchema` and
both title handlers, with new constants `formTitleMaxChars = 200` /
`textAnswerMaxChars = 2000` (schema.go) as the single source of truth.

Verified while fixing this (it corrected the audit's own first pass):
PocketBase v0.40.6 enforces `TextField.Max` in **runes, not bytes**
(`core/field_text.go`: "note: casted to []rune to count multi-byte chars as
one") — proven live against the pinned version: a 1020-rune CJK name
(3060 bytes) passes a `Max=1020` field, the 1021st rune fails with
`validation_max_text_constraint`. So the column caps (`title` 200, answers
2000, `users.name` 255) were already consistent with the character promise
all along; the answer cap needs no handler pre-check — an over-long answer
dies in `e.App.Save` with a per-field validation error that
`handleSubmission` already translates into the 400 shape the SPA renders.
(The initial fix had widened the column caps 4× "for bytes" and added a
redundant submission pre-check; both reverted after the live check.)

Adjacent bug fixed as part of #8 (every per-question error — including the
answer cap — renders through this path): **per-question validation errors
displayed `[object Object]`**. PocketBase's `safeErrorsData` serializes each
field of an ozzo `validation.Errors` as a single `{code, message}` object —
not an array — so `msgs.join(" ")` (respondent submit) and `first[0]`
(signup error) both stringified an object. Added a `validationMessage()`
normalizer in app.js (handles object, array-of-object, and string shapes)
and used it in both places; also dropped the stale "Check the highlighted
fields" message (there is no field highlighting in the dialog).

### 9. OPEN — Whitespace-only labels pass validation
`schema.go`: `"   "` satisfies `q.Label == ""` being false and `len ≤ 200`.
A form can ship with a blank-looking question. Trim before checking.

### 10. OPEN — The unique `(form, email)` index isn't reconciled on existing collections
`ensureCollaboratorsCollection` accepts a pre-existing collection *as-is*,
but the unique index is part of the schema. If an older binary created the
collection without it, the race backstop in `handleInviteCollaborator`
(`strings.Contains(serr.Error(), "unique")`) silently stops working and two
racing invites both succeed → duplicate seats. Re-assert the index on the
reconcile path; consider replacing the string-match 409 detection with a
real unique-constraint check. (The rule re-assertion from #4 now covers the
same "existing collection" gap for rules, but not for indexes.)

### 11. OPEN — `viewer, _ := currentCreator(e)` ignores the error, then dereferences
`routes_creator.go` (update, invite, results handlers). Safe today only
because `requireFormAccess`/`requireFormOwner` ran first; if the calls are
ever reordered, `viewer.Id` is a nil-pointer panic → 500. Capture the error
and bail.

### 12. OPEN — Results panel undercounts submissions vs. dashboard
`handleFormResults` reports `submissionCount: len(records)` — capped at the
1000-row fetch — while the dashboard uses real `COUNT(*)`. A form with
5,000 submissions shows "1000 responses" in the results header, and
aggregations cover only the newest 1000. Use `app.CountRecords` for the
header total and label the chart window honestly.

### 13. OPEN — localStorage draft is unvalidated on load
`app.js loadDraft` checks `Array.isArray(d.questions)` but not the shape of
each question. A corrupted or tampered `formsDraft` entry (e.g.
`options: null`) makes `q.options.map(...)` in `buildFormPayload` throw an
uncaught `TypeError` — builder unusable until storage is cleared.
Validate/normalize each question on load. Note: `saveDraft` also persists
the form's **access password in plaintext** to localStorage — consider
keeping `accessPassword` out of the draft.

### 14. OPEN — Co-creators can change or remove the access password
`handleUpdateForm` is open to owners AND co-creators (documented as
intentional). Sanity-check: a co-creator can set a new password only they
know (locking the owner out of the respondent flow) or extend the form's
lifespan. If password management should be owner-only, gate those fields on
the owner check.

### 15. OPEN — User deletion can hold the request open a long time
`deleteUsersForms` drops every owned form's table *before* the user row
goes, inside the pre-delete hook. A user with many large forms gets a
multi-second blocking delete → HTTP client timeouts mid-cascade, and the
"returning an error aborts the deletion" guarantee then leaves a
half-deleted state the user must retry.

### 16. RESOLVED (verified, no code change) — Non-string answer coercion
`handleSubmission` does `record.Set(name, value)` with raw `any` values.
Verified live against the pinned v0.40.6: `{"q1": 42}` (number) is
**coerced to the string "42" and stored** (201) — PocketBase standard
`cast.ToString` behavior, fine for a text answer; `{"q1": {...}}` (object)
and `{"q1": [...]}` (array) are rejected with a per-field 400
(`validation_required`, "Cannot be blank.") — the message is misleading for
a non-blank object, but the status and shape are right. No 500 binding
failure in any case; nothing to fix.

### 21. FIXED — Guest publish was broken: v0.40.6 auth-create returns no token, and the signup handler trusted one
Discovered while functionally verifying fix #8. The auth dialog's CREATE tab
did:
`const result = await pb.collection("users").create({...}); pb.authStore.save(result.token, result.record);`
with a code comment asserting "the response is {token, record}". In the
pinned PocketBase v0.40.6, `POST /api/collections/users/records` returns
**the record only — no `token` field** (verified live against the running
binary). The SDK's generic `create()` also never touches the auth store
(only the `auth*` methods call `authResponse`, which saves it). So
`result.token` was `undefined`, and `authStore.save(undefined, record)`
stored an **empty token with a non-null record** — and `authStore.isValid`
is `!isTokenExpired(token)`, which is `true` for an empty token (no payload,
no expiry). Net effect: after every fresh signup the SPA believed the user
was signed in, but every `/api/v1` call went out without an Authorization
header → 401. The primary onboarding path (guest signs up → publishes first
form) was broken; only the pre-existing SIGN IN tab worked.

Fix: the CREATE tab now keeps `create()`, then calls
`await pb.collection("users").authWithPassword(email, password)` — the same
call the sign-in tab already uses — which saves a real token to the store
itself; the bogus `authStore.save(result.token, ...)` line is gone.
Verified live end-to-end: create (200, no token in body) →
auth-with-password (200 + token) → `POST /api/v1/forms` with that token
(201, form created).

## Minor

17. `app.js` — `isSignedIn() ? "Publish form" : "Publish form"` is a dead ternary.
18. `state.isOwner` is written in three places and never read — the UI uses `data.form.isOwner` from the API. Dead state.
19. `expiryText` uses `Math.ceil`, so a form expiring in 3 hours reads "expires in 1 days".
20. The signup error line shows "Wrong email or password." for network errors too.

## What's solid

The high-risk surfaces are well handled:

- **XSS posture is clean.** Every `innerHTML` site escapes all
  user-controlled strings (titles, labels, options, respondent answers,
  emails, owner names) via `escapeHtml`/`escapeAttr`; dynamic text that
  doesn't need markup uses `textContent`; question IDs are canonicalized
  server-side before they become column names or DOM ids; `CSS.escape` on
  query selectors.
- **Auth model is correct.** Superuser tokens get 403 via an explicit
  `IsSuperuser()` check (a nil check alone would let the admin use the
  creator API); the collection name is verified; ownership is taken from
  the authenticated session, never the request body; inaccessible forms
  answer 404, never 403.
- **SQL is safe.** Every query uses parameterized `dbx.Params`; filter
  strings are constants; dynamic collection names derive from server-side
  form ids only.
- **Password handling is timing-safe** (`hmac.Equal`), and missing vs wrong
  access passwords get identical 401s.
- **The purge / boot-reconciliation / privacy-cascade logic is careful:**
  idempotent provisioning, the 5-minute skew buffer, the orphan-table sweep
  pass, and the user-delete cascade that drops `dyn_` tables before the
  user row goes (the relation cascade alone would orphan respondent data).
