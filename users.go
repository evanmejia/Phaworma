// users.go — my Stage 3 multi-user layer.
//
// This file owns the two collections that make co-creation possible:
//
//   - `users`: regular accounts. Guests sign up here without any friction
//     (public create rule, self-only CRUD rules), and the SPA's auth dialog
//     targets this collection. Superusers stay strictly on the built-in
//     dashboard — my creator API rejects their tokens.
//   - `collaborators`: per-form access grants. One row = one (form, email)
//     pair; the user link fills in the moment the invited address signs up.
//
// It also owns the two record hooks that keep the data model honest:
// pending invites auto-activate on signup, and a user's deletion takes
// their owned forms (and their dynamic tables) with it.
package main

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"
)

const (
	usersCollectionName         = "users"
	collaboratorsCollectionName = "collaborators"

	// selfOnlyRule is the only rule shape my users collection needs beyond
	// the public create: an account can list/view/update/delete itself and
	// nothing else. My /api/v1 endpoints do the real authorization work.
	selfOnlyRule = "id = @request.auth.id"
)

// --- provisioning ----------------------------------------------------------

// ensureUsersCollection creates my `users` auth collection if it's missing
// (fresh installs), and reconciles the installed default one otherwise.
//
// PocketBase's own installer already ships a stock `users` collection (the
// _pb_users_auth_ one) in pb_data, so in practice I mostly hit the
// reconcile path. I keep the create path for anyone who boots me against a
// bare data dir, and the reconcile pass repairs what my SPA depends on:
// a required display name and real created/updated table columns.
func ensureUsersCollection(app core.App) error {
	c, err := app.FindCachedCollectionByNameOrId(usersCollectionName)
	if err == nil {
		return reconcileUsersCollection(app, c)
	}

	c = core.NewAuthCollection(usersCollectionName)

	// NewAuthCollection gives me id/password/tokenKey/email/
	// emailVisibility/verified plus working password-auth defaults (email
	// identity, auto-generated token secret). I add the fields my UI shows.
	name := textField("name")
	name.Required = true
	name.Min = 1
	name.Max = 255

	avatar := core.Fields[core.FieldTypeFile]().(*core.FileField)
	avatar.Name = "avatar"
	avatar.MaxSelect = 1

	created := autodateField("created", true, false)
	updated := autodateField("updated", false, true)

	c.Fields.Add(name, avatar, created, updated)

	// Public signup, self-only everything else. Superusers bypass rules,
	// which is exactly how the dashboard manages accounts.
	c.CreateRule = types.Pointer("")
	self := types.Pointer(selfOnlyRule)
	c.ListRule = self
	c.ViewRule = self
	c.UpdateRule = self
	c.DeleteRule = self

	if err := app.Save(c); err != nil {
		return fmt.Errorf("failed to create the %q collection: %w", usersCollectionName, err)
	}
	app.Logger().Info("created the users collection")
	return nil
}

// reconcileUsersCollection repairs an existing users collection so it
// matches what my SPA and my hooks expect. Idempotent — it only touches
// what is actually off.
func reconcileUsersCollection(app core.App, c *core.Collection) error {
	if !c.IsAuth() {
		// A non-auth collection squatting on my users name would break
		// auth and my hooks alike — I refuse to boot silently.
		return fmt.Errorf("the %q collection exists but is not an auth collection", usersCollectionName)
	}

	changed := false

	// My co-creator list shows the invitee's name, so an empty name is
	// not a state I want to support. If the field is missing I add it;
	// if it's optional I tighten it.
	if nameField, _ := c.Fields.GetByName("name").(*core.TextField); nameField == nil {
		name := textField("name")
		name.Required = true
		name.Min = 1
		name.Max = 255
		c.Fields.Add(name)
		changed = true
	} else if !nameField.Required {
		nameField.Required = true
		nameField.Min = 1
		changed = true
	}

	// The stock installer collection declares created/updated as plain
	// (non-system) autodate fields, and an earlier boot of mine may have
	// left the table without the columns — same disease I fixed on forms.
	needCreated := c.Fields.GetByName("created") == nil || !tableHasColumn(app, usersCollectionName, "created")
	needUpdated := c.Fields.GetByName("updated") == nil || !tableHasColumn(app, usersCollectionName, "updated")
	if needCreated || needUpdated {
		oldJSON, merr := c.MarshalJSON()
		if merr != nil {
			return fmt.Errorf("failed to snapshot the %q collection: %w", usersCollectionName, merr)
		}
		var old core.Collection
		if uerr := old.UnmarshalJSON(oldJSON); uerr != nil {
			return fmt.Errorf("failed to restore %q snapshot: %w", usersCollectionName, uerr)
		}
		if !tableHasColumn(app, usersCollectionName, "created") {
			old.Fields.RemoveByName("created")
		}
		if !tableHasColumn(app, usersCollectionName, "updated") {
			old.Fields.RemoveByName("updated")
		}
		if c.Fields.GetByName("created") == nil {
			c.Fields.Add(autodateField("created", true, false))
		}
		if c.Fields.GetByName("updated") == nil {
			c.Fields.Add(autodateField("updated", false, true))
		}
		if changed {
			// persist the name-field tightening alongside the new
			// field declarations
			if serr := app.Save(c); serr != nil {
				return fmt.Errorf("failed to save %q collection: %w", usersCollectionName, serr)
			}
		}
		if serr := app.SyncRecordTableSchema(c, &old); serr != nil {
			return fmt.Errorf("failed to sync timestamp columns onto %q: %w", usersCollectionName, serr)
		}
		app.Logger().Info("synced timestamp fields onto the users collection")
	} else if changed {
		if serr := app.Save(c); serr != nil {
			return fmt.Errorf("failed to save %q collection: %w", usersCollectionName, serr)
		}
		app.Logger().Info("tightened the users name field")
	}
	return nil
}

// ensureFormsOwnerField upgrades my `forms` collection with the Stage 3
// owner relation. Existing forms keep owner=NULL — my canAccessForm
// treats a missing owner as "nobody has access", which is the safe
// default for data I didn't create in this version.
func ensureFormsOwnerField(app core.App) error {
	forms, err := app.FindCachedCollectionByNameOrId(formsCollectionName)
	if err != nil {
		return fmt.Errorf("the %q collection is missing: %w", formsCollectionName, err)
	}
	users, err := app.FindCachedCollectionByNameOrId(usersCollectionName)
	if err != nil {
		return fmt.Errorf("the %q collection is missing: %w", usersCollectionName, err)
	}

	owner, _ := forms.Fields.GetByName("owner").(*core.RelationField)
	if owner != nil {
		// Already wired — I only verify it points where I need it to.
		if owner.CollectionId == users.Id && owner.MaxSelect == 1 && !owner.CascadeDelete {
			return nil
		}
		owner.CollectionId = users.Id
		owner.MaxSelect = 1
		// CascadeDelete must stay OFF here on purpose: a user deletion
		// cascades through my hook (which drops the dyn_ tables first).
		// A raw cascade would delete owned forms and orphan their
		// respondent tables — a privacy violation.
		owner.CascadeDelete = false
		if serr := app.Save(forms); serr != nil {
			return fmt.Errorf("failed to fix the forms owner field: %w", serr)
		}
		return nil
	}

	// The field is new: snapshot the collection WITHOUT it so the table
	// sync diffs exactly "add the owner column" (app.Save's internal
	// sync would diff the cached object against itself and no-op).
	oldJSON, merr := forms.MarshalJSON()
	if merr != nil {
		return fmt.Errorf("failed to snapshot the %q collection: %w", formsCollectionName, merr)
	}
	var old core.Collection
	if uerr := old.UnmarshalJSON(oldJSON); uerr != nil {
		return fmt.Errorf("failed to restore %q snapshot: %w", formsCollectionName, uerr)
	}

	owner = core.Fields[core.FieldTypeRelation]().(*core.RelationField)
	owner.Name = "owner"
	owner.CollectionId = users.Id
	owner.Required = true
	owner.MaxSelect = 1
	owner.Presentable = true
	forms.Fields.Add(owner)

	if serr := app.Save(forms); serr != nil {
		return fmt.Errorf("failed to add the owner field to %q: %w", formsCollectionName, serr)
	}
	if serr := app.SyncRecordTableSchema(forms, &old); serr != nil {
		return fmt.Errorf("failed to sync the owner column onto %q: %w", formsCollectionName, serr)
	}
	app.Logger().Info("added the owner relation to the forms collection")
	return nil
}

// ensureCollaboratorsCollection creates my `collaborators` collection if
// it's missing. The schema is stable (I introduced it in Stage 3 and
// won't need to migrate it), so an existing collection is accepted as-is.
func ensureCollaboratorsCollection(app core.App) error {
	if _, err := app.FindCachedCollectionByNameOrId(collaboratorsCollectionName); err == nil {
		return nil
	}

	forms, err := app.FindCachedCollectionByNameOrId(formsCollectionName)
	if err != nil {
		return fmt.Errorf("the %q collection is missing: %w", formsCollectionName, err)
	}
	users, err := app.FindCachedCollectionByNameOrId(usersCollectionName)
	if err != nil {
		return fmt.Errorf("the %q collection is missing: %w", usersCollectionName, err)
	}

	formRef := core.Fields[core.FieldTypeRelation]().(*core.RelationField)
	formRef.Name = "form"
	formRef.CollectionId = forms.Id
	formRef.Required = true
	formRef.MaxSelect = 1
	// Deleting a form deletes its invites — cascade runs referenced
	// (form) -> referencing (collaborators row), which is the direction
	// I verified in core/record_model.go.
	formRef.CascadeDelete = true

	userRef := core.Fields[core.FieldTypeRelation]().(*core.RelationField)
	userRef.Name = "user"
	userRef.CollectionId = users.Id
	userRef.MaxSelect = 1
	// Nullable on purpose: a pending invite has no account yet. When the
	// invited user is ever deleted, their leftover rows (co-creator seats
	// on forms they don't own) cascade away instead of dangling.
	userRef.CascadeDelete = true

	// An EmailField on purpose: PocketBase's own email validation
	// (format + my domain rules) is the enforcement layer, so my invite
	// handler only has to pre-check the empty case.
	email := core.Fields[core.FieldTypeEmail]().(*core.EmailField)
	email.Name = "email"
	email.Required = true

	status := selectField("status", []string{"pending", "active"})
	status.Required = true

	created := autodateField("created", true, false)

	c := core.NewBaseCollection(collaboratorsCollectionName)
	c.Fields = core.NewFieldsList(formRef, userRef, email, status, created)
	denyAllRules(c)
	c.AddIndex("idx_collaborators_form_email", true, "form, email", "")

	if err := app.Save(c); err != nil {
		return fmt.Errorf("failed to create the %q collection: %w", collaboratorsCollectionName, err)
	}
	app.Logger().Info("created the collaborators collection")
	return nil
}

// --- record hooks ------------------------------------------------------------

// findUserByEmail is my case-insensitive user lookup for the invite flow.
// PocketBase stores auth emails in the case the person typed them, and my
// invite handler lowercases addresses before storing them on the seat row
// — so a plain FindAuthRecordByEmail can miss an existing account by case
// alone (the users collection's email index is not guaranteed to be
// NOCASE). I query with an explicit COLLATE NOCASE expression, which is
// the same thing FindAuthRecordByEmail does when that index IS present.
// A no-match answer is (nil, nil); I surface real query errors.
func findUserByEmail(app core.App, email string) (*core.Record, error) {
	record := &core.Record{}
	err := app.RecordQuery(usersCollectionName).
		AndWhere(dbx.NewExp("[[email]] = {:email} COLLATE NOCASE", dbx.Params{"email": email})).
		Limit(1).
		One(record)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return record, nil
}

// activatePendingCollaborators flips every pending collaborator row whose
// email matches the freshly created user's email to active and links the
// new account. I call it from OnRecordAfterCreateSuccess so a failed
// signup never leaves a grant pointing at a user that doesn't exist.
//
// A failure here does NOT fail the signup: my canAccessForm carries a
// "pending row with my email" fallback, so the user still gets access to
// the form and the next invite retry repairs the row.
func activatePendingCollaborators(app core.App, user *core.Record) error {
	email := strings.ToLower(user.GetString("email"))
	rows, err := app.FindRecordsByFilter(collaboratorsCollectionName,
		"status = 'pending' && email = {:email}", "", 0, 0,
		dbx.Params{"email": email})
	if err != nil {
		return err
	}

	for _, row := range rows {
		row.Set("user", user.Id)
		row.Set("status", "active")
		if serr := app.Save(row); serr != nil {
			return fmt.Errorf("failed to activate pending collaborator %s: %w", row.GetString("id"), serr)
		}
		app.Logger().Info("activated pending collaborator on signup",
			"user", user.Id, "form", row.GetString("form"), "email", email)
	}
	return nil
}

// deleteUsersForms is my OnRecordDelete handler for the users collection.
// Every form the user owns gets the FULL delete — dynamic table dropped,
// then the form record removed — BEFORE the user row goes away.
//
// This hook is what makes user deletion privacy-safe: without it, the
// relation cascade would only null out the owner references and my dyn_
// tables (full of respondent answers) would stay on disk as orphans.
// Returning an error aborts the user deletion, so I never leave a user
// behind whose forms are half-deleted.
func deleteUsersForms(app core.App, user *core.Record) error {
	forms, err := app.FindRecordsByFilter(formsCollectionName,
		"owner = {:id}", "", 0, 0, dbx.Params{"id": user.Id})
	if err != nil {
		return fmt.Errorf("failed to list owned forms: %w", err)
	}

	for _, form := range forms {
		if serr := deleteFormCompletely(app, form); serr != nil {
			return fmt.Errorf("refusing to delete user with form %s (%s) still to clean up: %w",
				form.GetString("id"), form.GetString("short_code"), serr)
		}
	}
	return nil
}
