// collections.go — my collection provisioning layer.
//
// Every form I create owns exactly one isolated "dynamic" PocketBase
// collection named dyn_<formId>. This file is the ONLY place that knows how
// to build, verify, and destroy those collections — my OnBootstrap hook, my
// creator API, and my purge cron all funnel through here, so the "one form
// = one table" invariant has a single code path.
package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"
)

const (
	formsCollectionName = "forms"
	dynPrefix           = "dyn_"
)

// superuserOnlyRule is applied to every collection I create. In PocketBase
// an EMPTY rule means "public", so I must set an explicit deny rule on
// everything: my respondents never talk to the native REST API at all —
// they go through my /api/v1 endpoints, where I control access. Superusers
// bypass rules entirely, so this string only has to be false for everyone
// else. (Stage 3 replaces it with a co-creator relation check.)
const superuserOnlyRule = "@request.auth.role = 'admin'"

func denyAllRules(c *core.Collection) {
	rule := types.Pointer(superuserOnlyRule)
	c.ListRule = rule
	c.ViewRule = rule
	c.CreateRule = rule
	c.UpdateRule = rule
	c.DeleteRule = rule
}

// dynamicCollectionName is the table backing a form's submissions.
func dynamicCollectionName(formId string) string {
	return dynPrefix + formId
}

// --- field builders (tiny helpers so I don't repeat the factory dance) ---

func textField(name string) *core.TextField {
	f := core.Fields[core.FieldTypeText]().(*core.TextField)
	f.Name = name
	return f
}

func selectField(name string, values []string) *core.SelectField {
	f := core.Fields[core.FieldTypeSelect]().(*core.SelectField)
	f.Name = name
	f.Values = values
	return f
}

func jsonField(name string) *core.JSONField {
	f := core.Fields[core.FieldTypeJSON]().(*core.JSONField)
	f.Name = name
	return f
}

func numberField(name string) *core.NumberField {
	f := core.Fields[core.FieldTypeNumber]().(*core.NumberField)
	f.Name = name
	return f
}

func dateField(name string) *core.DateField {
	f := core.Fields[core.FieldTypeDate]().(*core.DateField)
	f.Name = name
	return f
}

// autodateField is my "created"/"updated" equivalent. PocketBase v0.40
// removed the implicit timestamp system fields — timestamps are now regular
// AutodateFields that I must declare myself, and they fill themselves in
// via a record interceptor (record.Set on them is a deliberate no-op). I
// mark them System so nobody can rename or drop them out from under my
// sorts and my datasheet.
func autodateField(name string, onCreate, onUpdate bool) *core.AutodateField {
	f := core.Fields[core.FieldTypeAutodate]().(*core.AutodateField)
	f.Name = name
	f.System = true
	f.OnCreate = onCreate
	f.OnUpdate = onUpdate
	return f
}

// tableHasColumn tells me whether the record table ACTUALLY has a column
// for the given field. The table — not the collection's field list — is my
// source of truth, because an earlier version of my binary declared the
// timestamp fields without the column ever materializing (app.Save's
// internal table sync diffs against the cached collection, so mutating the
// cached object in place produced an empty diff).
func tableHasColumn(app core.App, table, column string) bool {
	cols, err := app.TableColumns(table)
	if err != nil {
		return false
	}
	for _, name := range cols {
		if strings.EqualFold(name, column) {
			return true
		}
	}
	return false
}

// stampCreatedField upgrades a dynamic collection that predates my
// AutodateField support so its table has a "created" column. Idempotent —
// it trusts the table, so it also repairs collections whose JSON already
// declares the field but whose column was never synced.
func stampCreatedField(app core.App, c *core.Collection) error {
	if tableHasColumn(app, c.Name, "created") {
		return nil
	}

	// Snapshot the current JSON and strip "created" from the copy. The copy
	// becomes the honest "old" side of the table sync, guaranteeing a real
	// diff whether or not the field is already declared on c.
	oldJSON, err := c.MarshalJSON()
	if err != nil {
		return err
	}
	var old core.Collection
	if err := old.UnmarshalJSON(oldJSON); err != nil {
		return err
	}
	old.Fields.RemoveByName("created")

	if c.Fields.GetByName("created") == nil {
		c.Fields.Add(autodateField("created", true, false))
		if serr := app.Save(c); serr != nil {
			return serr
		}
	}
	return app.SyncRecordTableSchema(c, &old)
}

// ensureFormsCollection creates my persistent `forms` collection if it
// doesn't exist yet. I create it programmatically (create-if-missing)
// instead of shipping a migration file because it's the only "static"
// collection in my app, and idempotency is easier to reason about in one
// place — my OnBootstrap hook.
//
// If the collection already exists I still reconcile its timestamp fields:
// versions of my binary before I learned that v0.40 dropped the implicit
// created/updated system fields provisioned this collection without them,
// and I need the columns to exist for my sorts and my API views.
func ensureFormsCollection(app core.App) error {
	c, err := app.FindCachedCollectionByNameOrId(formsCollectionName)
	if err == nil {
		// Reconcile an existing collection. Both the FIELD declaration and
		// the TABLE column must exist for each timestamp — an earlier
		// version of my binary declared the fields without the columns ever
		// being synced, so I check the table for each one.
		needCreated := c.Fields.GetByName("created") == nil || !tableHasColumn(app, formsCollectionName, "created")
		needUpdated := c.Fields.GetByName("updated") == nil || !tableHasColumn(app, formsCollectionName, "updated")
		if !needCreated && !needUpdated {
			return nil
		}

		// The "old" side of the table sync: a copy of the collection with
		// every field whose column is missing removed, so the sync diff is
		// exactly "add these columns".
		oldJSON, merr := c.MarshalJSON()
		if merr != nil {
			return fmt.Errorf("failed to snapshot the %q collection: %w", formsCollectionName, merr)
		}
		var old core.Collection
		if uerr := old.UnmarshalJSON(oldJSON); uerr != nil {
			return fmt.Errorf("failed to restore %q snapshot: %w", formsCollectionName, uerr)
		}
		if !tableHasColumn(app, formsCollectionName, "created") {
			old.Fields.RemoveByName("created")
		}
		if !tableHasColumn(app, formsCollectionName, "updated") {
			old.Fields.RemoveByName("updated")
		}

		changed := false
		if c.Fields.GetByName("created") == nil {
			c.Fields.Add(autodateField("created", true, false))
			changed = true
		}
		if c.Fields.GetByName("updated") == nil {
			c.Fields.Add(autodateField("updated", false, true))
			changed = true
		}
		if changed {
			// the field declarations are new -> persist them to the
			// _collections table
			if serr := app.Save(c); serr != nil {
				return fmt.Errorf("failed to save %q collection: %w", formsCollectionName, serr)
			}
		}
		if serr := app.SyncRecordTableSchema(c, &old); serr != nil {
			return fmt.Errorf("failed to sync timestamp columns onto %q: %w", formsCollectionName, serr)
		}
		app.Logger().Info("synced timestamp fields onto the forms collection")
		return nil
	}

	c = core.NewBaseCollection(formsCollectionName)

	title := textField("title")
	title.Required = true
	title.Max = 200

	shortCode := textField("short_code")
	shortCode.Required = true
	shortCode.Max = 16

	passHash := textField("access_password_hash")
	passHash.Max = 128

	passSalt := textField("access_password_salt")
	passSalt.Max = 64

	schema := jsonField("schema")
	schema.Required = true

	lifespan := numberField("lifespan_days")
	lifespan.Required = true
	lifespan.OnlyInt = true
	lifespan.Min = types.Pointer(1.0)
	lifespan.Max = types.Pointer(30.0)

	expiresAt := dateField("expires_at")
	expiresAt.Required = true

	status := selectField("status", []string{"active", "expired"})
	status.Required = true

	created := autodateField("created", true, false)
	updated := autodateField("updated", false, true)

	c.Fields = core.NewFieldsList(title, shortCode, passHash, passSalt, schema, lifespan, expiresAt, status, created, updated)
	denyAllRules(c)

	if err := app.Save(c); err != nil {
		return fmt.Errorf("failed to create the %q collection: %w", formsCollectionName, err)
	}
	app.Logger().Info("created the forms collection")
	return nil
}

// buildDynamicCollection constructs (but does not persist) the dynamic
// collection for a form, deriving one PocketBase field per schema question:
// text -> TextField, choice/dropdown -> SelectField with the question's
// options. PocketBase then validates every submission against these fields
// for me, which is exactly the enforcement layer I want between a
// respondent's JSON and my SQLite table.
func buildDynamicCollection(form *core.Record) (*core.Collection, error) {
	schema, err := parseSchema(form)
	if err != nil {
		return nil, err
	}

	c := core.NewBaseCollection(dynamicCollectionName(form.GetString("id")))

	fields := make([]core.Field, 0, len(schema.Questions))
	for _, q := range schema.Questions {
		// I declare the field OUTSIDE the switch — case-scoped variables
		// wouldn't reach the append below.
		var field core.Field
		switch q.Type {
		case questionTypeText:
			f := textField(q.ID)
			f.Required = q.Required
			f.Max = 2000 // long-form answers allowed, runaway payloads capped
			field = f
		case questionTypeChoice, questionTypeDropdown:
			f := selectField(q.ID, q.Options)
			f.Required = q.Required
			field = f
		}
		fields = append(fields, field)
	}

	// The "created" autodate field is what my "-created" sort and my
	// datasheet's "Submitted" column rely on — in v0.40 nothing adds it
	// for me, so I declare it explicitly. Submissions are append-only, so
	// I don't bother with an "updated" column.
	fields = append(fields, autodateField("created", true, false))

	c.Fields = core.NewFieldsList(fields...)
	denyAllRules(c)
	return c, nil
}

// parseSchema reads the persisted JSON schema off a form record.
func parseSchema(form *core.Record) (formSchema, error) {
	var s formSchema
	if err := form.UnmarshalJSONField("schema", &s); err != nil {
		return s, fmt.Errorf("form %s has an unreadable schema: %w", form.GetString("id"), err)
	}
	return s, nil
}

// schemaQuestionCount is a convenience for my list views.
func schemaQuestionCount(form *core.Record) int {
	s, err := parseSchema(form)
	if err != nil {
		return 0
	}
	return len(s.Questions)
}

// ensureFormCollections is my boot-time reconciliation pass. It runs on
// every OnBootstrap (after e.Next(), so the DB is live):
//
//  1. any active form already past its expiry (plus my skew buffer) gets
//     the full purge — table dropped, status flipped to "expired" — my
//     safety net for forms that expired while the server was down, and
//  2. any active form whose dynamic collection is missing gets it rebuilt
//     from the stored schema.
//
// Idempotent by design — I never assume the previous boot left things in a
// particular state.
func ensureFormCollections(app core.App) error {
	forms, err := app.FindRecordsByFilter(formsCollectionName, "", "", 0, 0)
	if err != nil {
		return fmt.Errorf("failed to list forms: %w", err)
	}

	now := time.Now()
	for _, form := range forms {
		if form.GetString("status") != "active" {
			continue // expired forms keep no table — by design
		}

		deadline := now.Add(-purgeClockSkewBuffer)
		if form.GetDateTime("expires_at").Time().Before(deadline) {
			// Past my skew buffer -> the FULL purge: drop the table AND
			// mark expired. A mark-only path would leave respondent data
			// on disk, and my cron's first pass only ever sees ACTIVE
			// forms, so nothing else would clean it up until the orphan
			// sweep ran.
			if perr := purgeForm(app, form); perr != nil {
				app.Logger().Warn("failed to purge past-due form at boot", "form", form.GetString("id"), "error", perr)
			}
			continue
		}

		if form.GetDateTime("expires_at").Time().Before(now) {
			// Expired but inside my skew buffer -> mark only; the table
			// drops on my next cron tick once the buffer passes.
			form.Set("status", "expired")
			if serr := app.Save(form); serr != nil {
				app.Logger().Warn("failed to mark form expired", "form", form.GetString("id"), "error", serr)
			}
		}

		dynName := dynamicCollectionName(form.GetString("id"))
		if dyn, cerr := app.FindCachedCollectionByNameOrId(dynName); cerr == nil {
			// Table exists — but if it predates my AutodateField support it
			// lacks the "created" column, so I stamp it in.
			if serr := stampCreatedField(app, dyn); serr != nil {
				app.Logger().Warn("failed to add created field to dynamic collection", "collection", dynName, "error", serr)
			}
			continue
		}

		// Table missing for an active form — rebuild it from the schema.
		dyn, berr := buildDynamicCollection(form)
		if berr != nil {
			app.Logger().Warn("failed to rebuild dynamic collection", "form", form.GetString("id"), "error", berr)
			continue
		}
		if serr := app.Save(dyn); serr != nil {
			app.Logger().Warn("failed to persist rebuilt dynamic collection", "form", form.GetString("id"), "error", serr)
			continue
		}
		app.Logger().Info("rebuilt dynamic collection at boot", "collection", dynName)
	}
	return nil
}

// purgeForm executes my "ephemeral" promise for one form: it drops the
// dynamic collection (app.Delete runs the system hook that executes
// DROP TABLE — I verified this in core/collection_model.go) and marks the
// form expired. I keep the form record itself on purpose: it holds no
// respondent data, and keeping it lets /s/<code> answer 410 Gone ("expired")
// instead of a bare 404, which is the honest response for a self-destructed
// form.
func purgeForm(app core.App, form *core.Record) error {
	dynName := dynamicCollectionName(form.GetString("id"))
	if dyn, err := app.FindCachedCollectionByNameOrId(dynName); err == nil {
		if derr := app.Delete(dyn); derr != nil {
			return fmt.Errorf("failed to drop %s: %w", dynName, derr)
		}
	}

	form.Set("status", "expired")
	if serr := app.Save(form); serr != nil {
		return fmt.Errorf("failed to mark form expired: %w", serr)
	}

	app.Logger().Info("purged expired form", "form", form.GetString("id"), "shortCode", form.GetString("short_code"))
	return nil
}

// deleteFormCompletely is the creator's explicit "delete forever" — purge
// plus removal of the form record itself.
func deleteFormCompletely(app core.App, form *core.Record) error {
	if err := purgeForm(app, form); err != nil {
		return err
	}
	if err := app.Delete(form); err != nil {
		return fmt.Errorf("failed to delete form record: %w", err)
	}
	return nil
}
