package store

import (
	"strings"
	"testing"

	"github.com/Einlanzerous/purser/internal/connector"
)

// Migration 0004's one-shot backfill identifies tasks that were recorded as
// 'failed' but were really unavailable by matching connector.ErrPending's
// message as a literal prefix of last_error. That makes the sentinel's text
// load-bearing schema, and nothing else in the build would notice it drifting:
// edit the string and the migration still applies cleanly, every other test
// still passes, and the backfill silently stops matching the rows it exists for.
//
// This is the pin. It needs no database — it compares the shipped SQL against
// the shipped sentinel, both read from the code that actually runs.
func TestMigration0004_BackfillMatchesErrPendingsText(t *testing.T) {
	migs, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	var sql string
	for _, m := range migs {
		if m.version == "0004" {
			sql = m.sql
		}
	}
	if sql == "" {
		t.Fatal("migration 0004 not found in the embedded FS")
	}

	want := connector.ErrPending.Error()
	if !strings.Contains(sql, "'"+want+"%'") {
		t.Errorf("migration 0004 does not grep for connector.ErrPending's current text.\n"+
			"ErrPending.Error() = %q\n"+
			"Changing that string is a schema-adjacent edit: update the LIKE pattern in\n"+
			"0004, and consider whether already-migrated rows need a follow-up migration.\n\nSQL:\n%s",
			want, sql)
	}
}
