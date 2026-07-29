package cli

import (
	"context"
	"errors"
	"testing"

	"dv/internal/config"
)

func TestCollectContainerInventoryUsesOwnershipPrecedence(t *testing.T) {
	cfg := config.Default()
	cfg.LabelOverrides = map[string]map[string]string{}
	cfg.ContainerImages["mapped"] = "discourse"
	cfg.ContainerImages["mapped-elsewhere"] = "custom"
	cfg.LabelOverrides["overridden"] = map[string]string{
		"com.dv.owner":      "dv",
		"com.dv.image-name": "discourse",
	}
	image := cfg.Images["discourse"]
	query := func(context.Context) (string, error) {
		return "" +
			"mapped\tforeign:tag\tExited (0) 2 hours ago\t\t\t2026-07-27 10:00:00 +0000 UTC\n" +
			"labelled\tforeign:tag\tUp 1 hour\t\tcom.dv.owner=dv,com.dv.image-name=discourse\t2026-07-28 10:00:00 +0000 UTC\n" +
			"overridden\tforeign:tag\tCreated 3 hours ago\t\t\t2026-07-26 10:00:00 +0000 UTC\n" +
			"legacy\tai_agent\tUp 5 minutes\t0.0.0.0:3001->3000/tcp\t\t2026-07-29 10:00:00 +0000 UTC\n" +
			"mapped-elsewhere\tai_agent\tUp 2 minutes\t\t\t2026-07-29 10:30:00 +0000 UTC\n" +
			"unrelated\tforeign:tag\tUp 1 minute\t\t\t2026-07-29 11:00:00 +0000 UTC\n", nil
	}

	records, err := collectContainerInventory(context.Background(), cfg, "discourse", image, "labelled", query)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 4 {
		t.Fatalf("got %d records, want 4: %#v", len(records), records)
	}
	seen := map[string]containerInventoryRecord{}
	for _, record := range records {
		seen[record.name] = record
	}
	for _, name := range []string{"mapped", "labelled", "overridden", "legacy"} {
		if _, ok := seen[name]; !ok {
			t.Errorf("missing %s", name)
		}
	}
	if _, ok := seen["unrelated"]; ok {
		t.Error("unrelated container was included")
	}
	if _, ok := seen["mapped-elsewhere"]; ok {
		t.Error("explicit mapping to another image was overridden by tag fallback")
	}
	if !seen["labelled"].selected {
		t.Error("selected container was not marked")
	}
	if got := seen["legacy"].urls; len(got) != 1 || got[0] != "http://localhost:3001" {
		t.Fatalf("legacy URLs = %#v", got)
	}
}

func TestCollectContainerInventoryReturnsDockerFailure(t *testing.T) {
	want := errors.New("daemon unavailable")
	_, err := collectContainerInventory(context.Background(), config.Default(), "discourse", config.Default().Images["discourse"], "", func(context.Context) (string, error) {
		return "", want
	})
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}
