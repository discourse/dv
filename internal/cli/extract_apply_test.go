package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestApplyExtractStatusHandlesRename(t *testing.T) {
	containerDir := t.TempDir()
	localRepo := t.TempDir()

	oldPath := filepath.Join(localRepo, "old.txt")
	if err := os.WriteFile(oldPath, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	newSrc := filepath.Join(containerDir, "new.txt")
	if err := os.WriteFile(newSrc, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}

	origCopy := extractCopyFromContainer
	t.Cleanup(func() {
		extractCopyFromContainer = origCopy
	})
	extractCopyFromContainer = func(_ string, srcInContainer, dstOnHost string) error {
		return copyFile(srcInContainer, dstOnHost)
	}

	var logBuf bytes.Buffer
	changedCount, err := applyExtractStatus(&logBuf, "fake-container", containerDir, localRepo, "R  new.txt\x00old.txt\x00")
	if err != nil {
		t.Fatalf("applyExtractStatus failed: %v", err)
	}
	if changedCount != 1 {
		t.Fatalf("changedCount = %d, want 1", changedCount)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("old path still exists after rename, err=%v", err)
	}
	newDst := filepath.Join(localRepo, "new.txt")
	contents, err := os.ReadFile(newDst)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "new" {
		t.Fatalf("new file contents = %q, want %q", string(contents), "new")
	}
}

func TestParseStatusOutputPreservesSpacesAndRenameMarkers(t *testing.T) {
	newPath := " new  -> target "
	oldPath := "old -> name "
	spacedPath := " leading and trailing "
	out := "R  " + newPath + "\x00" + oldPath + "\x00?? " + spacedPath + "\x00"

	entries := parseStatusOutput(out)
	if len(entries) != 2 {
		t.Fatalf("len(entries) = %d, want 2", len(entries))
	}

	if entries[0].staged != 'R' || entries[0].unstaged != ' ' {
		t.Fatalf("rename entry status = %q%q, want R<space>", entries[0].staged, entries[0].unstaged)
	}
	if entries[0].path != newPath {
		t.Fatalf("rename new path = %q, want %q", entries[0].path, newPath)
	}
	if entries[0].oldPath != oldPath {
		t.Fatalf("rename old path = %q, want %q", entries[0].oldPath, oldPath)
	}

	if entries[1].staged != '?' || entries[1].unstaged != '?' {
		t.Fatalf("untracked entry status = %q%q, want ??", entries[1].staged, entries[1].unstaged)
	}
	if entries[1].path != spacedPath {
		t.Fatalf("untracked path = %q, want %q", entries[1].path, spacedPath)
	}
}
