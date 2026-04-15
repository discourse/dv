package cli

import (
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"

	"dv/internal/docker"
)

var extractCopyFromContainer = docker.CopyFromContainer

func applyExtractStatus(logOut io.Writer, containerName, containerWorkdir, localRepo, statusOut string) (int, error) {
	changes := buildTrackedChanges(parseStatusOutput(statusOut))
	changedCount := 0
	for _, change := range changes {
		changedCount++
		if change.kind == changeRename && change.oldPath != "" {
			oldDst := filepath.Join(localRepo, filepath.FromSlash(change.oldPath))
			if err := os.RemoveAll(oldDst); err != nil && !os.IsNotExist(err) {
				return changedCount, fmt.Errorf("remove renamed path %s: %w", change.oldPath, err)
			}
		}

		switch change.kind {
		case changeDelete:
			absDst := filepath.Join(localRepo, filepath.FromSlash(change.path))
			if err := os.RemoveAll(absDst); err != nil && !os.IsNotExist(err) {
				return changedCount, fmt.Errorf("remove deleted path %s: %w", change.path, err)
			}
		case changeModify, changeRename:
			absDst := filepath.Join(localRepo, filepath.FromSlash(change.path))
			if err := os.MkdirAll(filepath.Dir(absDst), 0o755); err != nil {
				return changedCount, err
			}
			if err := extractCopyFromContainer(containerName, path.Join(containerWorkdir, change.path), absDst); err != nil {
				fmt.Fprintf(logOut, "Warning: could not copy %s\n", change.path)
			}
		}
	}
	return changedCount, nil
}
