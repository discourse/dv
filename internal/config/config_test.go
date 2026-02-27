package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestEffectiveWorkdir_ContainerOverrideTakesPrecedence(t *testing.T) {
	t.Parallel()

	cfg := Config{
		Workdir:        "/global/workdir",
		CustomWorkdirs: map[string]string{"mycontainer": "/container/override"},
	}
	img := ImageConfig{Workdir: "/image/workdir"}

	got := EffectiveWorkdir(cfg, img, "mycontainer")
	if got != "/container/override" {
		t.Fatalf("expected /container/override, got %q", got)
	}
}

func TestEffectiveWorkdir_FallsBackToImageWorkdir(t *testing.T) {
	t.Parallel()

	cfg := Config{
		Workdir:        "/global/workdir",
		CustomWorkdirs: map[string]string{},
	}
	img := ImageConfig{Workdir: "/image/workdir"}

	got := EffectiveWorkdir(cfg, img, "mycontainer")
	if got != "/image/workdir" {
		t.Fatalf("expected /image/workdir, got %q", got)
	}
}

func TestEffectiveWorkdir_FallsBackToLegacyGlobalWorkdir(t *testing.T) {
	t.Parallel()

	cfg := Config{
		Workdir:        "/global/workdir",
		CustomWorkdirs: map[string]string{},
	}
	img := ImageConfig{Workdir: ""}

	got := EffectiveWorkdir(cfg, img, "mycontainer")
	if got != "/global/workdir" {
		t.Fatalf("expected /global/workdir, got %q", got)
	}
}

func TestEffectiveWorkdir_FallsBackToDefault(t *testing.T) {
	t.Parallel()

	cfg := Config{
		Workdir:        "",
		CustomWorkdirs: map[string]string{},
	}
	img := ImageConfig{Workdir: ""}

	got := EffectiveWorkdir(cfg, img, "mycontainer")
	if got != "/var/www/discourse" {
		t.Fatalf("expected /var/www/discourse, got %q", got)
	}
}

func TestEffectiveWorkdir_HandlesEmptyStringsAndWhitespace(t *testing.T) {
	t.Parallel()

	cfg := Config{
		Workdir:        "   ",
		CustomWorkdirs: map[string]string{"mycontainer": "  "},
	}
	img := ImageConfig{Workdir: "  "}

	got := EffectiveWorkdir(cfg, img, "mycontainer")
	if got != "/var/www/discourse" {
		t.Fatalf("expected /var/www/discourse, got %q", got)
	}
}

func TestEffectiveWorkdir_NilCustomWorkdirs(t *testing.T) {
	t.Parallel()

	cfg := Config{
		Workdir:        "",
		CustomWorkdirs: nil,
	}
	img := ImageConfig{Workdir: "/image/workdir"}

	got := EffectiveWorkdir(cfg, img, "mycontainer")
	if got != "/image/workdir" {
		t.Fatalf("expected /image/workdir, got %q", got)
	}
}

func TestMigrateCopyFiles_EmptyCopyRulesGetsDefaults(t *testing.T) {
	t.Parallel()

	cfg := Config{
		CopyRules: nil,
		CopyFiles: nil,
	}
	cfg.migrateCopyFiles()

	if len(cfg.CopyRules) == 0 {
		t.Fatal("expected default copy rules to be added")
	}
	// Check for a known default rule
	found := false
	for _, r := range cfg.CopyRules {
		if r.Host == "~/.claude/.credentials.json" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected default claude credentials rule")
	}
}

func TestMigrateCopyFiles_ExistingCopyFilesMigratedToCopyRules(t *testing.T) {
	t.Parallel()

	cfg := Config{
		CopyRules: []CopyRule{},
		CopyFiles: map[string]string{
			"~/custom/file": "/container/path",
		},
	}
	cfg.migrateCopyFiles()

	if cfg.CopyFiles != nil {
		t.Fatal("expected CopyFiles to be cleared after migration")
	}
	found := false
	for _, r := range cfg.CopyRules {
		if r.Host == "~/custom/file" && r.Container == "/container/path" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected migrated copy rule")
	}
}

func TestMigrateCopyFiles_DuplicateRulesNotAppended(t *testing.T) {
	t.Parallel()

	// Start with a default rule already in CopyRules
	existingRule := CopyRule{
		Host:      "~/.claude/.credentials.json",
		Container: "/home/discourse/.claude/.credentials.json",
	}
	cfg := Config{
		CopyRules: []CopyRule{existingRule},
		CopyFiles: nil,
	}

	initialLen := len(cfg.CopyRules)
	cfg.migrateCopyFiles()

	// Count how many times the claude credentials rule appears
	count := 0
	for _, r := range cfg.CopyRules {
		if r.Host == "~/.claude/.credentials.json" {
			count++
		}
	}
	if count > 1 {
		t.Fatalf("expected no duplicate rules, got %d copies of claude credentials rule", count)
	}
	// Should not have grown because the one default we already had won't be re-added
	if len(cfg.CopyRules) < initialLen {
		t.Fatal("rules should not have been removed")
	}
}

func TestLoadOrCreate_CreatesDefaultConfigWhenFileDoesNotExist(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cfg, err := LoadOrCreate(tmpDir)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	// Check config file was created
	if _, err := os.Stat(Path(tmpDir)); os.IsNotExist(err) {
		t.Fatal("expected config file to be created")
	}

	// Check defaults
	if cfg.SelectedImage != "discourse" {
		t.Fatalf("expected SelectedImage 'discourse', got %q", cfg.SelectedImage)
	}
	if cfg.Images == nil || len(cfg.Images) == 0 {
		t.Fatal("expected Images map to be initialized")
	}
}

func TestLoadOrCreate_LoadsExistingConfig(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	existing := Config{
		SelectedImage: "custom-image",
		Images: map[string]ImageConfig{
			"custom-image": {
				Kind:    "custom",
				Tag:     "custom-tag",
				Workdir: "/custom/workdir",
			},
		},
		ContainerImages: map[string]string{},
		CustomWorkdirs:  map[string]string{},
	}
	data, _ := json.MarshalIndent(existing, "", "  ")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(tmpDir), data, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadOrCreate(tmpDir)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if cfg.SelectedImage != "custom-image" {
		t.Fatalf("expected SelectedImage 'custom-image', got %q", cfg.SelectedImage)
	}
}

func TestLoadOrCreate_HandlesInvalidJSON(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(tmpDir), []byte("{invalid json"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadOrCreate(tmpDir)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestLoadOrCreate_MigratesLegacyConfigFormat(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	// Legacy config without Images map
	legacy := map[string]interface{}{
		"imageTag":      "old-tag",
		"workdir":       "/old/workdir",
		"containerPort": 4200,
	}
	data, _ := json.MarshalIndent(legacy, "", "  ")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(tmpDir), data, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadOrCreate(tmpDir)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	// Should have migrated to new format
	if cfg.Images == nil || len(cfg.Images) == 0 {
		t.Fatal("expected Images map to be initialized during migration")
	}
	if cfg.SelectedImage != "discourse" {
		t.Fatalf("expected SelectedImage 'discourse' after migration, got %q", cfg.SelectedImage)
	}
	discourse, ok := cfg.Images["discourse"]
	if !ok {
		t.Fatal("expected discourse image to be created during migration")
	}
	if discourse.Tag != "old-tag" {
		t.Fatalf("expected migrated tag 'old-tag', got %q", discourse.Tag)
	}
}

func TestLoadOrCreate_InitializesNilMaps(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	// Config with nil maps
	partial := map[string]interface{}{
		"selectedImage": "discourse",
		"images": map[string]interface{}{
			"discourse": map[string]interface{}{
				"kind":    "discourse",
				"tag":     "test",
				"workdir": "/var/www/discourse",
			},
		},
		// CustomWorkdirs and ContainerImages are missing/nil
	}
	data, _ := json.MarshalIndent(partial, "", "  ")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(tmpDir), data, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadOrCreate(tmpDir)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	if cfg.ContainerImages == nil {
		t.Fatal("expected ContainerImages to be initialized")
	}
	if cfg.CustomWorkdirs == nil {
		t.Fatal("expected CustomWorkdirs to be initialized")
	}
}

func TestLocalProxyApplyDefaults_EmptyFieldsGetDefaults(t *testing.T) {
	t.Parallel()

	lp := LocalProxyConfig{}
	lp.ApplyDefaults()

	if lp.ContainerName != "dv-local-proxy" {
		t.Fatalf("expected ContainerName 'dv-local-proxy', got %q", lp.ContainerName)
	}
	if lp.ImageTag != "dv-local-proxy" {
		t.Fatalf("expected ImageTag 'dv-local-proxy', got %q", lp.ImageTag)
	}
	if lp.HTTPPort != 80 {
		t.Fatalf("expected HTTPPort 80, got %d", lp.HTTPPort)
	}
	if lp.APIPort != 2080 {
		t.Fatalf("expected APIPort 2080, got %d", lp.APIPort)
	}
	if lp.Hostname != "dv.localhost" {
		t.Fatalf("expected Hostname 'dv.localhost', got %q", lp.Hostname)
	}
}

func TestLocalProxyApplyDefaults_HTTPSEnablesPort443(t *testing.T) {
	t.Parallel()

	lp := LocalProxyConfig{HTTPS: true}
	lp.ApplyDefaults()

	if lp.HTTPSPort != 443 {
		t.Fatalf("expected HTTPSPort 443, got %d", lp.HTTPSPort)
	}
}

func TestLocalProxyApplyDefaults_NonEmptyFieldsPreserved(t *testing.T) {
	t.Parallel()

	lp := LocalProxyConfig{
		ContainerName: "custom-proxy",
		ImageTag:      "custom-tag",
		HTTPPort:      8080,
		APIPort:       3000,
		Hostname:      "custom.localhost",
	}
	lp.ApplyDefaults()

	if lp.ContainerName != "custom-proxy" {
		t.Fatalf("expected ContainerName to be preserved, got %q", lp.ContainerName)
	}
	if lp.ImageTag != "custom-tag" {
		t.Fatalf("expected ImageTag to be preserved, got %q", lp.ImageTag)
	}
	if lp.HTTPPort != 8080 {
		t.Fatalf("expected HTTPPort to be preserved, got %d", lp.HTTPPort)
	}
	if lp.APIPort != 3000 {
		t.Fatalf("expected APIPort to be preserved, got %d", lp.APIPort)
	}
	if lp.Hostname != "custom.localhost" {
		t.Fatalf("expected Hostname to be preserved, got %q", lp.Hostname)
	}
}

func TestAppendMissingDefaultCopyRules_CaseInsensitiveDuplicateDetection(t *testing.T) {
	t.Parallel()

	// Use mixed case host path
	existing := []CopyRule{{
		Host:      "~/.CLAUDE/.CREDENTIALS.JSON",
		Container: "/home/discourse/.claude/.credentials.json",
	}}
	defaults := []CopyRule{{
		Host:      "~/.claude/.credentials.json",
		Container: "/home/discourse/.claude/.credentials.json",
	}}

	result := appendMissingDefaultCopyRules(existing, defaults)

	// Should not add duplicate
	count := 0
	for _, r := range result {
		if r.Container == "/home/discourse/.claude/.credentials.json" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 rule, got %d", count)
	}
}

func TestAppendMissingDefaultCopyRules_PreservesExistingRules(t *testing.T) {
	t.Parallel()

	existing := []CopyRule{{
		Host:      "~/my/custom/file",
		Container: "/container/custom/file",
	}}
	defaults := DefaultCopyRules()

	result := appendMissingDefaultCopyRules(existing, defaults)

	// First rule should be our custom one
	if result[0].Host != "~/my/custom/file" {
		t.Fatal("expected existing rules to be preserved at the start")
	}
	// Should have our custom rule + all defaults
	if len(result) < len(defaults)+1 {
		t.Fatalf("expected at least %d rules, got %d", len(defaults)+1, len(result))
	}
}

func TestAppendMissingDefaultCopyRules_AppendsMissingDefaults(t *testing.T) {
	t.Parallel()

	existing := []CopyRule{}
	defaults := DefaultCopyRules()

	result := appendMissingDefaultCopyRules(existing, defaults)

	if len(result) != len(defaults) {
		t.Fatalf("expected %d rules, got %d", len(defaults), len(result))
	}
}

func TestLoadOrCreate_DefaultTemplateField(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	// Create config with defaultTemplate set
	cfg := Config{
		SelectedImage: "discourse",
		Images: map[string]ImageConfig{
			"discourse": {
				Kind:    "discourse",
				Tag:     "test",
				Workdir: "/var/www/discourse",
			},
		},
		ContainerImages: map[string]string{},
		CustomWorkdirs:  map[string]string{},
		DefaultTemplate: "/path/to/template.yaml",
	}

	if err := Save(tmpDir, cfg); err != nil {
		t.Fatal(err)
	}

	// Load config and verify DefaultTemplate persisted
	loaded, err := LoadOrCreate(tmpDir)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	if loaded.DefaultTemplate != "/path/to/template.yaml" {
		t.Fatalf("expected DefaultTemplate '/path/to/template.yaml', got %q", loaded.DefaultTemplate)
	}
}

func TestSave_DefaultTemplateOmittedWhenEmpty(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	cfg := Config{
		SelectedImage: "discourse",
		Images: map[string]ImageConfig{
			"discourse": {
				Kind:    "discourse",
				Tag:     "test",
				Workdir: "/var/www/discourse",
			},
		},
		ContainerImages: map[string]string{},
		CustomWorkdirs:  map[string]string{},
		DefaultTemplate: "", // Explicitly set to empty
	}

	if err := Save(tmpDir, cfg); err != nil {
		t.Fatal(err)
	}

	// Read raw JSON and verify defaultTemplate is not present
	data, err := os.ReadFile(Path(tmpDir))
	if err != nil {
		t.Fatal(err)
	}

	if bytes.Contains(data, []byte("defaultTemplate")) {
		t.Fatal("expected defaultTemplate to be omitted from JSON when empty")
	}
}

func TestSave_DefaultTemplateIncludedWhenSet(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	cfg := Config{
		SelectedImage: "discourse",
		Images: map[string]ImageConfig{
			"discourse": {
				Kind:    "discourse",
				Tag:     "test",
				Workdir: "/var/www/discourse",
			},
		},
		ContainerImages: map[string]string{},
		CustomWorkdirs:  map[string]string{},
		DefaultTemplate: "templates/my-template.yaml",
	}

	if err := Save(tmpDir, cfg); err != nil {
		t.Fatal(err)
	}

	// Read raw JSON and verify defaultTemplate is present
	data, err := os.ReadFile(Path(tmpDir))
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Contains(data, []byte("defaultTemplate")) {
		t.Fatal("expected defaultTemplate to be included in JSON when set")
	}
	if !bytes.Contains(data, []byte("templates/my-template.yaml")) {
		t.Fatal("expected defaultTemplate value to be in JSON")
	}
}

func TestPath(t *testing.T) {
	t.Parallel()

	dir := "/some/config/dir"
	got := Path(dir)
	want := filepath.Join(dir, "config.json")
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestDefault(t *testing.T) {
	t.Parallel()

	cfg := Default()

	if cfg.SelectedImage != "discourse" {
		t.Fatalf("expected SelectedImage 'discourse', got %q", cfg.SelectedImage)
	}
	if cfg.Images == nil {
		t.Fatal("expected Images to be initialized")
	}
	if _, ok := cfg.Images["discourse"]; !ok {
		t.Fatal("expected discourse image to exist")
	}
	if cfg.ContainerImages == nil {
		t.Fatal("expected ContainerImages to be initialized")
	}
	if cfg.CustomWorkdirs == nil {
		t.Fatal("expected CustomWorkdirs to be initialized")
	}
	if len(cfg.CopyRules) == 0 {
		t.Fatal("expected default CopyRules")
	}
	if cfg.DefaultTemplate != "" {
		t.Fatalf("expected DefaultTemplate to be empty, got %q", cfg.DefaultTemplate)
	}
}
