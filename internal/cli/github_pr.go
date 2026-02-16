package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"dv/internal/config"
	"dv/internal/docker"
	"dv/internal/xdg"
)

type ghPR struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	UpdatedAt time.Time `json:"updated_at"`
	Draft     bool      `json:"draft"`
}

type ghPRDetail struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	Head   struct {
		Ref  string `json:"ref"`
		Repo struct {
			CloneURL string `json:"clone_url"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

type prCompletionCache struct {
	FetchedAt time.Time `json:"fetched_at"`
	Owner     string    `json:"owner"`
	Repo      string    `json:"repo"`
	Query     string    `json:"query"`
	Limit     int       `json:"limit"`
	PRs       []ghPR    `json:"prs"`
}

const prCompletionCacheTTL = 45 * time.Second

var ghAuthTokenOnce sync.Once
var ghAuthTokenCached string

func githubAuthToken() string {
	if tok := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); tok != "" {
		return tok
	}
	if tok := strings.TrimSpace(os.Getenv("GH_TOKEN")); tok != "" {
		return tok
	}
	if tok := strings.TrimSpace(os.Getenv("GITHUB_PAT")); tok != "" {
		return tok
	}
	ghAuthTokenOnce.Do(func() {
		out, err := exec.Command("gh", "auth", "token").Output()
		if err == nil {
			ghAuthTokenCached = strings.TrimSpace(string(out))
		}
	})
	return ghAuthTokenCached
}

func applyGitHubHeaders(req *http.Request) {
	req.Header.Set("User-Agent", "dv-cli")
	if tok := githubAuthToken(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
}

func prCompletionCachePath() (string, error) {
	cacheDir, err := xdg.CacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cacheDir, "pr-completion.json"), nil
}

func loadPRCompletionCache(owner, repo, query string, limit int) ([]ghPR, bool) {
	cachePath, err := prCompletionCachePath()
	if err != nil {
		return nil, false
	}
	data, err := os.ReadFile(cachePath)
	if err != nil {
		return nil, false
	}
	var cache prCompletionCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return nil, false
	}
	if cache.Owner != owner || cache.Repo != repo || cache.Query != strings.ToLower(query) || cache.Limit != limit {
		return nil, false
	}
	if cache.FetchedAt.IsZero() || time.Since(cache.FetchedAt) > prCompletionCacheTTL {
		return nil, false
	}
	return cache.PRs, true
}

func savePRCompletionCache(owner, repo, query string, limit int, prs []ghPR) {
	cachePath, err := prCompletionCachePath()
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		return
	}
	cache := prCompletionCache{
		FetchedAt: time.Now().UTC(),
		Owner:     owner,
		Repo:      repo,
		Query:     strings.ToLower(query),
		Limit:     limit,
		PRs:       prs,
	}
	data, err := json.Marshal(cache)
	if err != nil {
		return
	}
	_ = os.WriteFile(cachePath, data, 0o644)
}

// fetchPRDetail fetches details for a specific PR from GitHub API
func fetchPRDetail(owner, repo string, prNumber int) (*ghPRDetail, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/pulls/%d", owner, repo, prNumber)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	applyGitHubHeaders(req)
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GitHub API error: %s", resp.Status)
	}
	var pr ghPRDetail
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return nil, err
	}
	return &pr, nil
}

// listOpenPRs queries GitHub REST API for open PRs, paginated up to limit.
func listOpenPRs(owner, repo string, limit int) ([]ghPR, error) {
	if limit <= 0 {
		limit = 30
	}
	if limit > 200 {
		limit = 200
	}
	perPage := 100
	if limit < perPage {
		perPage = limit
	}
	var all []ghPR
	page := 1
	client := &http.Client{Timeout: 8 * time.Second}
	for len(all) < limit {
		url := fmt.Sprintf("https://api.github.com/repos/%s/%s/pulls?state=open&per_page=%d&page=%d&sort=updated&direction=desc", owner, repo, perPage, page)
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return nil, err
		}
		applyGitHubHeaders(req)
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			resp.Body.Close()
			return nil, fmt.Errorf("GitHub API error: %s", resp.Status)
		}
		var prs []ghPR
		if err := json.NewDecoder(resp.Body).Decode(&prs); err != nil {
			resp.Body.Close()
			return nil, err
		}
		resp.Body.Close()
		if len(prs) == 0 {
			break
		}
		all = append(all, prs...)
		if len(all) >= limit {
			break
		}
		page++
	}
	if len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

// searchOpenPRs uses GitHub search API to find open PRs with query in title/body.
func searchOpenPRs(owner, repo, query string, limit int) ([]ghPR, error) {
	if limit <= 0 {
		limit = 30
	}
	if limit > 100 {
		limit = 100
	}
	// Use search issues API with in:title,body filter
	q := fmt.Sprintf("repo:%s/%s+is:pr+is:open+in:title,body+%s", owner, repo, query)
	url := fmt.Sprintf("https://api.github.com/search/issues?q=%s&per_page=%d&sort=updated&order=desc", urlQueryEscape(q), limit)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	applyGitHubHeaders(req)
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GitHub API error: %s", resp.Status)
	}
	var res struct {
		Items []struct {
			Number    int       `json:"number"`
			Title     string    `json:"title"`
			Body      string    `json:"body"`
			UpdatedAt time.Time `json:"updated_at"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, err
	}
	out := make([]ghPR, 0, len(res.Items))
	for _, it := range res.Items {
		out = append(out, ghPR{Number: it.Number, Title: it.Title, Body: it.Body, UpdatedAt: it.UpdatedAt})
	}
	return out, nil
}

// urlQueryEscape performs minimal escaping for GitHub search query
func urlQueryEscape(s string) string {
	// Replace spaces with '+'; leave other characters as-is for simplicity
	r := strings.ReplaceAll(s, " ", "+")
	return r
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// repoOwnerRepoFromContainer tries to read remote.origin.url inside the container
// and parse it for a GitHub owner/repo pair. Returns empty strings on failure.
func repoOwnerRepoFromContainer(cfg config.Config, containerName string) (string, string) {
	// If container isn't running, avoid starting it just for completion
	if !docker.Exists(containerName) || !docker.Running(containerName) {
		return "", ""
	}
	// Determine workdir
	imgName := cfg.ContainerImages[containerName]
	var imgCfg config.ImageConfig
	if imgName != "" {
		imgCfg = cfg.Images[imgName]
	} else {
		_, imgCfg, _ = resolveImage(cfg, "")
	}
	workdir := imgCfg.Workdir
	if strings.TrimSpace(workdir) == "" {
		workdir = "/var/www/discourse"
	}
	// Prefer upstream if present, else fall back to origin
	upOut, _ := docker.ExecOutput(containerName, workdir, nil, []string{"bash", "-lc", "git config --get remote.upstream.url || true"})
	remoteURL := strings.TrimSpace(upOut)
	if remoteURL == "" {
		out, _ := docker.ExecOutput(containerName, workdir, nil, []string{"bash", "-lc", "git config --get remote.origin.url || true"})
		remoteURL = strings.TrimSpace(out)
	}
	if remoteURL == "" {
		return "", ""
	}
	owner, repo := ownerRepoFromURL(remoteURL)
	return owner, repo
}

// prSearchOwnerRepoFromContainer chooses the best owner/repo for PR search.
// Prefer upstream remote; if origin is a fork of 'discourse' repo, normalize owner to 'discourse'.
func prSearchOwnerRepoFromContainer(cfg config.Config, containerName string) (string, string) {
	owner, repo := repoOwnerRepoFromContainer(cfg, containerName)
	if repo == "" {
		return owner, repo
	}
	// Normalize common fork case: searching PRs on upstream 'discourse' rather than fork
	if strings.EqualFold(repo, "discourse") && !strings.EqualFold(owner, "discourse") {
		return "discourse", repo
	}
	return owner, repo
}

// ownerRepoFromURL extracts GitHub owner/repo from common remote URL formats.
// Supports https and ssh formats; strips .git suffix.
func ownerRepoFromURL(url string) (string, string) {
	s := strings.TrimSpace(url)
	s = strings.TrimSuffix(s, ".git")
	// Normalize
	// Examples:
	//  https://github.com/discourse/discourse
	//  git@github.com:discourse/discourse
	//  ssh://git@github.com/discourse/discourse
	var hostIdx int
	if i := strings.Index(s, "github.com"); i >= 0 {
		hostIdx = i + len("github.com")
	} else {
		return "", ""
	}
	tail := s[hostIdx:]
	// Remove leading separators like ':' or '/'
	tail = strings.TrimLeft(tail, ":/")
	parts := strings.Split(tail, "/")
	if len(parts) < 2 {
		return "", ""
	}
	owner, repo := parts[0], parts[1]
	owner = strings.TrimSpace(owner)
	repo = strings.TrimSpace(repo)
	if owner == "" || repo == "" {
		return "", ""
	}
	return owner, repo
}

func prHeadBranchName(cfg config.Config, prNumber int) (string, error) {
	owner, repo := ownerRepoFromURL(cfg.DiscourseRepo)
	if owner == "" || repo == "" {
		return "", fmt.Errorf("unable to determine repository owner/name; check 'discourseRepo' in config")
	}
	prDetail, err := fetchPRDetail(owner, repo, prNumber)
	if err != nil {
		return "", err
	}
	if ref := strings.TrimSpace(prDetail.Head.Ref); ref != "" {
		return ref, nil
	}
	return strings.TrimSpace(prDetail.Base.Ref), nil
}

func splitCompletionQuery(input string) (string, int, bool) {
	trimmed := strings.TrimSpace(input)
	if idx := strings.Index(trimmed, ":"); idx >= 0 {
		trimmed = trimmed[:idx]
	}
	trimmed = strings.TrimSpace(trimmed)
	if trimmed == "" || isNumeric(trimmed) {
		return trimmed, 0, false
	}
	i := len(trimmed)
	for i > 0 && trimmed[i-1] >= '0' && trimmed[i-1] <= '9' {
		i--
	}
	if i < len(trimmed) {
		base := strings.TrimSpace(trimmed[:i])
		if base != "" {
			ordinal, err := strconv.Atoi(trimmed[i:])
			if err == nil && ordinal > 0 {
				return base, ordinal, true
			}
		}
	}
	return trimmed, 0, false
}

func filterPRs(prs []ghPR, query string) []ghPR {
	if query == "" {
		return prs
	}
	numeric := isNumeric(query)
	out := make([]ghPR, 0, len(prs))
	for _, pr := range prs {
		numStr := strconv.Itoa(pr.Number)
		if numeric {
			if !strings.Contains(numStr, query) {
				continue
			}
		} else {
			title := strings.ToLower(pr.Title)
			body := strings.ToLower(pr.Body)
			if !strings.Contains(title, query) && !strings.Contains(body, query) {
				continue
			}
		}
		out = append(out, pr)
	}
	return out
}

// SuggestPRNumbers returns completion candidates for PR numbers.
func SuggestPRNumbers(owner, repo string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if owner == "" || repo == "" {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	limit := 100
	if v := os.Getenv("DV_PR_COMPLETE_LIMIT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	baseQuery, ordinal, hasOrdinal := splitCompletionQuery(toComplete)
	query := strings.ToLower(baseQuery)
	prs, ok := loadPRCompletionCache(owner, repo, query, limit)
	if !ok {
		var err error
		if baseQuery != "" && !isNumeric(query) {
			prs, err = searchOpenPRs(owner, repo, baseQuery, limit)
			if err != nil || len(prs) == 0 {
				prs, err = listOpenPRs(owner, repo, limit)
			}
		} else {
			prs, err = listOpenPRs(owner, repo, limit)
		}
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		prs = filterPRs(prs, query)
		savePRCompletionCache(owner, repo, query, limit, prs)
	}
	if hasOrdinal {
		if ordinal < 1 || ordinal > len(prs) {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		prs = []ghPR{prs[ordinal-1]}
	}
	var out []string
	for _, pr := range prs {
		numStr := strconv.Itoa(pr.Number)

		// If we are searching by a string, prepend the query to the number
		// so zsh doesn't filter it out. ResolvePR will handle stripping it.
		val := numStr
		if baseQuery != "" && !isNumeric(query) {
			displayOrdinal := len(out) + 1
			if hasOrdinal {
				displayOrdinal = ordinal
			}
			val = fmt.Sprintf("%s%d:%d", baseQuery, displayOrdinal, pr.Number)
		}
		out = append(out, fmt.Sprintf("%s\t%s", val, pr.Title))
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}

// ResolvePR takes a numeric string (PR number) or a search query and returns the PR number.
// If multiple PRs match a search query, it prompts the user to select one.
func ResolvePR(cmd *cobra.Command, cfg config.Config, input string) (int, error) {
	input = strings.TrimSpace(input)

	// Handle prefixed values from SuggestPRNumbers completion (e.g. "caro:1234")
	if i := strings.LastIndex(input, ":"); i >= 0 {
		remainder := strings.TrimSpace(input[i+1:])
		if isNumeric(remainder) {
			num, _ := strconv.Atoi(remainder)
			return num, nil
		}
		prefix := strings.TrimSpace(input[:i])
		if prefix != "" {
			input = prefix
		}
	}

	if isNumeric(input) {
		num, err := strconv.Atoi(input)
		if err != nil {
			return 0, fmt.Errorf("invalid PR number: %w", err)
		}
		return num, nil
	}

	owner, repo := ownerRepoFromURL(cfg.DiscourseRepo)
	if owner == "" || repo == "" {
		return 0, fmt.Errorf("unable to determine repository owner/name; check 'discourseRepo' in config")
	}

	query := strings.TrimSpace(input)
	if query == "" {
		return 0, fmt.Errorf("invalid PR query")
	}

	const resultLimit = 10
	fmt.Fprintf(cmd.ErrOrStderr(), "Searching for open PRs matching '%s'...\n", query)
	prs, err := searchOpenPRs(owner, repo, query, resultLimit)
	if err == nil {
		prs = filterPRs(prs, strings.ToLower(query))
	}
	if err != nil || len(prs) == 0 {
		fallback, fallbackErr := listOpenPRs(owner, repo, 100)
		if fallbackErr != nil {
			if err != nil {
				return 0, fmt.Errorf("search PRs: %w", err)
			}
			return 0, fmt.Errorf("list open PRs: %w", fallbackErr)
		}
		prs = filterPRs(fallback, strings.ToLower(query))
		if len(prs) > resultLimit {
			prs = prs[:resultLimit]
		}
	}

	if len(prs) == 0 {
		return 0, fmt.Errorf("no open PRs found matching '%s'", query)
	}

	if len(prs) == 1 {
		fmt.Fprintf(cmd.ErrOrStderr(), "Found PR #%d: %s\n", prs[0].Number, prs[0].Title)
		return prs[0].Number, nil
	}

	// Multiple matches - interactive picker
	fmt.Fprintf(cmd.ErrOrStderr(), "\nMultiple PRs found matching '%s':\n", query)
	for i, pr := range prs {
		fmt.Fprintf(cmd.ErrOrStderr(), "  %d) #%d: %s\n", i+1, pr.Number, pr.Title)
	}
	fmt.Fprintln(cmd.ErrOrStderr())

	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Fprintf(cmd.ErrOrStderr(), "Select a PR (1-%d, or 'c' to cancel): ", len(prs))
		text, _ := reader.ReadString('\n')
		text = strings.TrimSpace(text)
		if strings.ToLower(text) == "c" {
			return 0, fmt.Errorf("PR selection cancelled")
		}
		choice, err := strconv.Atoi(text)
		if err == nil && choice >= 1 && choice <= len(prs) {
			return prs[choice-1].Number, nil
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "Invalid selection '%s'.\n", text)
	}
}
