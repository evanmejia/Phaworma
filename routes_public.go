// routes_public.go — my respondent-facing endpoints.
//
// These are the only form-facing routes in the app: short-link redirect,
// the public schema fetch, and submissions. None of them require any auth
// — respondents have no accounts, and that's the whole point of the
// product. What I gate them with instead: (a) the short code is the ONLY
// client-supplied identifier, and (b) the optional per-form access
// password, checked with a timing-safe compare.
package main

import (
	"database/sql"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	validation "github.com/pocketbase/ozzo-validation/v4"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"
)

func registerPublicRoutes(r *router.Router[*core.RequestEvent]) {
	// /f/<code> is the form's real address in Stage 3 — an explicit Go
	// route (not the static index fallback) so I control the 404/410
	// answers for unknown and expired codes, and the SPA shell is served
	// for live ones. My client-side pathname router then reads the code
	// off location.pathname.
	r.GET("/f/{shortCode}", handleFormPage)
	r.GET("/s/{shortCode}", handleShortLink)
	r.GET("/api/v1/public/forms/{shortCode}", handlePublicForm)
	r.POST("/api/v1/forms/{shortCode}/submissions", handleSubmission)
}

// findFormByShortCode is my shortCode -> form lookup. I lowercase the code
// before the query: my generator only emits lowercase, but respondents can
// type the link in any case.
func findFormByShortCode(app core.App, code string) (*core.Record, error) {
	return app.FindFirstRecordByFilter(formsCollectionName,
		"short_code = {:code}", dbx.Params{"code": code})
}

// formExpired answers "is this form dead?" using both signals: the status
// flag (set by my purge cron and boot reconciliation) and the raw expiry
// timestamp — so a form can never outlive its date in the gap between two
// cron ticks.
func formExpired(form *core.Record) bool {
	if form.GetString("status") == "expired" {
		return true
	}
	return form.GetDateTime("expires_at").Time().Before(time.Now())
}

// handleShortLink is my legacy short-link redirector. Every Stage 3 form
// has /f/<code> as its canonical address, so /s/<code> is nothing but a
// 301 for old shares (my Stage 2 SPA links, anything cached, anything
// bookmarked). Unknown and expired codes keep the same honest answers as
// /f/<code> — a 301 into a dead page would just be a slower 404.
func handleShortLink(e *core.RequestEvent) error {
	code := strings.ToLower(e.Request.PathValue("shortCode"))

	form, err := findFormByShortCode(e.App, code)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return e.NotFoundError("unknown short link", nil)
		}
		return err
	}

	if formExpired(form) {
		return e.Error(http.StatusGone,
			"this form has expired and its responses were deleted", nil)
	}

	// Permanent by design: the /f/ address is stable for the form's whole
	// lifespan, so browsers may cache the redirect.
	return e.Redirect(http.StatusMovedPermanently, "/f/"+code)
}

// handleFormPage serves a form's canonical public page. This is a REAL
// Go route (registered before the static wildcard), which is what lets me
// answer 404 for unknown codes and 410 Gone for expired ones — the
// static index fallback could only ever hand back a 200 shell.
//
// For a live form I serve my SPA's index.html directly; the client-side
// pathname router picks the code out of location.pathname and fetches
// the public schema from /api/v1/public/forms/<code>.
func handleFormPage(e *core.RequestEvent) error {
	code := strings.ToLower(e.Request.PathValue("shortCode"))

	form, err := findFormByShortCode(e.App, code)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return e.NotFoundError("form not found", nil)
		}
		return err
	}

	if formExpired(form) {
		return e.Error(http.StatusGone,
			"this form has expired and its responses were deleted", nil)
	}

	// I serve the shell straight from my pb_public dir. FileFS sets the
	// right content type from the extension and streams via
	// http.ServeContent, so caching headers come out sane.
	return e.FileFS(os.DirFS(publicDir()), "index.html")
}

// handlePublicForm returns just enough for my respondent SPA to render the
// questions. The password gate lives HERE (and again on submit): a locked
// form answers a plain 401 until the right password arrives. I used to
// include the form's title in the 401's error data so the frontend could
// label the gate — but the router's safeErrorsData only renders
// validation.Errors sensibly and mangles any other map, so the gate is now
// generically worded. The schema itself is only ever returned after a
// correct password.
func handlePublicForm(e *core.RequestEvent) error {
	code := strings.ToLower(e.Request.PathValue("shortCode"))

	form, err := findFormByShortCode(e.App, code)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return e.NotFoundError("unknown short link", nil)
		}
		return err
	}
	if formExpired(form) {
		return e.Error(http.StatusGone,
			"this form has expired and its responses were deleted", nil)
	}

	schema, err := parseSchema(form)
	if err != nil {
		return e.InternalServerError("this form's schema is unreadable — contact its creator", nil)
	}

	if form.GetString("access_password_hash") != "" {
		sent := e.Request.Header.Get("X-Access-Password")
		if sent == "" || !verifyAccessPassword(
			sent,
			form.GetString("access_password_salt"),
			form.GetString("access_password_hash"),
		) {
			// Wrong and missing passwords get the SAME response on
			// purpose — I don't want to leak which case the caller hit.
			return e.UnauthorizedError("this form is password protected", nil)
		}
		// A legacy (sha256) entry that just verified gets upgraded to
		// argon2id in place — once per form, on the first correct
		// unlock after the upgrade.
		if isLegacyAccessPasswordHash(form.GetString("access_password_hash")) {
			upgradeLegacyAccessPassword(e.App, form, sent)
		}
	}

	return e.JSON(http.StatusOK, map[string]any{
		"title":     form.GetString("title"),
		"shortCode": code,
		"questions": schema.Questions,
	})
}

// submissionBody is the only shape my SPA POSTs. Answers is a flat map of
// questionId -> value; I deliberately don't allow nesting.
type submissionBody struct {
	Answers map[string]any `json:"answers"`
}

// handleSubmission turns a respondent's answers into a row in the form's
// dynamic collection. The enforcement I apply here:
//
//   - the short code is the only client-supplied identifier; the dynamic
//     collection name is derived server-side, so nobody can target
//     another form's table (or a system table),
//   - unknown answer keys are DROPPED before the record is built — I don't
//     want to hand PocketBase's field validation more surface than the
//     schema declares,
//   - the access password is checked with the same timing-safe helper as
//     the public fetch.
func handleSubmission(e *core.RequestEvent) error {
	code := strings.ToLower(e.Request.PathValue("shortCode"))

	form, err := findFormByShortCode(e.App, code)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return e.NotFoundError("unknown short link", nil)
		}
		return err
	}
	if formExpired(form) {
		return e.Error(http.StatusGone,
			"this form has expired and its responses were deleted", nil)
	}

	if form.GetString("access_password_hash") != "" {
		sent := e.Request.Header.Get("X-Access-Password")
		if sent == "" || !verifyAccessPassword(
			sent,
			form.GetString("access_password_salt"),
			form.GetString("access_password_hash"),
		) {
			return e.UnauthorizedError("wrong access password", nil)
		}
		// Same in-place upgrade as the public fetch — a submission with
		// the right legacy password migrates the form's hash.
		if isLegacyAccessPasswordHash(form.GetString("access_password_hash")) {
			upgradeLegacyAccessPassword(e.App, form, sent)
		}
	}

	var body submissionBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("the request body must be JSON with an \"answers\" object", nil)
	}

	dyn, err := e.App.FindCachedCollectionByNameOrId(dynamicCollectionName(form.GetString("id")))
	if err != nil {
		// An active form without a table shouldn't exist (my boot
		// reconciliation rebuilds missing ones), but if it does I answer
		// 503 instead of crashing the submission.
		return e.Error(http.StatusServiceUnavailable,
			"this form's storage is not ready yet — try again in a moment", nil)
	}

	record := core.NewRecord(dyn)
	for name, value := range body.Answers {
		field := dyn.Fields.GetByName(name)
		if field == nil || field.GetSystem() {
			continue // unknown or system key — I drop it silently
		}
		// No answer-length pre-check of my own: my text columns carry a
		// Max of textAnswerMaxChars, and PocketBase enforces Max in RUNES
		// (verified in v0.40.6's field_text.go: "casted to []rune to
		// count multi-byte chars as one") — the same unit as my
		// character promise. An over-long answer therefore dies in
		// e.App.Save below with a per-field validation error, which the
		// block right there turns into the 400 shape my SPA renders.
		record.Set(name, value)
	}

	if err := e.App.Save(record); err != nil {
		// PocketBase field validation failures arrive as ozzo
		// validation.Errors (a field -> error map), NOT as an ApiError —
		// I translate them into a 400 so my SPA can show per-question
		// errors. Everything else is a real server problem: I let it
		// bubble up as a 500.
		var verrs validation.Errors
		if errors.As(err, &verrs) {
			return e.BadRequestError("some answers were invalid", verrs)
		}
		return err
	}

	return e.JSON(http.StatusCreated, map[string]any{"id": record.GetString("id")})
}
