// Package main is my entrypoint for forms-saas — my self-hosted, ephemeral,
// privacy-first form builder (a lean Microsoft Forms alternative).
//
// I'm extending PocketBase as my application framework instead of bolting
// it onto a web stack: one binary gives me the SQLite database, the auth
// layer, the static file server for my pb_public/ frontend, and the cron
// scheduler. I don't want a separate web server, broker, or ORM — for a
// single LXC container under forms.evanmejia.com, PocketBase's core API IS
// the backend.
package main

import (
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/hook"
	"github.com/pocketbase/pocketbase/tools/osutils"
)

func main() {
	// I create the launcher with the default config. If I later need to
	// move my data dir or tweak DB pool sizes on the LXC, I swap this for
	// pocketbase.NewWithConfig(...).
	app := pocketbase.New()

	// Note to my future self: I deliberately do NOT register PocketBase's
	// bundled plugins here — not the JS VM hooks plugin (I write Go, not
	// pb_hooks) and not the auto-update checker (this is a privacy-first
	// product and I don't want it phoning home).

	// ------------------------------------------------------------------
	// Boot-time provisioning.
	//
	// The PocketBase version I pinned (v0.40.x) has no OnBeforeServe hook
	// — the lifecycle is OnBootstrap -> OnServe -> OnTerminate.
	// OnBootstrap is the right stand-in for my provisioning logic: after
	// be.Next(), PocketBase has opened the DB and run migrations, so
	// everything after it runs against a live database, before the HTTP
	// server accepts a single request.
	//
	// This hook MUST stay idempotent — it fires on every boot, including
	// `superuser` CLI runs, and a form's dynamic collection can
	// legitimately be missing (my cron dropped it after expiry, or a form
	// was just created).
	// ------------------------------------------------------------------
	app.OnBootstrap().Bind(&hook.Handler[*core.BootstrapEvent]{
		Id: "provisionFormCollections",
		Func: func(be *core.BootstrapEvent) error {
			if err := be.Next(); err != nil {
				return err
			}
			// The order here is dependency-driven: users first (the forms
			// owner relation points at it), then forms, then the owner
			// field, then collaborators (which reference BOTH), and only
			// after all collections exist does the reconciliation pass
			// walk my forms — mark past-due ones expired and rebuild any
			// missing dynamic collections.
			if err := ensureUsersCollection(be.App); err != nil {
				return err
			}
			if err := ensureFormsCollection(be.App); err != nil {
				return err
			}
			if err := ensureFormsOwnerField(be.App); err != nil {
				return err
			}
			if err := ensureCollaboratorsCollection(be.App); err != nil {
				return err
			}
			return ensureFormCollections(be.App)
		},
	})

	// ------------------------------------------------------------------
	// Record hooks — my multi-user lifecycle rules.
	//
	// Both hooks filter on the collection name in the handler body,
	// because PocketBase's record hooks are GLOBAL (they fire for every
	// record in every collection, including dashboard-initiated changes).
	// ------------------------------------------------------------------

	// When a user signs up, any pending co-creator invite addressed to
	// their email activates immediately. I hook the AFTER-create success
	// event so a failed signup never grants anything, and I swallow
	// failures (logged, not fatal) because my canAccessForm carries a
	// pending-email fallback that keeps access working either way.
	app.OnRecordAfterCreateSuccess().Bind(&hook.Handler[*core.RecordEvent]{
		Id: "activatePendingCollaboratorsOnSignup",
		Func: func(re *core.RecordEvent) error {
			if re.Record.Collection().Name != usersCollectionName {
				return re.Next()
			}
			if err := re.Next(); err != nil {
				return err
			}
			if err := activatePendingCollaborators(re.App, re.Record); err != nil {
				re.App.Logger().Error("failed to activate pending collaborators",
					"user", re.Record.Id, "error", err)
			}
			return nil
		},
	})

	// When a user is deleted (by themselves via the API, or by a
	// superuser in the dashboard), every form they own gets the FULL
	// delete — dynamic table dropped, then the form record removed —
	// BEFORE the user row goes. Without this hook the relation cascade
	// would orphan my dyn_ tables, and respondent data outliving its
	// creator is exactly the privacy violation this product exists to
	// avoid. Returning an error aborts the user deletion itself.
	app.OnRecordDelete().Bind(&hook.Handler[*core.RecordEvent]{
		Id: "deleteOwnedFormsOnUserDelete",
		Func: func(re *core.RecordEvent) error {
			if re.Record.Collection().Name != usersCollectionName {
				return re.Next()
			}
			if err := deleteUsersForms(re.App, re.Record); err != nil {
				return err
			}
			return re.Next()
		},
	})

	// ------------------------------------------------------------------
	// HTTP routes + static frontend.
	//
	// PocketBase v0.40 hands me the router through the OnServe event —
	// after it has registered its own API routes but before it builds the
	// final mux. My /s/{shortCode} and /api/v1/... routes beat the
	// /{path...} static wildcard by Go ServeMux pattern specificity, so
	// the SPA can never shadow a real endpoint.
	// ------------------------------------------------------------------
	app.OnServe().Bind(&hook.Handler[*core.ServeEvent]{
		Id: "customRoutesAndStatic",
		Func: func(se *core.ServeEvent) error {
			registerPublicRoutes(se.Router)
			registerCreatorRoutes(se.Router)

			// My SPA. indexFallback=true makes unknown paths serve
			// index.html, which my hash router needs for refresh and deep
			// links.
			if !se.Router.HasRoute(http.MethodGet, "/{path...}") {
				se.Router.GET("/{path...}", apis.Static(os.DirFS(publicDir()), true))
			}

			return se.Next()
		},
	})

	// ------------------------------------------------------------------
	// Hourly self-cleaning cron — the heart of the "ephemeral" promise.
	//
	// FORMS_PURGE_CRON overrides the schedule; I use "* * * * *" when I
	// want to test the purge by hand without waiting for the top of the
	// hour. I run it in-process because I have exactly one LXC instance —
	// if I ever run two instances behind a load balancer I must add
	// DB-level job locking first, or two nodes could DROP the same table
	// at once.
	// ------------------------------------------------------------------
	purgeSpec := os.Getenv("FORMS_PURGE_CRON")
	if purgeSpec == "" {
		purgeSpec = "0 * * * *"
	}
	app.Cron().MustAdd("purgeExpiredForms", purgeSpec, func() {
		runPurge(app)
	})

	// app.Start() registers the default `serve` / `superuser` CLI
	// commands, bootstraps the app, and runs until SIGTERM/SIGINT.
	//
	// Deployment note: my Nginx Proxy Manager LXC (192.168.40.40)
	// terminates TLS for forms.evanmejia.com and proxies to this binary's
	// HTTP port. I therefore run `serve --http 0.0.0.0:8090` (the default
	// bind is 127.0.0.1:8090, which NPM on the Proxmox bridge cannot
	// reach), and I must never expose 8090 publicly without NPM in front —
	// the app speaks plain HTTP and trusts NPM's X-Forwarded-* headers
	// for the client IP and scheme.
	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}

// publicDir locates my pb_public/ frontend the same way PocketBase's own
// base example does: next to the source tree under `go run`, next to the
// executable for compiled binaries.
func publicDir() string {
	if osutils.IsProbablyGoRun() {
		return "./pb_public"
	}
	return filepath.Join(filepath.Dir(os.Args[0]), "pb_public")
}
