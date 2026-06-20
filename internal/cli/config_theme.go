package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"dv/internal/config"
	"dv/internal/discourse"
	"dv/internal/docker"
	"dv/internal/resources"
	"dv/internal/xdg"
)

const (
	themeWatcherScriptPath = "/usr/local/bin/dv_theme_watcher.rb"
	themeAPIKeyDir         = "/home/discourse/.dv/theme_api_keys"
	defaultThemeOwner      = "discourse"
)

type themeCommandContext struct {
	cfg           *config.Config
	configDir     string
	containerName string
	discourseRoot string
	dataDir       string
	verbose       bool
	envs          docker.Envs
}

func (ctx themeCommandContext) hostMirrorPath(slug string) string {
	clean := themeDirSlug(slug)
	if clean == "" {
		clean = "theme"
	}
	return filepath.Join(ctx.dataDir, fmt.Sprintf("%s_src", clean))
}

func (ctx themeCommandContext) verboseLog(cmd *cobra.Command, format string, args ...interface{}) {
	if !ctx.verbose {
		return
	}
	fmt.Fprintf(cmd.OutOrStdout(), format+"\n", args...)
}

var configThemeCmd = &cobra.Command{
	Use:   "theme [REPO]",
	Short: "Create or link a Discourse theme workspace and update the workdir",
	Long: `Without arguments, this command scaffolds a new theme under /home/discourse inside the
target container. Pass a git URL or GitHub slug (owner/repo) to clone an existing theme.
In both cases the workdir override is updated and an AGENTS.md guide is written to the
theme root so AI tooling understands the layout.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		configDir, err := xdg.ConfigDir()
		if err != nil {
			return err
		}
		cfg, err := config.LoadOrCreate(configDir)
		if err != nil {
			return err
		}

		containerOverride, _ := cmd.Flags().GetString("container")
		containerName := strings.TrimSpace(containerOverride)
		if containerName == "" {
			containerName = currentAgentName(cfg)
		}
		if strings.TrimSpace(containerName) == "" {
			fmt.Fprintln(cmd.ErrOrStderr(), "No container selected. Run 'dv start' first.")
			return nil
		}

		if !docker.Exists(containerName) {
			fmt.Fprintf(cmd.OutOrStdout(), "Container '%s' does not exist. Run 'dv start' first.\n", containerName)
			return nil
		}
		if !docker.Running(containerName) {
			fmt.Fprintf(cmd.OutOrStdout(), "Starting container '%s'...\n", containerName)
			if err := startContainerWithPostStartHook(cmd, cfg, configDir, containerName, "config theme"); err != nil {
				return err
			}
		}

		imgName := cfg.ContainerImages[containerName]
		var imgCfg config.ImageConfig
		if imgName != "" {
			imgCfg = cfg.Images[imgName]
		} else {
			if _, resolved, err := resolveImage(cfg, ""); err == nil {
				imgCfg = resolved
			} else {
				return err
			}
		}

		dataDir, err := xdg.DataDir()
		if err != nil {
			return err
		}

		verboseFlag, _ := cmd.Flags().GetBool("verbose")

		discourseRoot := strings.TrimSpace(imgCfg.Workdir)
		if discourseRoot == "" {
			discourseRoot = "/var/www/discourse"
		}

		ctx := themeCommandContext{
			cfg:           &cfg,
			configDir:     configDir,
			containerName: containerName,
			discourseRoot: discourseRoot,
			dataDir:       dataDir,
			verbose:       verboseFlag,
			envs:          collectEnvPassthrough(cfg),
		}

		themeNameFlag, _ := cmd.Flags().GetString("theme-name")
		themeNameFlag = strings.TrimSpace(themeNameFlag)

		if len(args) == 0 {
			return handleThemeScaffold(cmd, ctx, themeNameFlag)
		}
		t := templateTheme{
			Repo:      args[0],
			Name:      themeNameFlag,
			AutoWatch: false,
			Path:      "", // default
		}
		return handleThemeClone(cmd, ctx, t)
	},
}

func init() {
	configThemeCmd.Flags().String("theme-name", "", "Friendly name to use for the theme (defaults to input)")
	configThemeCmd.Flags().String("container", "", "Container to configure (defaults to the selected agent)")
	configThemeCmd.Flags().String("kind", "", "Scaffold as 'theme' or 'component' (prompts when omitted)")
	configThemeCmd.Flags().Bool("verbose", false, "Print diagnostic output during theme setup")
	configCmd.AddCommand(configThemeCmd)
}

func handleThemeScaffold(cmd *cobra.Command, ctx themeCommandContext, flagName string) error {
	name := flagName
	if name == "" {
		var err error
		name, err = promptThemeName(cmd)
		if err != nil {
			return err
		}
	}

	kindFlag, _ := cmd.Flags().GetString("kind")
	isComponent, err := resolveThemeKind(cmd, kindFlag)
	if err != nil {
		return err
	}

	dirSlug := themeDirSlug(name)
	serviceName := fmt.Sprintf("theme-watch-%s", dirSlug)
	themePath := path.Join("/home/discourse", dirSlug)
	hostMirrorPath := ctx.hostMirrorPath(dirSlug)
	if err := ensureContainerPathAvailable(ctx.containerName, themePath); err != nil {
		return err
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Installing discourse_theme gem inside '%s'...\n", ctx.containerName)
	if err := installDiscourseThemeGem(cmd, ctx.containerName); err != nil {
		return err
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Creating theme skeleton at %s...\n", themePath)
	if err := scaffoldThemeIntoContainer(ctx, name, isComponent, serviceName, themePath, "", hostMirrorPath); err != nil {
		return err
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Initializing git repository (main) inside %s...\n", themePath)
	if err := ensureThemeGitRepo(cmd, ctx, themePath); err != nil {
		return err
	}

	serviceName, err = finalizeThemeWorkspace(cmd, ctx, finalizeThemeOptions{
		DisplayName:    name,
		ThemePath:      themePath,
		RepoURL:        "",
		IsComponent:    isComponent,
		Slug:           dirSlug,
		ServiceName:    serviceName,
		HostMirrorPath: hostMirrorPath,
	})
	if err != nil {
		return err
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Theme '%s' ready at %s. Watcher service '%s' now tracks changes.\n", name, themePath, serviceName)
	return nil
}

func handleThemeClone(cmd *cobra.Command, ctx themeCommandContext, theme templateTheme) error {
	normalizedTheme, repoURL, defaultName, err := normalizeThemeCloneSpec(theme)
	if err != nil {
		return err
	}
	theme = normalizedTheme

	name := theme.Name
	if name == "" {
		name = defaultName
	}
	dirSlug := themeDirSlug(name)
	serviceName := fmt.Sprintf("theme-watch-%s", dirSlug)
	themePath := theme.Path
	if strings.TrimSpace(themePath) == "" {
		themePath = path.Join("/home/discourse", dirSlug)
	}
	hostMirrorPath := ctx.hostMirrorPath(dirSlug)
	if err := ensureContainerPathAvailable(ctx.containerName, themePath); err != nil {
		return err
	}

	cloneArgs := []string{"git", "clone"}
	if theme.Branch != "" {
		cloneArgs = append(cloneArgs, "--branch", theme.Branch)
	}
	cloneArgs = append(cloneArgs, repoURL, themePath)
	fmt.Fprintf(cmd.OutOrStdout(), "Cloning %s into %s...\n", repoURL, themePath)
	cloneScript := shellJoin(cloneArgs)
	if out, err := docker.ExecOutput(ctx.containerName, ctx.discourseRoot, ctx.envs, []string{"bash", "-lc", cloneScript}); err != nil {
		if strings.TrimSpace(out) != "" {
			fmt.Fprint(cmd.ErrOrStderr(), out)
		}
		return fmt.Errorf("git clone failed: %w", err)
	} else if strings.TrimSpace(out) != "" {
		fmt.Fprint(cmd.OutOrStdout(), out)
	}

	if theme.PR != 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "Checking out theme PR %d...\n", theme.PR)
		if err := checkoutThemePR(ctx, themePath, theme.PR); err != nil {
			return err
		}
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Ensuring discourse_theme gem is available...\n")
	if err := installDiscourseThemeGem(cmd, ctx.containerName); err != nil {
		return err
	}

	enableTheme := themeEnabledDefault(theme, false)
	installResult, err := uploadThemeIntoDiscourse(cmd, ctx, themePath, enableTheme)
	if err != nil {
		return err
	}
	isComponent := installResult.Component

	serviceName, err = finalizeThemeWorkspace(cmd, ctx, finalizeThemeOptions{
		DisplayName:     name,
		ThemePath:       themePath,
		RepoURL:         repoURL,
		IsComponent:     isComponent,
		Slug:            dirSlug,
		ServiceName:     serviceName,
		HostMirrorPath:  hostMirrorPath,
		UploadedThemeID: installResult.ID,
	})
	if err != nil {
		return err
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Linked theme '%s' at %s (repo: %s). Watcher service '%s' now tracks changes.\n", name, themePath, repoURL, serviceName)
	return nil
}

type finalizeThemeOptions struct {
	DisplayName     string
	ThemePath       string
	RepoURL         string
	IsComponent     bool
	Slug            string
	ServiceName     string
	HostMirrorPath  string
	UploadedThemeID int
}

func finalizeThemeWorkspace(cmd *cobra.Command, ctx themeCommandContext, opts finalizeThemeOptions) (string, error) {
	serviceName := opts.ServiceName
	if strings.TrimSpace(serviceName) == "" {
		serviceName = fmt.Sprintf("theme-watch-%s", opts.Slug)
	}
	hostMirror := strings.TrimSpace(opts.HostMirrorPath)
	if hostMirror == "" {
		hostMirror = ctx.hostMirrorPath(opts.Slug)
	}
	if err := writeAgentFileToContainer(ctx, opts.ThemePath, opts.DisplayName, opts.RepoURL, serviceName, opts.IsComponent, hostMirror); err != nil {
		return "", err
	}
	if err := configureThemeWatcher(cmd, ctx, opts, serviceName); err != nil {
		return "", err
	}
	if err := setContainerWorkdir(ctx.cfg, ctx.configDir, ctx.containerName, opts.ThemePath); err != nil {
		return "", err
	}
	return serviceName, nil
}

func promptThemeName(cmd *cobra.Command) (string, error) {
	if !isTerminalInput() {
		return "", errors.New("stdin is not interactive; pass --theme-name instead")
	}
	fmt.Fprint(cmd.OutOrStdout(), "Theme name: ")
	reader := bufio.NewReader(cmd.InOrStdin())
	value, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", errors.New("theme name cannot be empty")
	}
	return trimmed, nil
}

func resolveThemeKind(cmd *cobra.Command, flagValue string) (bool, error) {
	trimmed := strings.ToLower(strings.TrimSpace(flagValue))
	switch trimmed {
	case "":
		return promptThemeKind(cmd)
	case "theme":
		return false, nil
	case "component":
		return true, nil
	default:
		return false, fmt.Errorf("invalid --kind value %q, expected 'theme' or 'component'", flagValue)
	}
}

func promptThemeKind(cmd *cobra.Command) (bool, error) {
	if !isTerminalInput() {
		return false, errors.New("stdin is not interactive; pass --kind theme|component")
	}
	fmt.Fprint(cmd.OutOrStdout(), "Is this a theme component? [y/N]: ")
	reader := bufio.NewReader(cmd.InOrStdin())
	value, err := reader.ReadString('\n')
	if err != nil {
		return false, err
	}
	answer := strings.ToLower(strings.TrimSpace(value))
	return answer == "y" || answer == "yes", nil
}

func isTerminalInput() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

func themeDirSlug(name string) string {
	lower := strings.ToLower(strings.TrimSpace(name))
	if lower == "" {
		return "theme"
	}
	var builder strings.Builder
	lastDash := false
	for _, r := range lower {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			builder.WriteRune(r)
			lastDash = false
		case r == '-' || r == '_':
			builder.WriteRune(r)
			lastDash = false
		case unicode.IsSpace(r):
			if !lastDash {
				builder.WriteRune('-')
				lastDash = true
			}
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			builder.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				builder.WriteRune('-')
				lastDash = true
			}
		}
	}
	slug := strings.Trim(builder.String(), "-")
	if slug == "" {
		return "theme"
	}
	return slug
}

func ensureContainerPathAvailable(containerName, themePath string) error {
	script := fmt.Sprintf("if [ -e %s ]; then echo '__DV_EXISTS__'; fi", shellQuote(themePath))
	out, err := docker.ExecOutput(containerName, "/home/discourse", nil, []string{"bash", "-lc", script})
	if err != nil {
		return fmt.Errorf("failed to check %s: %w", themePath, err)
	}
	if strings.Contains(out, "__DV_EXISTS__") {
		return fmt.Errorf("path %s already exists in container %s", themePath, containerName)
	}
	return nil
}

func installDiscourseThemeGem(cmd *cobra.Command, containerName string) error {
	script := "set -euo pipefail; gem install discourse_theme --no-document"
	out, err := docker.ExecAsRoot(containerName, "/root", nil, []string{"bash", "-lc", script})
	if err != nil {
		if strings.TrimSpace(out) != "" {
			fmt.Fprint(cmd.ErrOrStderr(), out)
		}
		return fmt.Errorf("failed to install discourse_theme gem: %w", err)
	}
	if strings.TrimSpace(out) != "" {
		fmt.Fprint(cmd.OutOrStdout(), out)
	}
	return nil
}

func scaffoldThemeIntoContainer(ctx themeCommandContext, displayName string, isComponent bool, serviceName, themePath, repoURL, hostMirrorPath string) error {
	tempDir, err := os.MkdirTemp("", "dv-theme-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tempDir)

	root := filepath.Join(tempDir, "theme")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}

	if err := writeThemeSkeleton(root, themeSkeletonPayload{
		DisplayName:            displayName,
		IsComponent:            isComponent,
		ServiceName:            serviceName,
		ThemePath:              themePath,
		ContainerName:          ctx.containerName,
		ContainerDiscoursePath: ctx.discourseRoot,
		HostDiscoursePath:      hostMirrorPath,
		RepositoryURL:          repoURL,
	}); err != nil {
		return err
	}

	if err := docker.CopyToContainerWithOwnership(ctx.containerName, root, themePath, true); err != nil {
		return err
	}
	return nil
}

func writeAgentFileToContainer(ctx themeCommandContext, themePath, displayName, repoURL, serviceName string, isComponent bool, hostMirrorPath string) error {
	content, err := resources.RenderThemeAgent(resources.ThemeAgentData{
		ThemeName:              displayName,
		ThemePath:              themePath,
		ContainerName:          ctx.containerName,
		ContainerDiscoursePath: ctx.discourseRoot,
		HostDiscoursePath:      hostMirrorPath,
		RepositoryURL:          repoURL,
		ServiceName:            serviceName,
		IsComponent:            isComponent,
	})
	if err != nil {
		return err
	}
	tmpFile, err := os.CreateTemp("", "dv-agent-*.md")
	if err != nil {
		return err
	}
	defer func() {
		tmpFile.Close()
		os.Remove(tmpFile.Name())
	}()

	if _, err := tmpFile.WriteString(content); err != nil {
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}

	agentPath := path.Join(themePath, "AGENTS.md")
	if err := docker.CopyToContainerWithOwnership(ctx.containerName, tmpFile.Name(), agentPath, false); err != nil {
		return err
	}
	return nil
}

func ensureThemeGitRepo(cmd *cobra.Command, ctx themeCommandContext, themePath string) error {
	script := `set -u
if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  exit 0
fi
if git init -b main >/dev/null 2>&1; then
  exit 0
fi
if git init >/dev/null 2>&1; then
  git branch -M main >/dev/null 2>&1 && exit 0
fi
exit 1
`
	out, err := docker.ExecOutput(ctx.containerName, themePath, nil, []string{"bash", "-lc", script})
	if err != nil {
		trimmed := strings.TrimSpace(out)
		if trimmed != "" {
			ctx.verboseLog(cmd, "git init output:\n%s", trimmed)
			return fmt.Errorf("failed to initialize git repo in %s: %s", themePath, trimmed)
		}
		return fmt.Errorf("failed to initialize git repo in %s: %w", themePath, err)
	}
	return nil
}

type themeSkeletonPayload struct {
	DisplayName            string
	IsComponent            bool
	ServiceName            string
	ThemePath              string
	ContainerName          string
	ContainerDiscoursePath string
	HostDiscoursePath      string
	RepositoryURL          string
}

func writeThemeSkeleton(root string, payload themeSkeletonPayload) error {
	dirs := []string{
		"common",
		"desktop",
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			return err
		}
	}

	about := map[string]any{
		"name":          payload.DisplayName,
		"about_url":     "",
		"license_url":   "",
		"component":     payload.IsComponent,
		"assets":        map[string]any{},
		"color_schemes": map[string]any{},
	}
	jsonBytes, err := json.MarshalIndent(about, "", "  ")
	if err != nil {
		return err
	}
	jsonBytes = append(jsonBytes, '\n')
	if err := os.WriteFile(filepath.Join(root, "about.json"), jsonBytes, 0o644); err != nil {
		return err
	}

	if err := os.WriteFile(filepath.Join(root, "common", "common.scss"), []byte("/* Shared SCSS */\n"), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, "desktop", "desktop.scss"), []byte("/* Desktop-only SCSS */\n"), 0o644); err != nil {
		return err
	}
	readme := fmt.Sprintf("# %s\n\nBootstrapped via `dv config theme`.\n", payload.DisplayName)
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte(readme), 0o644); err != nil {
		return err
	}

	content, err := resources.RenderThemeAgent(resources.ThemeAgentData{
		ThemeName:              payload.DisplayName,
		ThemePath:              payload.ThemePath,
		ContainerName:          payload.ContainerName,
		ContainerDiscoursePath: payload.ContainerDiscoursePath,
		HostDiscoursePath:      payload.HostDiscoursePath,
		RepositoryURL:          payload.RepositoryURL,
		ServiceName:            payload.ServiceName,
		IsComponent:            payload.IsComponent,
	})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte(content), 0o644)
}

type themeInstallResult struct {
	ID        int    `json:"id"`
	Name      string `json:"name"`
	Component bool   `json:"component"`
	Action    string `json:"action"`
	ParentID  int    `json:"parent_id"`
}

func uploadThemeIntoDiscourse(cmd *cobra.Command, ctx themeCommandContext, themePath string, enable bool) (themeInstallResult, error) {
	if enable {
		fmt.Fprintf(cmd.OutOrStdout(), "Uploading and enabling theme in Discourse...\n")
	} else {
		fmt.Fprintf(cmd.OutOrStdout(), "Uploading theme in Discourse...\n")
	}

	ruby := `require "json"

path = ENV.fetch("DV_THEME_PATH")
enable_theme = ENV.fetch("DV_THEME_ENABLE") == "1"

theme = RemoteTheme.import_theme_from_directory(path)
# Discourse's Theme#enabled flag means "not disabled". It is separate from
# dv's enabled option, which controls whether we attach a component or make
# a full theme the default after upload.
theme.enabled = true
if theme.changed?
  theme.save!
end

action = "uploaded"
parent_id = nil

if enable_theme
  if theme.component?
    parent = Theme.find_by(id: SiteSetting.default_theme_id)
    parent = nil if parent&.component?
    parent ||= Theme.find_by(id: -1)
    parent ||= Theme.not_components.order(:id).first
    raise "No parent theme found for component #{theme.name}" if parent.nil?

    if !parent.child_theme_ids.include?(theme.id)
      parent.add_relative_theme!(:child, theme)
    end

    action = "attached"
    parent_id = parent.id
  else
    theme.set_default!
    action = "default"
  end

  Theme.clear_cache!
  Theme.expire_site_cache!
end

STDOUT.sync = true
puts "DV_THEME_RESULT:" + JSON.generate(
  id: theme.id,
  name: theme.name,
  component: theme.component?,
  action: action,
  parent_id: parent_id,
)
`
	enableValue := "0"
	if enable {
		enableValue = "1"
	}
	runner := fmt.Sprintf("DV_THEME_PATH=%s DV_THEME_ENABLE=%s RAILS_ENV=development bundle exec rails runner - <<'RUBY'\n%s\nRUBY", shellQuote(themePath), shellQuote(enableValue), ruby)
	out, err := docker.ExecCombinedOutput(ctx.containerName, ctx.discourseRoot, ctx.envs, []string{"bash", "-lc", runner})
	if err != nil {
		trimmed := strings.TrimSpace(out)
		if trimmed != "" {
			return themeInstallResult{}, fmt.Errorf("failed to upload theme: %w\n%s", err, trimmed)
		}
		return themeInstallResult{}, fmt.Errorf("failed to upload theme: %w", err)
	}

	var result themeInstallResult
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "DV_THEME_RESULT:") {
			continue
		}
		payload := strings.TrimPrefix(line, "DV_THEME_RESULT:")
		if err := json.Unmarshal([]byte(payload), &result); err != nil {
			return themeInstallResult{}, fmt.Errorf("parse theme upload result: %w", err)
		}
		break
	}
	if result.ID == 0 {
		return themeInstallResult{}, fmt.Errorf("theme upload did not report a theme id: %s", strings.TrimSpace(out))
	}

	switch result.Action {
	case "attached":
		fmt.Fprintf(cmd.OutOrStdout(), "Theme component '%s' uploaded and attached to default theme.\n", result.Name)
	case "default":
		fmt.Fprintf(cmd.OutOrStdout(), "Theme '%s' uploaded and set as the default theme.\n", result.Name)
	default:
		fmt.Fprintf(cmd.OutOrStdout(), "Theme '%s' uploaded.\n", result.Name)
	}
	return result, nil
}

func configureThemeWatcher(cmd *cobra.Command, ctx themeCommandContext, opts finalizeThemeOptions, serviceName string) error {
	ctx.verboseLog(cmd, "Configuring watcher service %s for %s", serviceName, opts.ThemePath)
	discourseURL, err := resolveInternalDiscourseURL(ctx)
	if err != nil {
		return err
	}
	ctx.verboseLog(cmd, "Using internal Discourse URL: %s", discourseURL)
	apiKey, keyPath, err := ensureThemeAPIKey(cmd, ctx, opts.Slug)
	if err != nil {
		return err
	}
	ctx.verboseLog(cmd, "Stored API key at %s", keyPath)
	if err := ensureThemeWatcherScript(cmd, ctx); err != nil {
		return err
	}
	if err := writeThemeCLIConfig(cmd, ctx, opts.ThemePath, discourseURL, apiKey, opts.UploadedThemeID); err != nil {
		return err
	}
	return installWatcherService(cmd, ctx, serviceName, opts, discourseURL, keyPath)
}

func ensureThemeWatcherScript(cmd *cobra.Command, ctx themeCommandContext) error {
	checkCmd := fmt.Sprintf("test -x %s", shellQuote(themeWatcherScriptPath))
	ctx.verboseLog(cmd, "Ensuring watcher script at %s", themeWatcherScriptPath)
	if _, err := docker.ExecAsRoot(ctx.containerName, "/", nil, []string{"bash", "-lc", checkCmd}); err == nil {
		return nil
	}
	if _, err := docker.ExecAsRoot(ctx.containerName, "/", nil, []string{"bash", "-lc", fmt.Sprintf("mkdir -p %s", shellQuote(path.Dir(themeWatcherScriptPath)))}); err != nil {
		return err
	}
	tmpFile, err := os.CreateTemp("", "dv-theme-watcher-*.rb")
	if err != nil {
		return err
	}
	defer func() {
		tmpFile.Close()
		os.Remove(tmpFile.Name())
	}()
	if _, err := tmpFile.Write(resources.ThemeWatcherScript); err != nil {
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}
	ctx.verboseLog(cmd, "Copying watcher script into container")
	if err := docker.CopyToContainerWithOwnership(ctx.containerName, tmpFile.Name(), themeWatcherScriptPath, false); err != nil {
		return err
	}
	if _, err := docker.ExecAsRoot(ctx.containerName, "/", nil, []string{"chmod", "755", themeWatcherScriptPath}); err != nil {
		return err
	}
	return nil
}

func ensureThemeAPIKey(cmd *cobra.Command, ctx themeCommandContext, slug string) (string, string, error) {
	keyPath := themeKeyPath(slug)
	description := fmt.Sprintf("theme-watch-%s", slug)

	ctx.verboseLog(cmd, "Ensuring API key for theme %s at %s", slug, keyPath)

	key, _, err := discourse.EnsureAPIKeyForService(
		ctx.containerName,
		ctx.discourseRoot,
		description,
		keyPath,
		ctx.envs,
		ctx.verbose,
	)
	if err != nil {
		return "", "", err
	}

	return key, keyPath, nil
}

func writeThemeCLIConfig(cmd *cobra.Command, ctx themeCommandContext, themePath, discourseURL, apiKey string, themeID int) error {
	ruby := `require "discourse_theme"
DiscourseTheme::Cli.settings_file = File.expand_path("~/.discourse_theme")
config = DiscourseTheme::Config.new(DiscourseTheme::Cli.settings_file)
settings = config[ENV.fetch("THEME_DIR")]
settings.url = ENV.fetch("DISCOURSE_URL")
settings.api_key = ENV.fetch("DISCOURSE_API_KEY")
if ENV["DISCOURSE_THEME_ID"].to_i > 0
  settings.theme_id = ENV["DISCOURSE_THEME_ID"].to_i
end
`
	cmdStr := fmt.Sprintf("THEME_DIR=%s DISCOURSE_URL=%s DISCOURSE_API_KEY=%s DISCOURSE_THEME_ID=%s ruby <<'RUBY'\n%s\nRUBY", shellQuote(themePath), shellQuote(discourseURL), shellQuote(apiKey), shellQuote(strconv.Itoa(themeID)), ruby)
	ctx.verboseLog(cmd, "Writing ~/.discourse_theme entry for %s", themePath)
	if _, err := docker.ExecOutput(ctx.containerName, ctx.discourseRoot, ctx.envs, []string{"bash", "-lc", cmdStr}); err != nil {
		return fmt.Errorf("failed to update discourse_theme config: %w", err)
	}
	return nil
}

func installWatcherService(cmd *cobra.Command, ctx themeCommandContext, serviceName string, opts finalizeThemeOptions, discourseURL, keyPath string) error {
	serviceDir := path.Join("/etc/service", serviceName)
	ctx.verboseLog(cmd, "Creating runit service in %s (key path %s)", serviceDir, keyPath)
	if _, err := docker.ExecAsRoot(ctx.containerName, "/", nil, []string{"bash", "-lc", fmt.Sprintf("mkdir -p %s", shellQuote(serviceDir))}); err != nil {
		return err
	}
	runContent := fmt.Sprintf(`#!/bin/bash
set -euo pipefail

KEY_PATH=%s
THEME_DIR=%s
THEME_NAME=%s
WATCHER_BIN=%s
DISCOURSE_URL=%s
DISCOURSE_HOME=/home/discourse

if [ ! -s "$KEY_PATH" ]; then
  echo "Missing API key at $KEY_PATH" >&2
  sleep 5
  exit 1
fi

export DISCOURSE_URL="$DISCOURSE_URL"
export DISCOURSE_API_KEY="$(cat "$KEY_PATH")"
export THEME_DIR="$THEME_DIR"
export THEME_NAME="$THEME_NAME"
export HOME="$DISCOURSE_HOME"
export XDG_CONFIG_HOME="$DISCOURSE_HOME/.config"

cd "$THEME_DIR"
exec chpst -u discourse:discourse -U discourse:discourse ruby "$WATCHER_BIN"
`, shellQuote(keyPath), shellQuote(opts.ThemePath), shellQuote(opts.DisplayName), shellQuote(themeWatcherScriptPath), shellQuote(discourseURL))
	tmpFile, err := os.CreateTemp("", "dv-theme-run-*.sh")
	if err != nil {
		return err
	}
	defer func() {
		tmpFile.Close()
		os.Remove(tmpFile.Name())
	}()
	if _, err := tmpFile.WriteString(runContent); err != nil {
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}
	if err := docker.CopyToContainerWithOwnership(ctx.containerName, tmpFile.Name(), path.Join(serviceDir, "run"), false); err != nil {
		return err
	}
	if _, err := docker.ExecAsRoot(ctx.containerName, "/", nil, []string{"chmod", "+x", path.Join(serviceDir, "run")}); err != nil {
		return err
	}
	restartCmd := fmt.Sprintf("sv restart %s >/dev/null 2>&1 || sv start %s >/dev/null 2>&1", serviceName, serviceName)
	ctx.verboseLog(cmd, "Restarting %s via: %s", serviceName, restartCmd)
	if _, err := docker.ExecAsRoot(ctx.containerName, "/", nil, []string{"bash", "-lc", restartCmd}); err != nil {
		ctx.verboseLog(cmd, "Watcher restart command failed (continuing anyway): %v", err)
	}

	statusCmd := fmt.Sprintf("sv status %s", serviceName)
	ctx.verboseLog(cmd, "Checking watcher health via: %s", statusCmd)
	statusOut, err := docker.ExecAsRoot(ctx.containerName, "/", nil, []string{"bash", "-lc", statusCmd})
	if err != nil {
		msg := strings.TrimSpace(statusOut)
		if msg == "" {
			msg = err.Error()
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "Watcher service %s not ready yet (%s). Check later with 'sv status %s'.\n", serviceName, msg, serviceName)
		return nil
	}
	ctx.verboseLog(cmd, "Watcher status: %s", strings.TrimSpace(statusOut))
	return nil
}

func resolveInternalDiscourseURL(ctx themeCommandContext) (string, error) {
	out, err := docker.ExecOutput(ctx.containerName, ctx.discourseRoot, nil, []string{"bash", "-lc", "echo -n ${UNICORN_PORT:-3000}"})
	if err != nil {
		return "", err
	}
	port := strings.TrimSpace(out)
	if port == "" {
		port = "3000"
	}
	if _, err := strconv.Atoi(port); err != nil {
		return "", fmt.Errorf("invalid UNICORN_PORT value: %s", port)
	}
	return fmt.Sprintf("http://127.0.0.1:%s", port), nil
}

func themeKeyPath(slug string) string {
	return path.Join(themeAPIKeyDir, fmt.Sprintf("%s.key", slug))
}

func lastNonEmptyLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func resolveThemeSpecs(inputs []string) ([]templateTheme, error) {
	out := make([]templateTheme, 0, len(inputs))
	seenPaths := map[string]string{}
	for _, input := range inputs {
		theme, _, defaultName, err := resolveThemeSpec(input)
		if err != nil {
			return nil, err
		}
		name := theme.Name
		if name == "" {
			name = defaultName
		}
		themePath := strings.TrimSpace(theme.Path)
		if themePath == "" {
			themePath = path.Join("/home/discourse", themeDirSlug(name))
		}
		if prev, ok := seenPaths[themePath]; ok {
			return nil, fmt.Errorf("themes %q and %q both resolve to %s", prev, input, themePath)
		}
		seenPaths[themePath] = input
		out = append(out, theme)
	}
	return out, nil
}

func resolveThemeSpec(input string) (templateTheme, string, string, error) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return templateTheme{}, "", "", fmt.Errorf("theme cannot be empty")
	}
	theme := templateTheme{Repo: trimmed, Enabled: boolPtr(true)}
	return normalizeThemeCloneSpec(theme)
}

func normalizeThemeCloneSpec(theme templateTheme) (templateTheme, string, string, error) {
	originalRepo := strings.TrimSpace(theme.Repo)
	if originalRepo == "" {
		return theme, "", "", fmt.Errorf("theme repo cannot be empty")
	}

	if owner, repo, pr, ok := parseGitHubPullURL(originalRepo); ok {
		if theme.PR != 0 && theme.PR != pr {
			return theme, "", "", fmt.Errorf("theme repo %q points to PR %d but pr is set to %d", originalRepo, pr, theme.PR)
		}
		theme.Repo = githubRepoCloneURL(owner, repo)
		theme.PR = pr
	} else {
		base, pr, hasPR, err := splitThemePRShorthand(originalRepo)
		if err != nil {
			return theme, "", "", err
		}
		if hasPR {
			if theme.PR != 0 && theme.PR != pr {
				return theme, "", "", fmt.Errorf("theme repo %q points to PR %d but pr is set to %d", originalRepo, pr, theme.PR)
			}
			theme.Repo = base
			theme.PR = pr
		}
	}
	if theme.Branch != "" && theme.PR != 0 {
		return theme, "", "", fmt.Errorf("theme %q cannot specify both branch and pr", originalRepo)
	}

	repoURL, defaultName := normalizeThemeRepo(theme.Repo)
	if repoURL == "" {
		return theme, "", "", fmt.Errorf("could not determine repo URL from %q", originalRepo)
	}
	theme.Repo = repoURL
	if theme.PR != 0 {
		owner, repo := ownerRepoFromURL(repoURL)
		if owner == "" || repo == "" {
			return theme, "", "", fmt.Errorf("theme PR checkout requires a GitHub repo, got %q", repoURL)
		}
	}
	return theme, repoURL, defaultName, nil
}

func splitThemePRShorthand(input string) (string, int, bool, error) {
	idx := strings.LastIndex(input, "#")
	if idx < 0 {
		return input, 0, false, nil
	}
	base := strings.TrimSpace(input[:idx])
	prText := strings.TrimSpace(input[idx+1:])
	if base == "" || prText == "" {
		return "", 0, false, fmt.Errorf("invalid theme PR shorthand %q; use OWNER/REPO#123", input)
	}
	pr, err := strconv.Atoi(prText)
	if err != nil || pr <= 0 {
		return "", 0, false, fmt.Errorf("invalid theme PR shorthand %q; use OWNER/REPO#123", input)
	}
	return base, pr, true, nil
}

func parseGitHubPullURL(input string) (string, string, int, bool) {
	trimmed := strings.TrimSpace(strings.TrimSuffix(input, "/"))
	if strings.HasPrefix(trimmed, "github.com/") || strings.HasPrefix(trimmed, "www.github.com/") {
		trimmed = "https://" + trimmed
	}
	u, err := url.Parse(trimmed)
	if err != nil || u.Host == "" {
		return "", "", 0, false
	}
	host := strings.ToLower(u.Host)
	if host != "github.com" && host != "www.github.com" {
		return "", "", 0, false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 4 || parts[2] != "pull" {
		return "", "", 0, false
	}
	pr, err := strconv.Atoi(parts[3])
	if err != nil || pr <= 0 {
		return "", "", 0, false
	}
	owner := strings.TrimSpace(parts[0])
	repo := strings.TrimSuffix(strings.TrimSpace(parts[1]), ".git")
	if owner == "" || repo == "" {
		return "", "", 0, false
	}
	return owner, repo, pr, true
}

func githubRepoCloneURL(owner, repo string) string {
	return fmt.Sprintf("https://github.com/%s/%s.git", owner, strings.TrimSuffix(repo, ".git"))
}

func boolPtr(v bool) *bool {
	return &v
}

func themeEnabledDefault(theme templateTheme, defaultValue bool) bool {
	if theme.Enabled == nil {
		return defaultValue
	}
	return *theme.Enabled
}

func checkoutThemePR(ctx themeCommandContext, themePath string, prNumber int) error {
	branch := fmt.Sprintf("dv-pr-%d", prNumber)
	script := fmt.Sprintf(`set -euo pipefail
git fetch origin pull/%d/head:%s
git checkout %s
`, prNumber, shellQuote(branch), shellQuote(branch))
	out, err := docker.ExecCombinedOutput(ctx.containerName, themePath, ctx.envs, []string{"bash", "-lc", script})
	if err != nil {
		if strings.TrimSpace(out) != "" {
			return fmt.Errorf("theme PR checkout failed: %w: %s", err, strings.TrimSpace(out))
		}
		return fmt.Errorf("theme PR checkout failed: %w", err)
	}
	return nil
}

func normalizeThemeRepo(input string) (string, string) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return "", ""
	}
	if looksLikeGitURL(trimmed) {
		return trimmed, themeNameFromRepo(trimmed)
	}
	if !strings.Contains(trimmed, "/") {
		trimmed = defaultThemeOwner + "/" + trimmed
	}
	parts := strings.Split(trimmed, "/")
	if len(parts) != 2 {
		return "", ""
	}
	cloneURL := githubRepoCloneURL(parts[0], parts[1])
	return cloneURL, themeNameFromRepo(trimmed)
}

func themeNameFromRepo(ref string) string {
	ref = strings.TrimSuffix(ref, "/")
	ref = strings.TrimSuffix(ref, ".git")
	base := path.Base(ref)
	if base == "" {
		return "theme"
	}
	return base
}
