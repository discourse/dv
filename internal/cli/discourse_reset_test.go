package cli

import (
	"strings"
	"testing"
)

func TestBuildDatabaseDropCreateMigrateCommands_WithoutTestDB(t *testing.T) {
	t.Parallel()

	cmds := buildDatabaseDropCreateMigrateCommands(discourseResetScriptOpts{WithoutTestDB: true})
	script := strings.Join(cmds, "\n")

	if !strings.Contains(script, "bin/rake db:migrate") {
		t.Fatal("missing development database migration")
	}
	if strings.Contains(script, "RAILS_ENV=test") {
		t.Fatalf("test database migration should be omitted:\n%s", script)
	}
	if strings.Contains(script, "MIG_LOG_TEST") {
		t.Fatalf("test migration log should be omitted:\n%s", script)
	}
}

func TestBuildMaintenanceScript_WithoutTestDB(t *testing.T) {
	t.Parallel()

	script := buildMaintenanceScript(true)

	if !strings.Contains(script, "bin/rake db:migrate") {
		t.Fatal("missing development database migration")
	}
	if strings.Contains(script, "RAILS_ENV=test") {
		t.Fatalf("test database migration should be omitted:\n%s", script)
	}
	if strings.Contains(script, "dv-migrate-test.log") {
		t.Fatalf("test migration log should be omitted:\n%s", script)
	}
}

func TestBuildMaintenanceScript_WithTestDB(t *testing.T) {
	t.Parallel()

	script := buildMaintenanceScript(false)

	if !strings.Contains(script, "bin/rake db:migrate") {
		t.Fatal("missing development database migration")
	}
	if !strings.Contains(script, "RAILS_ENV=test bin/rake db:migrate") {
		t.Fatal("missing test database migration")
	}
}
