// purge.go — my hourly self-cleaning job, the heart of the "ephemeral"
// promise from my product vision.
package main

import (
	"time"

	"github.com/pocketbase/pocketbase/core"
)

// purgeClockSkewBuffer — I only purge forms that expired at least this long
// ago. My LXC clock and the expiry timestamps live on the same machine, so
// this is mostly insurance against a hand-edited expires_at — but it's the
// kind of buffer that saves a 3am support session.
const purgeClockSkewBuffer = 5 * time.Minute

// runPurge is the body of my cron job. I keep it a plain function (not an
// anonymous closure in main.go) so I can unit-test it in Stage 3 without
// booting a fake HTTP server.
func runPurge(app core.App) {
	forms, err := app.FindRecordsByFilter(formsCollectionName, "status = 'active'", "", 0, 0)
	if err != nil {
		app.Logger().Error("purge: failed to list active forms", "error", err)
		return
	}

	deadline := time.Now().Add(-purgeClockSkewBuffer)
	purged := 0
	for _, form := range forms {
		if form.GetDateTime("expires_at").Time().After(deadline) {
			continue // still fresh
		}
		if perr := purgeForm(app, form); perr != nil {
			app.Logger().Error("purge: failed to purge form", "form", form.GetString("id"), "error", perr)
			continue
		}
		purged++
	}

	if purged > 0 {
		app.Logger().Info("hourly purge complete", "purged", purged)
	}

	// Pass 2: expired forms whose tables were never dropped. This catches
	// forms that expired while the server was down — my boot
	// reconciliation marks them expired first, and pass 1 above only ever
	// sees ACTIVE forms, so without this sweep their respondent data would
	// stay on disk forever. The sweep is what keeps the "ephemeral"
	// promise true across downtime.
	orphaned, oerr := app.FindRecordsByFilter(formsCollectionName, "status = 'expired'", "", 0, 0)
	if oerr != nil {
		app.Logger().Error("purge: failed to list expired forms", "error", oerr)
		return
	}
	dropped := 0
	for _, form := range orphaned {
		dynName := dynamicCollectionName(form.GetString("id"))
		if !app.HasTable(dynName) {
			continue // already clean
		}
		dyn, cerr := app.FindCachedCollectionByNameOrId(dynName)
		if cerr != nil {
			continue // no collection registered for the table — nothing to drop
		}
		if derr := app.Delete(dyn); derr != nil {
			app.Logger().Error("purge: failed to drop orphaned table", "form", form.GetString("id"), "error", derr)
			continue
		}
		app.Logger().Info("purge: dropped orphaned table for expired form", "form", form.GetString("id"), "collection", dynName)
		dropped++
	}

	if dropped > 0 {
		app.Logger().Info("purge: orphaned table sweep complete", "dropped", dropped)
	}
}
