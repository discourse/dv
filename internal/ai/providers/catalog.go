package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"dv/internal/ai"
)

// CatalogOptions controls how provider catalogs are loaded.
type CatalogOptions struct {
	CacheDir   string
	Env        map[string]string
	TTL        time.Duration
	HTTPClient *http.Client
}

// LoadCatalog aggregates available provider models using built-in connectors.
func LoadCatalog(ctx context.Context, opts CatalogOptions) (ai.ProviderCatalog, error) {
	if opts.TTL <= 0 {
		opts.TTL = 30 * time.Minute
	}
	if opts.Env == nil {
		opts.Env = hostEnv()
	}
	cacheDir := opts.CacheDir
	if cacheDir == "" {
		cacheDir = filepath.Join(os.TempDir(), "dv-ai-providers-cache")
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return ai.ProviderCatalog{}, err
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}

	var entries []ai.ProviderEntry
	for _, conn := range builtinConnectors {
		entry := ai.ProviderEntry{
			ID:      conn.id(),
			Title:   conn.title(),
			EnvKeys: conn.envKeys(),
		}
		entry.HasCredentials = conn.hasCredentials(opts.Env)
		cachePath := filepath.Join(cacheDir, entry.ID+".json")

		if entry.HasCredentials {
			models, fetchedAt, err := conn.fetch(ctx, client, opts.Env)
			if err != nil {
				entry.Error = err.Error()
				if cached, cacheTime, cacheErr := loadCache(cachePath, opts.TTL); cacheErr == nil {
					entry.Models = cached
					entry.LastUpdated = cacheTime
				} else {
					entry.Error = fmt.Sprintf("%s (no cache)", err)
				}
			} else {
				entry.Models = models
				entry.LastUpdated = fetchedAt
				_ = saveCache(cachePath, models, fetchedAt)
			}
		} else {
			// Credentials are required to show provider models. Do not populate entries
			// from stale cache when the matching API key is absent from the current env.
			// This keeps the TUI catalog limited to providers the user can configure now.
		}
		entries = append(entries, entry)
	}

	return ai.ProviderCatalog{Entries: entries}, nil
}

func hostEnv() map[string]string {
	out := map[string]string{}
	for _, kv := range os.Environ() {
		if !strings.Contains(kv, "=") {
			continue
		}
		parts := strings.SplitN(kv, "=", 2)
		out[parts[0]] = parts[1]
	}
	return out
}

type cachePayload struct {
	RetrievedAt time.Time          `json:"retrieved_at"`
	Models      []ai.ProviderModel `json:"models"`
}

func loadCache(path string, ttl time.Duration) ([]ai.ProviderModel, time.Time, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, time.Time{}, err
	}
	var payload cachePayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, time.Time{}, err
	}
	if payload.RetrievedAt.IsZero() || time.Since(payload.RetrievedAt) > ttl {
		return nil, time.Time{}, fmt.Errorf("cache stale")
	}
	return payload.Models, payload.RetrievedAt, nil
}

func saveCache(path string, models []ai.ProviderModel, timestamp time.Time) error {
	payload := cachePayload{
		RetrievedAt: timestamp,
		Models:      models,
	}
	tmp := path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
