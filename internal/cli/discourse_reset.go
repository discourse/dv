package cli

import (
	"fmt"
	"strings"
)

// buildGitCleanupCommands generates commands to clean the working tree and ensure full history
func buildGitCleanupCommands() []string {
	return []string{
		"echo 'Cleaning working tree...'",
		"git reset --hard",
		"git clean -fd",
		"echo 'Ensuring full history is available (unshallow if needed)...'",
		"if [ -f .git/shallow ]; then git fetch origin --tags --prune --force --unshallow; else git fetch origin --tags --prune --force; fi",
	}
}

// buildPostCheckoutCommands generates commands to show current HEAD and reinstall dependencies
func buildPostCheckoutCommands() []string {
	return []string{
		"echo 'Current HEAD:'",
		"git --no-pager log --oneline -n 1",
		"echo 'Reinstalling dependencies (bundle and pnpm) if needed...'",
		// Best-effort; do not fail the whole command if these fail
		"(bundle check || bundle install) || true",
		"(command -v pnpm >/dev/null 2>&1 && pnpm install) || true",
	}
}

// buildDatabaseResetCommands generates commands to reset and migrate databases
func buildDatabaseResetCommands() []string {
	return []string{
		"echo 'Stopping services (as root): unicorn and ember-cli'",
		"sudo -n true 2>/dev/null || true",
		"sudo /usr/bin/sv force-stop unicorn || sudo sv force-stop unicorn || true",
		"sudo /usr/bin/sv force-stop ember-cli || sudo sv force-stop ember-cli || true",
		"echo 'Waiting for PostgreSQL to be ready...'",
		"timeout 30 bash -c 'until pg_isready > /dev/null 2>&1; do sleep 1; done' || (echo 'PostgreSQL did not become ready'; exit 1)",
		"echo 'Resetting and migrating databases (development and test)...'",
		"MIG_LOG_DEV=/tmp/dv-migrate-dev-$(date +%s).log",
		"MIG_LOG_TEST=/tmp/dv-migrate-test-$(date +%s).log",
		"(bin/rake db:drop || true)",
		"bin/rake db:create",
		"echo \"Migrating dev DB (output -> $MIG_LOG_DEV)\"",
		"bin/rake db:migrate > \"$MIG_LOG_DEV\" 2>&1",
		"echo \"Migrating test DB (output -> $MIG_LOG_TEST)\"",
		"RAILS_ENV=test bin/rake db:migrate > \"$MIG_LOG_TEST\" 2>&1",
		"bundle",
		"pnpm install",
		"echo 'Seeding users...'",
		"bin/rails r /tmp/seed_users.rb || true",
		"echo 'Migration logs:'",
		"echo \"  dev : $MIG_LOG_DEV\"",
		"echo \"  test: $MIG_LOG_TEST\"",
		"echo 'Done.'",
	}
}

// discourseResetScriptOpts configures buildDiscourseResetScript.
type discourseResetScriptOpts struct {
	SkipDBReset bool // if true, omit DB reset and migrations
}

// buildDiscourseResetScript generates a shell script that performs common
// Discourse development environment reset tasks:
// - Stops services (unicorn, ember-cli)
// - Cleans working tree
// - Ensures full git history
// - Executes custom checkout commands
// - Reinstalls dependencies
// - Optionally resets and migrates databases (when opts.SkipDBReset is false)
// - Seeds users
// - Restarts services on exit
//
// checkoutCmds should contain the git checkout logic specific to the caller
// (e.g., PR checkout, branch checkout).
func buildDiscourseResetScript(checkoutCmds []string, opts discourseResetScriptOpts) string {
	lines := []string{
		"set -euo pipefail",
		"cleanup() { echo 'Starting services (as root): unicorn and ember-cli'; sudo /usr/bin/sv start unicorn || sudo sv start unicorn || true; sudo /usr/bin/sv start ember-cli || sudo sv start ember-cli || true; }",
		"trap cleanup EXIT",
	}

	// Git cleanup
	lines = append(lines, buildGitCleanupCommands()...)

	// Caller-specific checkout commands
	lines = append(lines, checkoutCmds...)

	// Post-checkout steps
	lines = append(lines, buildPostCheckoutCommands()...)

	// Database reset (optional)
	if !opts.SkipDBReset {
		lines = append(lines, buildDatabaseResetCommands()...)
	}

	return strings.Join(lines, "\n")
}

// buildPRCheckoutCommands generates git commands to fetch and checkout a PR.
// It uses the actual branch name from GitHub to maintain branch identity.
func buildPRCheckoutCommands(prNumber int, branchName string) []string {
	refspec := fmt.Sprintf("+refs/pull/%d/head:refs/remotes/origin/pull/%d/head", prNumber, prNumber)
	prRef := fmt.Sprintf("refs/pull/%d/head", prNumber)

	return []string{
		fmt.Sprintf("pr_branch=%s", shellQuote(branchName)),
		fmt.Sprintf("pr_ref=%s", shellQuote(prRef)),
		fmt.Sprintf("pr_refspec=%s", shellQuote(refspec)),
		fmt.Sprintf("echo 'Fetching PR #%d (branch: %s) from origin...'", prNumber, branchName),
		"if ! git config --get-all remote.origin.fetch | grep -qxF \"$pr_refspec\"; then git config --add remote.origin.fetch \"$pr_refspec\"; fi",
		"git fetch origin \"$pr_refspec\"",
		fmt.Sprintf("git checkout -B \"$pr_branch\" origin/pull/%d/head", prNumber),
		"git config branch.\"${pr_branch}\".remote origin",
		"git config branch.\"${pr_branch}\".merge \"$pr_ref\"",
		fmt.Sprintf("echo \"Branch ${pr_branch} now tracks origin/pull/%d/head for git pull.\"", prNumber),
	}
}

// buildBranchCheckoutCommands generates git commands to checkout a branch.
func buildBranchCheckoutCommands(branchName string) []string {
	return []string{
		fmt.Sprintf("echo 'Checking out branch %s...'", branchName),
		fmt.Sprintf("git checkout %s", branchName),
		"git pull --ff-only || true",
	}
}

// buildNewBranchCheckoutCommands generates git commands to create and checkout
// a new branch from the default remote branch (origin/main or origin/master).
func buildNewBranchCheckoutCommands(branchName string) []string {
	return []string{
		fmt.Sprintf("echo 'Creating new branch %s from origin...'", branchName),
		"git fetch origin --tags --prune",
		"if git show-ref -q refs/remotes/origin/main; then default_ref=origin/main; else default_ref=origin/master; fi",
		fmt.Sprintf("git checkout -b %s \"$default_ref\"", shellQuote(branchName)),
	}
}

// buildCurrentBranchResetCommands generates commands to reset the current branch
// to match origin, discarding all local changes.
func buildCurrentBranchResetCommands() []string {
	return []string{
		"branch=$(git rev-parse --abbrev-ref HEAD)",
		"if [ \"$branch\" = \"HEAD\" ]; then echo 'Error: detached HEAD state'; exit 1; fi",
		"echo \"Current branch: $branch\"",
		"echo 'Fetching from origin...'",
		"git fetch origin --prune",
		"echo \"Resetting to origin/$branch...\"",
		"git reset --hard \"origin/$branch\"",
		"echo 'Cleaning untracked files...'",
		"git clean -fd",
	}
}

// buildDiscourseDatabaseResetScript generates a shell script that performs
// database reset only (no git operations):
// - Stops services (unicorn, ember-cli)
// - Resets and migrates databases
// - Seeds users
// - Restarts services on exit
func buildDiscourseDatabaseResetScript() string {
	lines := []string{
		"set -euo pipefail",
		"cleanup() { echo 'Starting services (as root): unicorn and ember-cli'; sudo /usr/bin/sv start unicorn || sudo sv start unicorn || true; sudo /usr/bin/sv start ember-cli || sudo sv start ember-cli || true; }",
		"trap cleanup EXIT",
	}

	// Database reset (reuses shared logic)
	lines = append(lines, buildDatabaseResetCommands()...)

	return strings.Join(lines, "\n")
}
