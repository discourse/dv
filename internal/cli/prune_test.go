package cli

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"dv/internal/config"
)

var pruneTestNow = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

func daysAgo(n int) time.Time { return pruneTestNow.Add(-time.Duration(n) * 24 * time.Hour) }

func pruneNames(candidates []pruneCandidate) []string {
	names := []string{}
	for _, c := range candidates {
		names = append(names, c.name)
	}
	return names
}

func TestSelectPruneCandidates(t *testing.T) {
	cfg := config.Default()
	cfg.ContainerImages["legacy"] = "discourse"
	containers := []pruneContainer{
		{name: "old", state: "exited", owner: "dv", finishedAt: daysAgo(30)},
		{name: "older", state: "exited", owner: "dv", finishedAt: daysAgo(60)},
		{name: "recent", state: "exited", owner: "dv", finishedAt: daysAgo(3)},
		{name: "running", state: "running", owner: "dv", finishedAt: daysAgo(90)},
		{name: "not-dv", state: "exited", finishedAt: daysAgo(90)},
		{name: "legacy", state: "exited", finishedAt: daysAgo(20)},
		{name: "never-ran", state: "created", owner: "dv", createdAt: daysAgo(40), finishedAt: time.Time{}},
		{name: "selected", state: "exited", owner: "dv", finishedAt: daysAgo(90)},
		{name: "kept", state: "exited", owner: "dv", finishedAt: daysAgo(90)},
	}
	got := pruneNames(selectPruneCandidates(cfg, containers, 14*24*time.Hour, pruneTestNow, []string{"selected", "kept", ""}))
	want := []string{"older", "never-ran", "old", "legacy"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
}

func TestParsePruneInspectUsesCreatedWhenNeverRun(t *testing.T) {
	raw := "/agent\texited\tdv\t2026-08-01T10:00:00.5Z\t0001-01-01T00:00:00Z\n" +
		"/other\texited\t\t2026-08-01T10:00:00Z\t2026-09-01T10:00:00.123456789Z\n"
	got := parsePruneInspect(raw)
	if len(got) != 2 || got[0].name != "agent" || got[0].owner != "dv" || got[1].owner != "" {
		t.Fatalf("parsed = %+v", got)
	}
	if want := time.Date(2026, 8, 1, 10, 0, 0, 500_000_000, time.UTC); !got[0].lastUsed().Equal(want) {
		t.Fatalf("lastUsed = %v, want created %v", got[0].lastUsed(), want)
	}
	if want := time.Date(2026, 9, 1, 10, 0, 0, 123456789, time.UTC); !got[1].lastUsed().Equal(want) {
		t.Fatalf("lastUsed = %v, want finished %v", got[1].lastUsed(), want)
	}
}

func TestParseAge(t *testing.T) {
	for raw, want := range map[string]time.Duration{
		"14d": 14 * 24 * time.Hour,
		"2w":  14 * 24 * time.Hour,
		"36h": 36 * time.Hour,
		"0d":  0,
	} {
		got, err := parseAge(raw)
		if err != nil || got != want {
			t.Fatalf("parseAge(%q) = %v, %v; want %v", raw, got, err, want)
		}
	}
	for _, raw := range []string{"", "d", "-3d", "soon", "-1h"} {
		if _, err := parseAge(raw); err == nil {
			t.Fatalf("parseAge(%q) succeeded, want error", raw)
		}
	}
}

func pruneTestCommand(args ...string) (*cobra.Command, *strings.Builder, *strings.Builder) {
	stdout := &strings.Builder{}
	stderr := &strings.Builder{}
	cmd := &cobra.Command{Use: "prune", RunE: runPrune}
	cmd.Flags().AddFlagSet(pruneCmd.Flags())
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	cmd.SetContext(context.Background())
	if err := cmd.ParseFlags(args); err != nil {
		panic(err)
	}
	return cmd, stdout, stderr
}

func stubPrune(t *testing.T, containers []pruneContainer, work map[string]string, checkErr map[string]error) *[]string {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("DV_AGENT", "")
	oldInspect, oldWork, oldRemove, oldNow := pruneInspect, pruneUnsavedWork, pruneRemove, pruneNow
	t.Cleanup(func() {
		pruneInspect, pruneUnsavedWork, pruneRemove, pruneNow = oldInspect, oldWork, oldRemove, oldNow
	})
	removed := &[]string{}
	pruneInspect = func(context.Context) ([]pruneContainer, error) { return containers, nil }
	pruneUnsavedWork = func(_ context.Context, _ config.Config, name string) (string, error) {
		return work[name], checkErr[name]
	}
	pruneRemove = func(_ *cobra.Command, name, _ string) error {
		*removed = append(*removed, name)
		return nil
	}
	pruneNow = func() time.Time { return pruneTestNow }
	return removed
}

func TestPruneDryRunRemovesNothing(t *testing.T) {
	removed := stubPrune(t, []pruneContainer{{name: "old", state: "exited", owner: "dv", finishedAt: daysAgo(30)}}, nil, nil)
	cmd, stdout, _ := pruneTestCommand()
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	if len(*removed) != 0 {
		t.Fatalf("removed %v in a dry run", *removed)
	}
	if !strings.Contains(stdout.String(), "old  (last ran 30 days ago)") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestPruneKeepsContainersWithUnsavedOrUncheckedWork(t *testing.T) {
	containers := []pruneContainer{
		{name: "clean", state: "exited", owner: "dv", finishedAt: daysAgo(30)},
		{name: "dirty", state: "exited", owner: "dv", finishedAt: daysAgo(30)},
		{name: "broken", state: "exited", owner: "dv", finishedAt: daysAgo(30)},
	}
	removed := stubPrune(t, containers,
		map[string]string{"dirty": "/var/www/discourse:\n   M app/models/post.rb"},
		map[string]error{"broken": errors.New("docker start failed")})
	cmd, stdout, stderr := pruneTestCommand("--yes")
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*removed, []string{"clean"}) {
		t.Fatalf("removed = %v, want [clean]", *removed)
	}
	if !strings.Contains(stdout.String(), "Keeping dirty: unsaved work") || !strings.Contains(stdout.String(), "app/models/post.rb") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "Keeping broken: could not check") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestPruneHonorsKeepAndOlderThan(t *testing.T) {
	containers := []pruneContainer{
		{name: "a", state: "exited", owner: "dv", finishedAt: daysAgo(10)},
		{name: "b", state: "exited", owner: "dv", finishedAt: daysAgo(10)},
		{name: "c", state: "exited", owner: "dv", finishedAt: daysAgo(3)},
	}
	removed := stubPrune(t, containers, nil, nil)
	cmd, _, _ := pruneTestCommand("--yes", "--older-than", "1w", "--keep", "b")
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*removed, []string{"a"}) {
		t.Fatalf("removed = %v, want [a]", *removed)
	}
}
