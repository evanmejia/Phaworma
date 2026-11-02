// routes_creator.go — my creator-facing API.
//
// Stage 3 auth model: regular users (the `users` collection) are the
// creators. Every route in this file sits behind requireUser, which
// accepts ONLY users-collection tokens — superusers are pointed at the
// built-in dashboard instead. Per-form authorization (owner, active
// co-creator, or pending invite matching my email) is checked in each
// form-scoped handler, and unauthorized forms answer 404 so the API
// never leaks that a form exists to someone who isn't allowed to see
// it.
package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	validation "github.com/pocketbase/ozzo-validation/v4"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"
)

func registerCreatorRoutes(r *router.Router[*core.RequestEvent]) {
	// BindFunc attaches requireUser as GROUP middleware: PocketBase's
	// router composes it into the hook chain of every route registered
	// under this group, and it short-circuits with 401/403 when the
	// request isn't a regular-user session.
	creator := r.Group("/api/v1").BindFunc(requireUser)

	creator.POST("/forms", handleCreateForm)
	creator.GET("/forms", handleListForms)
	creator.GET("/forms/{id}", handleGetForm)
	creator.PUT("/forms/{id}", handleUpdateForm)
	creator.DELETE("/forms/{id}", handleDeleteForm)
	creator.GET("/forms/{id}/submissions", handleListSubmissions)
	creator.GET("/forms/{id}/results", handleFormResults)
	creator.GET("/forms/{id}/collaborators", handleListCollaborators)
	creator.POST("/forms/{id}/collaborators", handleInviteCollaborator)
	creator.DELETE("/forms/{id}/collaborators/{collabId}", handleRemoveCollaborator)
}

// requireUser is my Stage 3 auth gate. It accepts exactly one kind of
// session: a token authenticated against my `users` collection.
//
// The two rejections matter in different ways:
//   - nil Auth is a plain "sign in" answer (401),
//   - a superuser token IS a valid auth record (RequestInfo.Auth is the
//     _superusers record), so without the IsSuperuser check platform
//     admins could silently use the creator API. My role separation is
//     structural: superusers administer the platform from /_/, regular
//     users build forms here.
func requireUser(e *core.RequestEvent) error {
	info, err := e.RequestInfo()
	if err != nil || info.Auth == nil {
		return e.UnauthorizedError("creator sign-in required", nil)
	}
	if info.Auth.IsSuperuser() {
		return e.Error(http.StatusForbidden,
			"superusers use the built-in admin dashboard", nil)
	}
	if info.Auth.Collection().Name != usersCollectionName {
		return e.UnauthorizedError("creator sign-in required", nil)
	}
	return e.Next()
}

// currentCreator returns the authenticated user's record. The group
// middleware already guarantees it's a users record, but I keep the
// defensive error path — handlers should never dereference a nil record.
func currentCreator(e *core.RequestEvent) (*core.Record, error) {
	info, err := e.RequestInfo()
	if err != nil || info.Auth == nil {
		return nil, err
	}
	return info.Auth, nil
}

// canAccessForm answers "may this user see or manage this form?" The
// three accepted shapes, in priority order:
//
//  1. the form owner,
//  2. an active collaborator row linked to my user id,
//  3. a pending collaborator row whose email matches mine — my fallback
//     for the (rare) case where my signup hook didn't manage to flip the
//     row to active.
func canAccessForm(app core.App, viewer *core.Record, form *core.Record) bool {
	if form.GetString("owner") == viewer.Id {
		return true
	}

	email := strings.ToLower(viewer.GetString("email"))
	row, err := app.FindFirstRecordByFilter(collaboratorsCollectionName,
		"form = {:formId} && (user = {:userId} || (status = 'pending' && email = {:email}))",
		dbx.Params{"formId": form.Id, "userId": viewer.Id, "email": email})
	if err != nil {
		return false
	}
	return row != nil
}

// requireFormAccess is the per-form gate my form-scoped handlers call
// after loading the form. It returns the viewer for handlers that need
// the record (owner checks, scoping), and answers 404 — not 403 — when
// the user has no access: "I don't know this form" is the only thing my
// API is allowed to say to an outsider.
func requireFormAccess(e *core.RequestEvent, form *core.Record) (*core.Record, error) {
	viewer, err := currentCreator(e)
	if err != nil {
		return nil, e.UnauthorizedError("creator sign-in required", nil)
	}
	if !canAccessForm(e.App, viewer, form) {
		return nil, e.NotFoundError("form not found", nil)
	}
	return viewer, nil
}

// requireFormOwner is the tighter gate for the two actions only the
// owner may take: deleting the form and managing co-creators. A user
// who CAN see the form but isn't the owner gets an honest 403 here
// (no existence is being leaked — they already know the form).
func requireFormOwner(e *core.RequestEvent, form *core.Record) (*core.Record, error) {
	viewer, err := requireFormAccess(e, form)
	if err != nil {
		return nil, err
	}
	if form.GetString("owner") != viewer.Id {
		return nil, e.ForbiddenError("only the form owner can do this", nil)
	}
	return viewer, nil
}

// findFormById is my form lookup by record id.
func findFormById(app core.App, id string) (*core.Record, error) {
	return app.FindRecordById(formsCollectionName, id)
}

// formsRecord builds a new record on my `forms` collection. I look the
// collection up per request instead of caching a pointer, because
// PocketBase reloads its collection cache after every collection change —
// a fresh lookup costs nothing and can never go stale.
func formsRecord(e *core.RequestEvent) (*core.Record, error) {
	c, err := e.App.FindCachedCollectionByNameOrId(formsCollectionName)
	if err != nil {
		return nil, err
	}
	return core.NewRecord(c), nil
}

// ownerInfo resolves a form's owner record into the small name/email
// pair my views need. A missing or deleted owner reads as empty — my
// views degrade gracefully instead of failing the whole response.
func ownerInfo(app core.App, ownerId string) (name, email string) {
	if ownerId == "" {
		return "", ""
	}
	owner, err := app.FindRecordById(usersCollectionName, ownerId)
	if err != nil {
		return "", ""
	}
	return owner.GetString("name"), owner.GetString("email")
}

// formView is the ONLY shape I ever serialize a form record in. I build
// it by hand (not via the record's own JSON) so the access-password hash
// and salt can NEVER leak into an API response — accessPasswordSet tells
// my SPA whether a password exists without revealing anything about it.
// viewerId is the requesting user ("" for no viewer, which never happens
// behind requireUser) — it drives the isOwner flag my SPA uses to show
// or hide owner-only controls.
func formView(app core.App, form *core.Record, submissionCount int64, viewerId string) map[string]any {
	ownerId := form.GetString("owner")
	ownerName, ownerEmail := ownerInfo(app, ownerId)
	return map[string]any{
		"id":                form.GetString("id"),
		"title":             form.GetString("title"),
		"shortCode":         form.GetString("short_code"),
		"accessPasswordSet": form.GetString("access_password_hash") != "",
		"lifespanDays":      form.GetInt("lifespan_days"),
		"expiresAt":         form.GetDateTime("expires_at").String(),
		"status":            form.GetString("status"),
		"questionCount":     schemaQuestionCount(form),
		"submissionCount":   submissionCount,
		"createdAt":         form.GetDateTime("created").String(),
		"ownerId":           ownerId,
		"ownerName":         ownerName,
		"ownerEmail":        ownerEmail,
		"isOwner":           viewerId != "" && viewerId == ownerId,
	}
}

// submissionCount is a per-form COUNT for my list views. A missing table
// (form created seconds ago, or already purged) reads as zero.
func submissionCount(app core.App, form *core.Record) int64 {
	n, err := app.CountRecords(dynamicCollectionName(form.GetString("id")))
	if err != nil {
		return 0
	}
	return n
}

// --- create --------------------------------------------------------------

// createFormRequest is what my builder POSTs. accessPassword is optional;
// an empty string means "no password".
type createFormRequest struct {
	Title          string     `json:"title"`
	LifespanDays   int        `json:"lifespanDays"`
	AccessPassword string     `json:"accessPassword"`
	Schema         formSchema `json:"schema"`
}

func handleCreateForm(e *core.RequestEvent) error {
	viewer, err := currentCreator(e)
	if err != nil {
		return e.UnauthorizedError("creator sign-in required", nil)
	}

	var req createFormRequest
	if err := e.BindBody(&req); err != nil {
		return e.BadRequestError("invalid request body", nil)
	}

	req.Title = strings.TrimSpace(req.Title)
	if req.Title == "" || len(req.Title) > 200 {
		return e.BadRequestError("title must be 1-200 characters", nil)
	}
	if req.LifespanDays < 1 || req.LifespanDays > 30 {
		return e.BadRequestError("lifespan must be between 1 and 30 days", nil)
	}
	if err := validateSchema(req.Schema); err != nil {
		return e.BadRequestError(err.Error(), nil)
	}
	canonicalizeQuestionIds(&req.Schema)

	code, err := generateShortCode(e.App)
	if err != nil {
		return e.InternalServerError("could not allocate a short link", nil)
	}

	form, err := formsRecord(e)
	if err != nil {
		// The forms collection is provisioned on every boot, so this
		// failing means something is seriously wrong.
		return e.InternalServerError("the forms collection is missing", nil)
	}

	// Ownership is set HERE, from the authenticated session — never from
	// the request body. A client can't claim someone else's forms.
	form.Set("owner", viewer.Id)
	form.Set("title", req.Title)
	form.Set("short_code", code)
	form.Set("lifespan_days", req.LifespanDays)
	form.Set("expires_at", time.Now().AddDate(0, 0, req.LifespanDays))
	form.Set("status", "active")

	// I marshal the schema to raw JSON myself instead of handing the
	// struct to record.Set — deterministic bytes beat relying on Prepare
	// value behavior for a field I own.
	schemaJSON, err := json.Marshal(req.Schema)
	if err != nil {
		return e.InternalServerError("failed to serialize the form schema", nil)
	}
	form.Set("schema", json.RawMessage(schemaJSON))

	if req.AccessPassword != "" {
		salt, hash, perr := newAccessPasswordCredentials(req.AccessPassword)
		if perr != nil {
			return e.InternalServerError("failed to store the access password", nil)
		}
		form.Set("access_password_hash", hash)
		form.Set("access_password_salt", salt)
	}

	if err := e.App.Save(form); err != nil {
		// My forms-collection field validation can still bite even after
		// my pre-checks — I translate it the same way as submissions.
		var verrs validation.Errors
		if errors.As(err, &verrs) {
			return e.BadRequestError("the form details are invalid", verrs)
		}
		return err
	}

	// NOW I build the form's dynamic collection. If this fails after the
	// form record is persisted, I roll the record back — a form without
	// its table is a broken state I refuse to leave behind.
	dyn, err := buildDynamicCollection(form)
	if err != nil {
		_ = e.App.Delete(form)
		return e.InternalServerError("failed to build this form's storage", nil)
	}
	if err := e.App.Save(dyn); err != nil {
		_ = e.App.Delete(form)
		return e.InternalServerError("failed to create this form's storage — nothing was saved", nil)
	}

	return e.JSON(http.StatusCreated, formView(e.App, form, 0, viewer.Id))
}

// canonicalizeQuestionIds rewrites ids as q1..qN in schema order. My
// builder already generates them that way; this is belt-and-braces so a
// hand-crafted payload can't smuggle in an id that clashes with a system
// column or a question I don't own.
func canonicalizeQuestionIds(s *formSchema) {
	for i := range s.Questions {
		s.Questions[i].ID = fmt.Sprintf("q%d", i+1)
	}
}

// --- list / read ---------------------------------------------------------

// handleListForms returns exactly the forms the viewer may see: the ones
// they own plus the ones where they hold a co-creator seat (active or a
// pending row matching their email — same three shapes as canAccessForm).
//
// PocketBase v0.40's filter language has no subquery support, so I run
// the two halves separately and merge in Go — my forms lists are small
// enough that two indexed queries beat any cleverness.
func handleListForms(e *core.RequestEvent) error {
	viewer, err := currentCreator(e)
	if err != nil {
		return e.UnauthorizedError("creator sign-in required", nil)
	}

	owned, err := e.App.FindRecordsByFilter(formsCollectionName,
		"owner = {:uid}", "-created", 0, 0, dbx.Params{"uid": viewer.Id})
	if err != nil {
		e.App.Logger().Error("failed to list owned forms", "error", err)
		return e.InternalServerError("failed to list forms", nil)
	}

	email := strings.ToLower(viewer.GetString("email"))
	seats, err := e.App.FindRecordsByFilter(collaboratorsCollectionName,
		"user = {:uid} || (status = 'pending' && email = {:email})", "", 0, 0,
		dbx.Params{"uid": viewer.Id, "email": email})
	if err != nil {
		e.App.Logger().Error("failed to list collaborator seats", "error", err)
		return e.InternalServerError("failed to list forms", nil)
	}

	// Collect the distinct form ids I co-create on (a seat list can hold
	// the same form twice if I ever add multi-select seats), fetch those
	// forms, and merge with my owned ones. `seen` tracks what's already
	// in my output; `seatSeen` only de-dups the id list itself.
	seen := make(map[string]bool, len(owned)+len(seats))
	out := make([]map[string]any, 0, len(owned)+len(seats))

	for _, form := range owned {
		if seen[form.Id] {
			continue
		}
		seen[form.Id] = true
		out = append(out, formView(e.App, form, submissionCount(e.App, form), viewer.Id))
	}

	seatSeen := make(map[string]bool, len(seats))
	seatIds := make([]string, 0, len(seats))
	for _, seat := range seats {
		fid := seat.GetString("form")
		if fid == "" || seatSeen[fid] || seen[fid] {
			continue
		}
		seatSeen[fid] = true
		seatIds = append(seatIds, fid)
	}
	if len(seatIds) > 0 {
		shared, err := e.App.FindRecordsByIds(formsCollectionName, seatIds)
		if err != nil {
			e.App.Logger().Error("failed to list shared forms", "error", err)
			return e.InternalServerError("failed to list forms", nil)
		}
		for _, form := range shared {
			if seen[form.Id] {
				continue
			}
			seen[form.Id] = true
			out = append(out, formView(e.App, form, submissionCount(e.App, form), viewer.Id))
		}
	}

	// I re-sort the merged list by created (newest first) because the two
	// halves arrived in different query orders.
	sort.Slice(out, func(i, j int) bool {
		// formView already flattened createdAt — I compare the ISO strings,
		// which sort chronologically (PocketBase emits RFC3339).
		return out[i]["createdAt"].(string) > out[j]["createdAt"].(string)
	})

	return e.JSON(http.StatusOK, out)
}

// handleGetForm returns the form metadata AND its schema — the builder's
// edit mode is the main consumer of this endpoint. Access is the standard
// per-form gate, so co-creators get the same payload (their SPA hides the
// schema editor, not the data).
func handleGetForm(e *core.RequestEvent) error {
	form, err := findFormById(e.App, e.Request.PathValue("id"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return e.NotFoundError("form not found", nil)
		}
		return err
	}

	viewer, err := requireFormAccess(e, form)
	if err != nil {
		return err
	}

	schema, err := parseSchema(form)
	if err != nil {
		return e.InternalServerError("this form's schema is unreadable", nil)
	}

	return e.JSON(http.StatusOK, map[string]any{
		"form":   formView(e.App, form, submissionCount(e.App, form), viewer.Id),
		"schema": schema,
	})
}

// --- update --------------------------------------------------------------

// updateFormRequest uses pointers so I can tell "not sent" from "sent as
// zero value" — title "" must mean invalid, not "clear the title".
type updateFormRequest struct {
	Title          *string `json:"title"`
	LifespanDays   *int    `json:"lifespanDays"`
	AccessPassword *string `json:"accessPassword"`
	RemovePassword bool    `json:"removePassword"`
}

// handleUpdateForm lets BOTH owners and co-creators edit the metadata
// (title, lifespan, access password). The schema is intentionally NOT
// part of this request — question edits stay owner-only via the builder,
// which my SPA enforces by hiding the editor from co-creators.
func handleUpdateForm(e *core.RequestEvent) error {
	form, err := findFormById(e.App, e.Request.PathValue("id"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return e.NotFoundError("form not found", nil)
		}
		return err
	}

	if _, err := requireFormAccess(e, form); err != nil {
		return err
	}

	if formExpired(form) {
		// Editing an expired form would mean rebuilding its dropped
		// table — Stage 4 territory. I refuse clearly instead.
		return e.Error(http.StatusConflict,
			"this form has expired and can no longer be edited", nil)
	}

	var req updateFormRequest
	if err := e.BindBody(&req); err != nil {
		return e.BadRequestError("invalid request body", nil)
	}

	if req.Title != nil {
		title := strings.TrimSpace(*req.Title)
		if title == "" || len(title) > 200 {
			return e.BadRequestError("title must be 1-200 characters", nil)
		}
		form.Set("title", title)
	}

	if req.LifespanDays != nil {
		if *req.LifespanDays < 1 || *req.LifespanDays > 30 {
			return e.BadRequestError("lifespan must be between 1 and 30 days", nil)
		}
		// Re-anchoring the lifespan restarts the clock from now — that's
		// the "extend my form" use case.
		form.Set("lifespan_days", *req.LifespanDays)
		form.Set("expires_at", time.Now().AddDate(0, 0, *req.LifespanDays))
	}

	if req.AccessPassword != nil && *req.AccessPassword != "" {
		salt, hash, perr := newAccessPasswordCredentials(*req.AccessPassword)
		if perr != nil {
			return e.InternalServerError("failed to store the access password", nil)
		}
		form.Set("access_password_hash", hash)
		form.Set("access_password_salt", salt)
	}

	if req.RemovePassword {
		form.Set("access_password_hash", "")
		form.Set("access_password_salt", "")
	}

	if err := e.App.Save(form); err != nil {
		var verrs validation.Errors
		if errors.As(err, &verrs) {
			return e.BadRequestError("the form details are invalid", verrs)
		}
		return err
	}

	viewer, _ := currentCreator(e)
	return e.JSON(http.StatusOK, formView(e.App, form, submissionCount(e.App, form), viewer.Id))
}

// --- delete --------------------------------------------------------------

// handleDeleteForm is owner-only. A co-creator who tries it gets 403
// (they know the form exists — no 404 theater), and an outsider gets 404.
func handleDeleteForm(e *core.RequestEvent) error {
	form, err := findFormById(e.App, e.Request.PathValue("id"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return e.NotFoundError("form not found", nil)
		}
		return err
	}

	if _, err := requireFormOwner(e, form); err != nil {
		return err
	}

	// deleteFormCompletely drops the dynamic table AND the form record —
	// after this, /f/<code> answers 404 (the code exists nowhere). The
	// form's collaborator rows cascade away with it.
	if err := deleteFormCompletely(e.App, form); err != nil {
		return e.InternalServerError("failed to delete this form — try again", nil)
	}
	return e.NoContent(http.StatusNoContent)
}

// --- collaborators -------------------------------------------------------

// collaboratorView is the shape my share screen serializes a collaborator
// row as. name is populated for active rows (the linked account's display
// name) and stays empty for pending ones — there is no account yet.
func collaboratorView(app core.App, row *core.Record) map[string]any {
	name := ""
	if userId := row.GetString("user"); userId != "" {
		if user, err := app.FindRecordById(usersCollectionName, userId); err == nil {
			name = user.GetString("name")
		}
	}
	return map[string]any{
		"id":      row.GetString("id"),
		"email":   row.GetString("email"),
		"status":  row.GetString("status"),
		"name":    name,
		"created": row.GetDateTime("created").String(),
	}
}

func handleListCollaborators(e *core.RequestEvent) error {
	form, err := findFormById(e.App, e.Request.PathValue("id"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return e.NotFoundError("form not found", nil)
		}
		return err
	}
	if _, err := requireFormAccess(e, form); err != nil {
		return err
	}

	rows, err := e.App.FindRecordsByFilter(collaboratorsCollectionName,
		"form = {:fid}", "created", 0, 0, dbx.Params{"fid": form.Id})
	if err != nil {
		e.App.Logger().Error("failed to list collaborators", "error", err)
		return e.InternalServerError("failed to list collaborators", nil)
	}

	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, collaboratorView(e.App, row))
	}
	return e.JSON(http.StatusOK, out)
}

type inviteCollaboratorRequest struct {
	Email string `json:"email"`
}

// handleInviteCollaborator is the owner's "add co-creator" action.
// Invites are addressed to an EMAIL, not an account:
//
//   - if a user with that email already exists, the row is created
//     ACTIVE with the user linked — they see the form on their next load;
//   - otherwise the row is PENDING and my signup hook (or my
//     canAccessForm fallback) activates it the moment that address
//     registers.
//
// The unique (form, email) index makes re-inviting a no-op answer: an
// existing pending row self-heals into active if the account now exists,
// and anything else answers 409.
func handleInviteCollaborator(e *core.RequestEvent) error {
	form, err := findFormById(e.App, e.Request.PathValue("id"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return e.NotFoundError("form not found", nil)
		}
		return err
	}
	if _, err := requireFormOwner(e, form); err != nil {
		return err
	}

	var req inviteCollaboratorRequest
	if err := e.BindBody(&req); err != nil {
		return e.BadRequestError("invalid request body", nil)
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if email == "" || !strings.Contains(email, "@") {
		// The EmailField on the collaborators row is my real format
		// enforcer at save time — this only stops the obviously empty
		// case from wasting a round trip.
		return e.BadRequestError("please enter a valid email address", nil)
	}

	// Inviting yourself is a no-op worth refusing clearly — you already
	// have every right this form can grant.
	viewer, _ := currentCreator(e)
	if email == strings.ToLower(viewer.GetString("email")) {
		return e.BadRequestError("you are the owner of this form", nil)
	}

	existing, err := e.App.FindFirstRecordByFilter(collaboratorsCollectionName,
		"form = {:fid} && email = {:email}",
		dbx.Params{"fid": form.Id, "email": email})
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	if existing != nil {
		if existing.GetString("status") == "pending" {
			// The account may have appeared since the invite — my signup
			// hook usually flips this, so this is the repair path for a
			// hook that missed (and the idempotency path for re-invites).
			if user, uerr := findUserByEmail(e.App, email); uerr == nil && user != nil {
				existing.Set("user", user.Id)
				existing.Set("status", "active")
				if serr := e.App.Save(existing); serr != nil {
					return e.InternalServerError("failed to activate the co-creator seat", nil)
				}
				return e.JSON(http.StatusOK, collaboratorView(e.App, existing))
			}
		}
		return e.Error(http.StatusConflict, "this email already has a seat on this form", nil)
	}

	// No seat yet — decide active vs pending by whether the account
	// already exists. findUserByEmail is case-insensitive on purpose: my
	// seat rows store the lowercased address, but the account may have
	// typed its email in any case.
	var user *core.Record
	if u, uerr := findUserByEmail(e.App, email); uerr != nil {
		return uerr
	} else {
		user = u
	}

	collabCollection, err := e.App.FindCachedCollectionByNameOrId(collaboratorsCollectionName)
	if err != nil {
		return e.InternalServerError("the collaborators collection is missing", nil)
	}
	row := core.NewRecord(collabCollection)
	row.Set("form", form.Id)
	row.Set("email", email)
	if user != nil {
		row.Set("user", user.Id)
		row.Set("status", "active")
	} else {
		row.Set("status", "pending")
	}

	if serr := e.App.Save(row); serr != nil {
		// A malformed email that slipped past my pre-check arrives here
		// as field validation errors — I answer 400 for those.
		var verrs validation.Errors
		if errors.As(serr, &verrs) {
			return e.BadRequestError("please enter a valid email address", verrs)
		}
		// The unique (form, email) index is my backstop against two
		// invites racing — a loser answers 409 like the check above.
		if strings.Contains(serr.Error(), "unique") {
			return e.Error(http.StatusConflict, "this email already has a seat on this form", nil)
		}
		e.App.Logger().Error("failed to invite collaborator", "error", serr)
		return e.InternalServerError("failed to invite this co-creator", nil)
	}

	return e.JSON(http.StatusCreated, collaboratorView(e.App, row))
}

// handleRemoveCollaborator is the owner's "revoke seat" action. Removing
// an active co-creator just deletes their row — the form, its data, and
// everyone else's access are untouched.
func handleRemoveCollaborator(e *core.RequestEvent) error {
	form, err := findFormById(e.App, e.Request.PathValue("id"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return e.NotFoundError("form not found", nil)
		}
		return err
	}
	if _, err := requireFormOwner(e, form); err != nil {
		return err
	}

	collabId := e.Request.PathValue("collabId")
	row, err := e.App.FindFirstRecordByFilter(collaboratorsCollectionName,
		"id = {:cid} && form = {:fid}",
		dbx.Params{"cid": collabId, "fid": form.Id})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return e.NotFoundError("co-creator seat not found", nil)
		}
		return err
	}

	if err := e.App.Delete(row); err != nil {
		e.App.Logger().Error("failed to remove collaborator", "error", err)
		return e.InternalServerError("failed to remove this co-creator", nil)
	}
	return e.NoContent(http.StatusNoContent)
}

// --- submissions & results -----------------------------------------------

// handleListSubmissions returns the raw datasheet (max 1000 rows, newest
// first). I pick only the question fields + id + created — system fields
// and the collection internals never reach my frontend.
func handleListSubmissions(e *core.RequestEvent) error {
	form, err := findFormById(e.App, e.Request.PathValue("id"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return e.NotFoundError("form not found", nil)
		}
		return err
	}
	if _, err := requireFormAccess(e, form); err != nil {
		return err
	}

	schema, err := parseSchema(form)
	if err != nil {
		return e.InternalServerError("this form's schema is unreadable", nil)
	}

	dynName := dynamicCollectionName(form.GetString("id"))
	records, err := e.App.FindRecordsByFilter(dynName, "", "-created", 1000, 0)
	if err != nil {
		// A missing table means "no submissions yet" (freshly created
		// form), not a server error.
		if !e.App.HasTable(dynName) {
			records = nil
		} else {
			e.App.Logger().Error("failed to read submissions", "form", form.GetString("id"), "error", err)
			return e.InternalServerError("failed to read submissions", nil)
		}
	}

	rows := make([]map[string]any, 0, len(records))
	for _, rec := range records {
		row := map[string]any{
			"id":      rec.GetString("id"),
			"created": rec.GetDateTime("created").String(),
		}
		for _, q := range schema.Questions {
			row[q.ID] = rec.GetString(q.ID)
		}
		rows = append(rows, row)
	}
	return e.JSON(http.StatusOK, rows)
}

// questionResult is one question's aggregation for my results panel.
// Counts is zero-filled for every declared option so my charts never have
// to special-case missing buckets.
type questionResult struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	Label      string         `json:"label"`
	Options    []string       `json:"options,omitempty"`
	Counts     map[string]int `json:"counts,omitempty"` // choice/dropdown only
	Answered   int            `json:"answered"`
	Unanswered int            `json:"unanswered"`
}

func handleFormResults(e *core.RequestEvent) error {
	form, err := findFormById(e.App, e.Request.PathValue("id"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return e.NotFoundError("form not found", nil)
		}
		return err
	}
	if _, err := requireFormAccess(e, form); err != nil {
		return err
	}

	schema, err := parseSchema(form)
	if err != nil {
		return e.InternalServerError("this form's schema is unreadable", nil)
	}

	dynName := dynamicCollectionName(form.GetString("id"))
	records, err := e.App.FindRecordsByFilter(dynName, "", "-created", 1000, 0)
	if err != nil {
		if !e.App.HasTable(dynName) {
			records = nil
		} else {
			e.App.Logger().Error("failed to read submissions", "form", form.GetString("id"), "error", err)
			return e.InternalServerError("failed to read submissions", nil)
		}
	}

	results := make([]questionResult, 0, len(schema.Questions))
	for _, q := range schema.Questions {
		r := questionResult{ID: q.ID, Type: q.Type, Label: q.Label}
		if q.Type == questionTypeChoice || q.Type == questionTypeDropdown {
			r.Options = q.Options
			r.Counts = make(map[string]int, len(q.Options))
			for _, opt := range q.Options {
				r.Counts[opt] = 0
			}
		}
		results = append(results, r)
	}

	for _, rec := range records {
		for i, q := range schema.Questions {
			v := rec.GetString(q.ID)
			if v == "" {
				results[i].Unanswered++
				continue
			}
			results[i].Answered++
			if results[i].Counts != nil {
				// PocketBase already rejected values outside the option
				// list, so a miss here can only mean a stale option — I
				// skip it rather than invent a bucket.
				if _, ok := results[i].Counts[v]; ok {
					results[i].Counts[v]++
				}
			}
		}
	}

	viewer, _ := currentCreator(e)
	return e.JSON(http.StatusOK, map[string]any{
		"form":    formView(e.App, form, int64(len(records)), viewer.Id),
		"results": results,
	})
}
