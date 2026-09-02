package cli

import (
	"strings"
	"testing"
)

func TestBuildGitCleanupCommands_HandlesStaleHeadLockBeforeReset(t *testing.T) {
	t.Parallel()

	script := strings.Join(buildGitCleanupCommands(), "\n")
	lockCheck := strings.Index(script, "git rev-parse --git-path HEAD.lock")
	reset := strings.Index(script, "git reset --hard")

	if lockCheck == -1 {
		t.Fatalf("missing HEAD lock detection:\n%s", script)
	}
	if !strings.Contains(script, "ps -eo comm=") {
		t.Fatalf("lock recovery must check for active Git processes:\n%s", script)
	}
	if !strings.Contains(script, `rm -f -- "$head_lock"`) {
		t.Fatalf("missing stale HEAD lock removal:\n%s", script)
	}
	if reset == -1 || lockCheck > reset {
		t.Fatalf("HEAD lock recovery must happen before git reset:\n%s", script)
	}
}

func TestBuildAssetsClobberCommands_WaitsForPostgres(t *testing.T) {
	t.Parallel()

	script := strings.Join(buildAssetsClobberCommands(), "\n")
	ready := strings.Index(script, "pg_isready")
	clobber := strings.Index(script, "bin/rails assets:clobber")

	if ready == -1 {
		t.Fatalf("missing PostgreSQL readiness check:\n%s", script)
	}
	if !strings.Contains(script, "/var/run/postgresql/.s.PGSQL.5432") {
		t.Fatalf("readiness check must verify the PostgreSQL socket:\n%s", script)
	}
	if clobber == -1 {
		t.Fatalf("missing asset clobber command:\n%s", script)
	}
	if ready > clobber {
		t.Fatalf("PostgreSQL readiness check must precede asset clobber:\n%s", script)
	}
}

func TestBuildDiscourseResetScript_WaitsBeforeRails(t *testing.T) {
	t.Parallel()

	script := buildDiscourseResetScript(buildBranchCheckoutCommands("example"), discourseResetScriptOpts{})
	ready := strings.Index(script, "pg_isready")
	clobber := strings.Index(script, "bin/rails assets:clobber")

	if ready == -1 || clobber == -1 || ready > clobber {
		t.Fatalf("PostgreSQL readiness check must precede asset clobber:\n%s", script)
	}
}

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
