package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"dv/internal/xdg"
)

type syncRecord struct {
	PID              int    `json:"pid"`
	ContainerName    string `json:"container_name"`
	ContainerWorkdir string `json:"container_workdir"`
	LocalRepo        string `json:"local_repo"`
	StartedAt        string `json:"started_at"`
	Path             string `json:"-"`
}

func registerExtractSync(cmd *cobra.Command, opts syncOptions) (func(), error) {
	stateDir, err := syncStateDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, err
	}

	localRepo := normalizeLocalRepo(opts.localRepo)
	recordPath := syncRecordPath(stateDir, localRepo)

	records, invalidPaths, err := readSyncRecords(stateDir)
	if err != nil {
		return nil, err
	}
	for _, stalePath := range invalidPaths {
		_ = os.Remove(stalePath)
	}

	active := make([]syncRecord, 0, len(records))
	for _, record := range records {
		if record.PID == os.Getpid() {
			_ = os.Remove(record.Path)
			continue
		}
		if isProcessRunning(record.PID) {
			active = append(active, record)
		} else {
			_ = os.Remove(record.Path)
		}
	}

	var sameRepo *syncRecord
	var others []syncRecord
	for i := range active {
		record := &active[i]
		if sameLocalRepo(record.LocalRepo, localRepo) {
			sameRepo = record
		} else {
			others = append(others, *record)
		}
	}

	if sameRepo != nil {
		fmt.Fprintf(opts.errOut, "⚠️  extract --sync already running for %s\n", localRepo)
		fmt.Fprintf(opts.errOut, "   pid %d, container %s, workdir %s\n", sameRepo.PID, sameRepo.ContainerName, sameRepo.ContainerWorkdir)
		stop, err := promptYesNo(cmd.InOrStdin(), opts.errOut, "Stop it now? (y/N): ")
		if err != nil {
			return nil, err
		}
		if !stop {
			return nil, fmt.Errorf("sync already running for %s (pid %d)", localRepo, sameRepo.PID)
		}
		if err := terminateSyncProcess(*sameRepo, opts.errOut); err != nil {
			return nil, err
		}
		_ = os.Remove(sameRepo.Path)
	}

	if len(others) > 0 {
		fmt.Fprintln(opts.errOut, "⚠️  Another extract --sync is already running:")
		for _, record := range others {
			fmt.Fprintf(opts.errOut, "   pid %d: %s (%s) -> %s\n", record.PID, record.ContainerName, record.ContainerWorkdir, record.LocalRepo)
		}
		stop, err := promptYesNo(cmd.InOrStdin(), opts.errOut, "Stop the running sync(s) before continuing? (y/N): ")
		if err != nil {
			return nil, err
		}
		if stop {
			for _, record := range others {
				if err := terminateSyncProcess(record, opts.errOut); err != nil {
					return nil, err
				}
				_ = os.Remove(record.Path)
			}
		}
	}

	record := syncRecord{
		PID:              os.Getpid(),
		ContainerName:    opts.containerName,
		ContainerWorkdir: opts.containerWorkdir,
		LocalRepo:        localRepo,
		StartedAt:        time.Now().UTC().Format(time.RFC3339),
	}
	if err := writeSyncRecord(recordPath, record); err != nil {
		return nil, err
	}

	return func() {
		_ = os.Remove(recordPath)
	}, nil
}

func syncStateDir() (string, error) {
	dataDir, err := xdg.DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dataDir, "extract_sync"), nil
}

func syncRecordPath(stateDir, localRepo string) string {
	sum := sha256.Sum256([]byte(localRepo))
	return filepath.Join(stateDir, hex.EncodeToString(sum[:])+".json")
}

func normalizeLocalRepo(repoPath string) string {
	repoPath = strings.TrimSpace(repoPath)
	if repoPath == "" {
		return repoPath
	}
	if abs, err := filepath.Abs(repoPath); err == nil {
		repoPath = abs
	}
	return filepath.Clean(repoPath)
}

func sameLocalRepo(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return normalizeLocalRepo(a) == normalizeLocalRepo(b)
}

func readSyncRecords(stateDir string) ([]syncRecord, []string, error) {
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}

	var records []syncRecord
	var invalid []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		p := filepath.Join(stateDir, entry.Name())
		record, err := readSyncRecord(p)
		if err != nil {
			invalid = append(invalid, p)
			continue
		}
		records = append(records, record)
	}

	return records, invalid, nil
}

func readSyncRecord(path string) (syncRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return syncRecord{}, err
	}
	var record syncRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return syncRecord{}, err
	}
	record.Path = path
	record.LocalRepo = normalizeLocalRepo(record.LocalRepo)
	return record, nil
}

func writeSyncRecord(path string, record syncRecord) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		existing, readErr := readSyncRecord(path)
		if readErr == nil && isProcessRunning(existing.PID) {
			return fmt.Errorf("sync already running for %s (pid %d)", existing.LocalRepo, existing.PID)
		}
		_ = os.Remove(path)
		f, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return err
		}
	}
	encoder := json.NewEncoder(f)
	if err := encoder.Encode(record); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

func isProcessRunning(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, syscall.Signal(0))
	if err == nil {
		return true
	}
	if errors.Is(err, syscall.EPERM) {
		return true
	}
	return false
}

func terminateSyncProcess(record syncRecord, out io.Writer) error {
	if record.PID <= 0 {
		return fmt.Errorf("invalid pid %d", record.PID)
	}
	if err := syscall.Kill(record.PID, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return fmt.Errorf("failed to signal pid %d: %w", record.PID, err)
	}
	fmt.Fprintf(out, "Sent SIGTERM to pid %d\n", record.PID)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !isProcessRunning(record.PID) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("process %d is still running; stop it and retry", record.PID)
}

func promptYesNo(in io.Reader, out io.Writer, prompt string) (bool, error) {
	if prompt != "" {
		fmt.Fprint(out, prompt)
	}
	var response string
	_, err := fmt.Fscanln(in, &response)
	if errors.Is(err, io.EOF) {
		return false, fmt.Errorf("confirmation required but stdin is not interactive: %w", err)
	}
	response = strings.TrimSpace(strings.ToLower(response))
	return response == "y" || response == "yes", nil
}
