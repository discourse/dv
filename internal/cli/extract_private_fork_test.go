package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscourseExtractLocalPathKeepsPublicDefault(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	got := discourseExtractLocalPath(dataDir, "git@github.com:discourse/discourse.git")
	want := filepath.Join(dataDir, "discourse_src")
	if got != want {
		t.Fatalf("discourseExtractLocalPath() = %q, want %q", got, want)
	}
}

func TestDiscourseExtractLocalPathUsesShortRepoPathForForks(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	got := discourseExtractLocalPath(dataDir, "git@github.com:example/private-discourse.git")
	want := filepath.Join(dataDir, "private-discourse_src")
	if got != want {
		t.Fatalf("discourseExtractLocalPath() = %q, want %q", got, want)
	}
}

func TestDiscourseExtractLocalPathUsesOwnerRepoForForkNamedDiscourse(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	got := discourseExtractLocalPath(dataDir, "git@github.com:example/discourse.git")
	want := filepath.Join(dataDir, "example-discourse_src")
	if got != want {
		t.Fatalf("discourseExtractLocalPath() = %q, want %q", got, want)
	}
}

func TestDiscourseExtractLocalPathUsesOwnerRepoWhenRepoPathTaken(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	taken := filepath.Join(dataDir, "private-discourse_src")
	if err := os.MkdirAll(taken, 0o755); err != nil {
		t.Fatal(err)
	}
	gitInit(t, taken)
	runGit(t, taken, "remote", "add", "origin", "git@github.com:someone-else/private-discourse.git")

	got := discourseExtractLocalPath(dataDir, "git@github.com:example/private-discourse.git")
	want := filepath.Join(dataDir, "example-private-discourse_src")
	if got != want {
		t.Fatalf("discourseExtractLocalPath() = %q, want %q", got, want)
	}
}

func TestDiscourseExtractLocalPathUsesFullPathWhenShortPathsTaken(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	short := filepath.Join(dataDir, "private-discourse_src")
	if err := os.MkdirAll(short, 0o755); err != nil {
		t.Fatal(err)
	}
	gitInit(t, short)
	runGit(t, short, "remote", "add", "origin", "git@github.com:someone-else/private-discourse.git")

	ownerRepo := filepath.Join(dataDir, "example-private-discourse_src")
	if err := os.MkdirAll(ownerRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitInit(t, ownerRepo)
	runGit(t, ownerRepo, "remote", "add", "origin", "git@github.com:other-example/private-discourse.git")

	got := discourseExtractLocalPath(dataDir, "git@github.com:example/private-discourse.git")
	want := filepath.Join(dataDir, "github-com-example-private-discourse_src")
	if got != want {
		t.Fatalf("discourseExtractLocalPath() = %q, want %q", got, want)
	}
}

func TestDiscourseExtractLocalPathReusesMatchingShortPath(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	short := filepath.Join(dataDir, "private-discourse_src")
	if err := os.MkdirAll(short, 0o755); err != nil {
		t.Fatal(err)
	}
	gitInit(t, short)
	runGit(t, short, "remote", "add", "origin", "git@github.com:example/private-discourse.git")

	got := discourseExtractLocalPath(dataDir, "https://github.com/example/private-discourse.git")
	if got != short {
		t.Fatalf("discourseExtractLocalPath() = %q, want %q", got, short)
	}
}

func TestSameGitRemoteNormalizesCommonForms(t *testing.T) {
	t.Parallel()

	pairs := [][2]string{
		{"git@github.com:example/private-discourse.git", "https://github.com/example/private-discourse.git"},
		{"ssh://git@github.com/example/private-discourse.git", "git@github.com:example/private-discourse.git"},
		{"https://github.com/example/private-discourse", "https://github.com/example/private-discourse.git"},
	}

	for _, pair := range pairs {
		if !sameGitRemote(pair[0], pair[1]) {
			t.Fatalf("expected %q and %q to match", pair[0], pair[1])
		}
	}
}

func TestSameGitRemoteDetectsDifferentFork(t *testing.T) {
	t.Parallel()

	if sameGitRemote("git@github.com:discourse/discourse.git", "git@github.com:example/private-discourse.git") {
		t.Fatal("expected different remotes not to match")
	}
}

func TestShouldRecloneLocalRepoWhenOriginDiffers(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	gitInit(t, dir)
	runGit(t, dir, "remote", "add", "origin", "git@github.com:discourse/discourse.git")

	if !shouldRecloneLocalRepo(dir, "git@github.com:example/private-discourse.git") {
		t.Fatal("expected reclone when origin points at a different fork")
	}
}

func TestShouldNotRecloneLocalRepoWhenOriginMatches(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	gitInit(t, dir)
	runGit(t, dir, "remote", "add", "origin", "git@github.com:example/private-discourse.git")

	if shouldRecloneLocalRepo(dir, "https://github.com/example/private-discourse.git") {
		t.Fatal("expected no reclone for equivalent SSH/HTTPS remotes")
	}
}

func TestMoveAsideLocalRepoPreservesMismatchedDirectory(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "private-discourse_src")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "important.txt")
	if err := os.WriteFile(marker, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}

	backup, err := moveAsideLocalRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("expected original path moved away, stat err=%v", err)
	}
	if got, err := os.ReadFile(filepath.Join(backup, "important.txt")); err != nil || string(got) != "keep me" {
		t.Fatalf("backup did not preserve marker: got %q err=%v", got, err)
	}
}

func TestShouldRecloneLocalRepoForNonGitDestination(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "discourse_src")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	if !shouldRecloneLocalRepo(dir, "git@github.com:example/private-discourse.git") {
		t.Fatal("expected reclone for existing non-git destination")
	}
}
