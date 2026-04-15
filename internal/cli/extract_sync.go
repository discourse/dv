package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"dv/internal/docker"
)

type syncOptions struct {
	containerName    string
	containerWorkdir string
	localRepo        string
	logOut           io.Writer
	errOut           io.Writer
	debug            bool
}

type gitSyncerIface interface {
	syncToContainer() error
	forceSyncToContainer() error
}

type changeSource int

const (
	sourceHost changeSource = iota
	sourceContainer
)

type watcherEvent struct {
	source changeSource
	path   string
}

type changeKind int

const (
	changeModify changeKind = iota
	changeDelete
	changeRename
)

type trackedChange struct {
	kind    changeKind
	path    string
	oldPath string
}

type statusEntry struct {
	staged   rune
	unstaged rune
	path     string
	oldPath  string
}

type retryEntry struct {
	source   changeSource
	attempts int
}

type extractSync struct {
	ctx           context.Context
	cancel        context.CancelFunc
	containerName string
	workdir       string
	localRepo     string
	logOut        io.Writer
	errOut        io.Writer
	debug         bool
	events        chan watcherEvent
	retryQueue    map[string]retryEntry

	// Git sync integration
	gitSyncer      gitSyncerIface
	gitEvents      chan struct{}
	fileSyncPaused atomic.Bool
	fileSyncIdle   chan struct{} // Closed when file sync is idle
	fileSyncIdleMu sync.Mutex    // Protects fileSyncIdle channel
	gitSyncPending int32         // Atomic flag: 1 if git sync is pending, 0 otherwise
	retryQueueMu   sync.Mutex    // Protects retryQueue
}

var errSyncSkipped = errors.New("sync skipped")
var errTransient = errors.New("transient error")

const maxRetryAttempts = 3

var dockerExecOutput = docker.ExecOutputContext
var dockerExecAsRoot = docker.ExecAsRootContext
var dockerCopyFromContainer = docker.CopyFromContainerContext
var dockerCopyToContainerWithOwnership = docker.CopyToContainerWithOwnershipContext

func isTransientError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return os.IsPermission(err) ||
		strings.Contains(msg, "Permission denied") ||
		strings.Contains(msg, "text file busy")
}

func runExtractSync(cmd *cobra.Command, opts syncOptions) error {
	ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	s := &extractSync{
		ctx:           ctx,
		cancel:        cancel,
		containerName: opts.containerName,
		workdir:       opts.containerWorkdir,
		localRepo:     opts.localRepo,
		logOut:        opts.logOut,
		errOut:        opts.errOut,
		debug:         opts.debug,
		events:        make(chan watcherEvent, 256),
		retryQueue:    make(map[string]retryEntry),
		gitEvents:     make(chan struct{}, 1),
		fileSyncIdle:  make(chan struct{}),
	}
	defer cancel()

	// Initialize git syncer
	s.gitSyncer = newGitSyncer(ctx, opts.containerName, opts.containerWorkdir, opts.localRepo, opts.logOut, opts.errOut, opts.debug)

	if err := s.ensureInotify(); err != nil {
		return err
	}

	if err := s.run(); err != nil {
		return err
	}
	fmt.Fprintln(s.logOut, "✅ Sync stopped")
	return nil
}

func (s *extractSync) run() error {
	g, ctx := errgroup.WithContext(s.ctx)
	s.ctx = ctx

	s.debugf("starting goroutines...")
	g.Go(func() error {
		s.debugf("runHostWatcher starting")
		return s.runHostWatcher()
	})
	g.Go(func() error {
		s.debugf("runContainerWatcher starting")
		return s.runContainerWatcher()
	})
	g.Go(func() error {
		s.debugf("processEvents starting")
		return s.processEvents()
	})
	g.Go(func() error {
		s.debugf("runGitWatcher starting")
		return s.runGitWatcher()
	})
	g.Go(func() error {
		s.debugf("processGitEvents starting")
		return s.processGitEvents()
	})

	if err := g.Wait(); err != nil {
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	return nil
}

func (s *extractSync) runHostWatcher() error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()

	if err := s.addHostWatchers(watcher, s.localRepo); err != nil {
		return err
	}

	for {
		select {
		case <-s.ctx.Done():
			return nil
		case err := <-watcher.Errors:
			if err != nil {
				return err
			}
		case event := <-watcher.Events:
			if event.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename) == 0 {
				continue
			}
			rel, ok := s.relativeFromLocal(event.Name)
			if !ok {
				continue
			}
			if rel == "" || rel == "." || shouldIgnoreRelative(rel) {
				continue
			}
			if event.Op&fsnotify.Create != 0 {
				// If a directory is created, watch it recursively
				info, err := os.Stat(event.Name)
				if err == nil && info.IsDir() {
					_ = s.addHostWatchers(watcher, event.Name)
					continue
				}
			}
			s.queueEvent(watcherEvent{source: sourceHost, path: rel})
		}
	}
}

func (s *extractSync) runContainerWatcher() error {
	args := []string{"exec", "--user", "discourse", "-w", s.workdir, s.containerName,
		"inotifywait", "-m", "-r",
		"-e", "modify", "-e", "create", "-e", "delete", "-e", "move",
		"--format", "%w%f|%e", "--exclude", "(^|/)\\.git(/|$)", "."}
	cmd := exec.CommandContext(s.ctx, "docker", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderrBuf strings.Builder
	cmd.Stderr = io.MultiWriter(s.errOut, &stderrBuf)
	if err := cmd.Start(); err != nil {
		return err
	}

	// Ensure cleanup happens when context is cancelled
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-s.ctx.Done():
			// Force kill the process immediately
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		case <-done:
		}
	}()

	// Read lines in a separate goroutine to avoid blocking on scanner.Scan()
	lines := make(chan string, 100)
	scanErr := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			select {
			case <-s.ctx.Done():
				return
			case lines <- scanner.Text():
			}
		}
		scanErr <- scanner.Err()
		close(lines)
	}()

	// Process lines until context is cancelled or scanner finishes
	for {
		select {
		case <-s.ctx.Done():
			// Give the process a moment to exit cleanly
			waitDone := make(chan error, 1)
			go func() {
				waitDone <- cmd.Wait()
			}()
			select {
			case <-time.After(100 * time.Millisecond):
				// Timeout waiting, force kill again just to be sure
				if cmd.Process != nil {
					_ = cmd.Process.Kill()
				}
			case <-waitDone:
			}
			return nil
		case line, ok := <-lines:
			if !ok {
				// Scanner finished, check for errors
				if err := <-scanErr; err != nil {
					if s.ctx.Err() != nil {
						return nil
					}
					msg := strings.TrimSpace(stderrBuf.String())
					if msg != "" {
						return fmt.Errorf("container watcher stream error: %w: %s", err, msg)
					}
					return fmt.Errorf("container watcher stream error: %w", err)
				}
				if err := cmd.Wait(); err != nil {
					if s.ctx.Err() != nil {
						return nil
					}
					msg := strings.TrimSpace(stderrBuf.String())
					if msg != "" {
						return fmt.Errorf("container watcher exited: %w: %s", err, msg)
					}
					return fmt.Errorf("container watcher exited: %w", err)
				}
				return nil
			}

			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			absPath, ok := parseInotifyLine(line)
			if !ok {
				s.debugf("ignoring unrecognized inotify line: %s", line)
				continue
			}
			if !path.IsAbs(absPath) {
				absPath = path.Clean(path.Join(s.workdir, absPath))
			}
			rel, ok := s.relativeFromContainer(absPath)
			if !ok || rel == "" || rel == "." || shouldIgnoreRelative(rel) {
				s.debugf("ignoring container event outside workdir: abs=%s rel=%s", absPath, rel)
				continue
			}
			// Directory events do not need to be queued; file events will arrive as children are modified.
			s.debugf("queueing container event: abs=%s rel=%s", absPath, rel)
			s.queueEvent(watcherEvent{source: sourceContainer, path: rel})
		}
	}
}

func (s *extractSync) processEvents() error {
	const settleDelay = 250 * time.Millisecond
	const flushTimeout = 30 * time.Second // Timeout for docker operations during flush
	hostPaths := make(map[string]struct{})
	containerPaths := make(map[string]struct{})
	timer := time.NewTimer(settleDelay)
	if !timer.Stop() {
		<-timer.C
	}
	timerActive := false

	// flushWithTimeout runs flush in a goroutine with timeout protection.
	// Returns nil on timeout (logs warning), error on failure, nil on success.
	flushWithTimeout := func(timeout time.Duration) error {
		// Check context before starting
		select {
		case <-s.ctx.Done():
			return nil
		default:
		}

		// Merge retry queue into paths to process
		for path, entry := range s.retrySnapshot() {
			if entry.source == sourceHost {
				hostPaths[path] = struct{}{}
			} else {
				containerPaths[path] = struct{}{}
			}
		}

		// Collect paths to process
		hostToProcess := mapKeys(hostPaths)
		containerToProcess := mapKeys(containerPaths)
		hostPaths = make(map[string]struct{})
		containerPaths = make(map[string]struct{})

		if len(hostToProcess) == 0 && len(containerToProcess) == 0 {
			return nil
		}

		flushCtx, flushCancel := context.WithCancel(s.ctx)
		defer flushCancel()

		flushDone := make(chan error, 1)
		go func() {
			var err error
			if len(hostToProcess) > 0 {
				if err = s.processHostChanges(flushCtx, hostToProcess); err != nil {
					flushDone <- err
					return
				}
			}
			if len(containerToProcess) > 0 {
				if err = s.processContainerChanges(flushCtx, containerToProcess); err != nil {
					flushDone <- err
					return
				}
			}
			flushDone <- nil
		}()

		// Use a ticker to periodically check if git sync became pending
		// If flush is taking > 1 second and git sync is pending, abort early
		gitCheckTicker := time.NewTicker(1 * time.Second)
		defer gitCheckTicker.Stop()
		timeoutTimer := time.NewTimer(timeout)
		defer timeoutTimer.Stop()

		waitForFlush := func(reason string) {
			select {
			case <-flushDone:
			case <-time.After(2 * time.Second):
				s.debugf("flush did not stop after %s", reason)
			}
		}

		for {
			select {
			case err := <-flushDone:
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return nil
				}
				return err
			case <-s.ctx.Done():
				// Context cancelled while flushing - wait briefly for clean exit
				flushCancel()
				waitForFlush("context cancellation")
				return nil
			case <-gitCheckTicker.C:
				// Check if git sync became pending - if so, abort flush early
				// Git operations take priority since file events may be stale
				if atomic.LoadInt32(&s.gitSyncPending) == 1 {
					s.debugf("flush aborted: git sync pending (flush taking too long)")
					flushCancel()
					waitForFlush("git sync pending")
					return nil
				}
			case <-timeoutTimer.C:
				s.debugf("flush timed out after %v - possible docker hang", timeout)
				flushCancel()
				waitForFlush("timeout")
				return nil
			}
		}
	}

	// Ticker to periodically signal idle while git sync is pending
	// This ensures waitForFileSyncIdle can receive the signal even if
	// it starts after the initial signal was sent
	idleTicker := time.NewTicker(25 * time.Millisecond)
	defer idleTicker.Stop()

	for {
		// Signal idle state when no pending events and timer not active,
		// OR when git sync is pending (we're deferring to git sync)
		isIdle := !timerActive && len(hostPaths) == 0 && len(containerPaths) == 0
		gitPending := atomic.LoadInt32(&s.gitSyncPending) == 1
		if isIdle || gitPending {
			s.signalFileSyncIdle()
		}

		select {
		case <-s.ctx.Done():
			if timerActive {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
			}
			// Final flush with short timeout on cleanup
			_ = flushWithTimeout(2 * time.Second)
			return nil
		case <-idleTicker.C:
			// Periodically signal idle while git sync is pending
			// This handles the case where waitForFileSyncIdle starts
			// after we already signaled (and recreated the channel)
			if atomic.LoadInt32(&s.gitSyncPending) == 1 {
				s.signalFileSyncIdle()
			}
		case event := <-s.events:
			if event.source == sourceHost {
				hostPaths[event.path] = struct{}{}
			} else {
				containerPaths[event.path] = struct{}{}
			}
			if timerActive {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
			}
			timer.Reset(settleDelay)
			timerActive = true
		case <-timer.C:
			timerActive = false
			// Skip file sync if paused for git sync
			if s.fileSyncPaused.Load() {
				s.debugf("file sync paused, skipping flush")
				continue
			}
			// Skip file sync if git sync is pending - the file events are likely
			// stale due to a git state change (reset, checkout, etc.)
			// Clear the pending paths and signal idle so git sync can proceed.
			if atomic.LoadInt32(&s.gitSyncPending) == 1 {
				s.debugf("git sync pending, deferring file sync (clearing %d host, %d container paths)",
					len(hostPaths), len(containerPaths))
				hostPaths = make(map[string]struct{})
				containerPaths = make(map[string]struct{})
				s.signalFileSyncIdle()
				continue
			}
			if err := flushWithTimeout(flushTimeout); err != nil {
				return err
			}
		}
	}
}

func (s *extractSync) processHostChanges(ctx context.Context, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Check if git sync became pending while we were queued
	if atomic.LoadInt32(&s.gitSyncPending) == 1 {
		s.debugf("git sync pending, aborting host changes processing")
		return nil
	}
	s.debugf("host events: %s", strings.Join(paths, ", "))

	// Ask git about these paths - it will filter out gitignored files
	entries, err := gitStatusPorcelainHost(ctx, s.localRepo, paths)
	if err != nil {
		return err
	}

	// Track which paths git reported as changed
	gitReported := make(map[string]bool)

	changes := buildTrackedChanges(entries)
	for i, change := range changes {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Check mid-batch if git sync needs priority
		if atomic.LoadInt32(&s.gitSyncPending) == 1 {
			s.debugf("git sync pending, aborting host changes mid-batch (%d/%d processed)", i, len(changes))
			return nil
		}

		gitReported[change.path] = true
		if change.oldPath != "" {
			gitReported[change.oldPath] = true
		}

		if change.kind == changeRename && change.oldPath != "" {
			if shouldIgnoreRelative(change.oldPath) {
				continue
			}
			if err := s.removeInContainer(ctx, change.oldPath); err != nil {
				return err
			}
			fmt.Fprintf(s.logOut, "host → container: removed %s\n", change.oldPath)
		}
		switch change.kind {
		case changeDelete:
			if err := s.removeInContainer(ctx, change.path); err != nil {
				return err
			}
			fmt.Fprintf(s.logOut, "host → container: removed %s\n", change.path)
		case changeModify, changeRename:
			same, err := s.hashesMatch(ctx, change.path)
			if err != nil {
				if errors.Is(err, errTransient) {
					s.queueRetry(change.path, sourceHost)
					continue
				}
				return err
			}
			if same {
				s.debugf("host path %s already synchronized", change.path)
				s.deleteRetry(change.path)
				continue
			}
			if err := s.copyHostToContainer(ctx, change.path); err != nil {
				if errors.Is(err, errSyncSkipped) {
					s.debugf("skipping host → container copy for %s (file vanished)", change.path)
					s.deleteRetry(change.path)
					continue
				}
				if errors.Is(err, errTransient) {
					s.queueRetry(change.path, sourceHost)
					continue
				}
				return err
			}
			s.deleteRetry(change.path)
			fmt.Fprintf(s.logOut, "host → container: updated %s\n", change.path)
		}
	}

	// For paths the watcher reported but git didn't, check if they need sync
	for i, rel := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Check mid-batch if git sync needs priority
		if atomic.LoadInt32(&s.gitSyncPending) == 1 {
			s.debugf("git sync pending, aborting host path check mid-batch (%d/%d processed)", i, len(paths))
			return nil
		}

		if gitReported[rel] || shouldIgnoreRelative(rel) {
			continue
		}

		// Check if file exists on host
		hostPath := filepath.Join(s.localRepo, filepath.FromSlash(rel))
		_, err := os.Stat(hostPath)
		hostExists := err == nil

		if !hostExists {
			// File was deleted on host, remove from container if it exists there
			// This handles both tracked and untracked file deletions
			checkCmd := []string{"bash", "-lc", fmt.Sprintf("test -e %s && echo exists", shellQuote(rel))}
			out, _ := dockerExecOutput(ctx, s.containerName, s.workdir, nil, checkCmd)
			if strings.Contains(out, "exists") {
				// Check if file is gitignored - don't sync gitignored files
				ignored, _ := s.isGitIgnored(ctx, s.localRepo, rel)
				if ignored {
					s.debugf("skipping deletion of %s (gitignored)", rel)
					continue
				}

				if err := s.removeInContainer(ctx, rel); err != nil {
					s.debugf("remove failed for %s: %v", rel, err)
					continue
				}
				fmt.Fprintf(s.logOut, "host → container: removed %s\n", rel)
			}
			continue
		}

		// File exists, check if this file is tracked by git (not gitignored)
		tracked, err := s.isTrackedByGit(ctx, s.localRepo, rel)
		if err != nil || !tracked {
			s.debugf("skipping %s (not tracked by git)", rel)
			continue
		}

		// File is tracked but git status didn't report it (it's clean)
		// Check if it actually differs from container (e.g., after git reset)
		same, err := s.hashesMatch(ctx, rel)
		if err != nil {
			if errors.Is(err, errTransient) {
				s.queueRetry(rel, sourceHost)
				continue
			}
			s.debugf("hash check failed for %s: %v", rel, err)
			continue
		}
		if !same {
			if err := s.copyHostToContainer(ctx, rel); err != nil {
				if errors.Is(err, errSyncSkipped) {
					s.debugf("skipping host → container copy for %s (file vanished)", rel)
					s.deleteRetry(rel)
					continue
				}
				if errors.Is(err, errTransient) {
					s.queueRetry(rel, sourceHost)
					continue
				}
				s.debugf("copy failed for %s: %v", rel, err)
				continue
			}
			s.deleteRetry(rel)
			fmt.Fprintf(s.logOut, "host → container: updated %s\n", rel)
		} else {
			s.debugf("host path %s already synchronized", rel)
			s.deleteRetry(rel)
		}
	}

	return nil
}

func (s *extractSync) processContainerChanges(ctx context.Context, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Check if git sync became pending while we were queued
	if atomic.LoadInt32(&s.gitSyncPending) == 1 {
		s.debugf("git sync pending, aborting container changes processing")
		return nil
	}
	s.debugf("container events: %s", strings.Join(paths, ", "))

	// Ask git about these paths - it will filter out gitignored files
	entries, err := gitStatusPorcelainContainer(ctx, s.containerName, s.workdir, paths)
	if err != nil {
		return err
	}

	// Track which paths git reported as changed
	gitReported := make(map[string]bool)

	changes := buildTrackedChanges(entries)
	for i, change := range changes {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Check mid-batch if git sync needs priority
		if atomic.LoadInt32(&s.gitSyncPending) == 1 {
			s.debugf("git sync pending, aborting container changes mid-batch (%d/%d processed)", i, len(changes))
			return nil
		}

		gitReported[change.path] = true
		if change.oldPath != "" {
			gitReported[change.oldPath] = true
		}

		if change.kind == changeRename && change.oldPath != "" {
			if shouldIgnoreRelative(change.oldPath) {
				continue
			}
			if err := s.removeOnHost(change.oldPath); err != nil {
				return err
			}
			fmt.Fprintf(s.logOut, "container → host: removed %s\n", change.oldPath)
		}
		switch change.kind {
		case changeDelete:
			if err := s.removeOnHost(change.path); err != nil {
				return err
			}
			fmt.Fprintf(s.logOut, "container → host: removed %s\n", change.path)
		case changeModify, changeRename:
			same, err := s.hashesMatch(ctx, change.path)
			if err != nil {
				if errors.Is(err, errTransient) {
					s.queueRetry(change.path, sourceContainer)
					continue
				}
				return err
			}
			if same {
				s.debugf("container path %s already synchronized", change.path)
				s.deleteRetry(change.path)
				continue
			}
			if err := s.copyContainerToHost(ctx, change.path); err != nil {
				if errors.Is(err, errSyncSkipped) {
					s.debugf("skipping container → host copy for %s (file vanished)", change.path)
					s.deleteRetry(change.path)
					continue
				}
				if errors.Is(err, errTransient) {
					s.queueRetry(change.path, sourceContainer)
					continue
				}
				return err
			}
			s.deleteRetry(change.path)
			fmt.Fprintf(s.logOut, "container → host: updated %s\n", change.path)
		}
	}

	// For paths the watcher reported but git didn't, check if they need sync
	for i, rel := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Check mid-batch if git sync needs priority
		if atomic.LoadInt32(&s.gitSyncPending) == 1 {
			s.debugf("git sync pending, aborting container path check mid-batch (%d/%d processed)", i, len(paths))
			return nil
		}

		if gitReported[rel] || shouldIgnoreRelative(rel) {
			continue
		}

		// Check if file exists in container
		checkCmd := []string{"bash", "-lc", fmt.Sprintf("test -e %s && echo exists", shellQuote(rel))}
		out, _ := dockerExecOutput(ctx, s.containerName, s.workdir, nil, checkCmd)
		containerExists := strings.Contains(out, "exists")

		if !containerExists {
			// File was deleted in container, remove from host if it exists there
			// This handles both tracked and untracked file deletions
			hostPath := filepath.Join(s.localRepo, filepath.FromSlash(rel))
			if _, err := os.Stat(hostPath); err == nil {
				// Check if file is gitignored - don't sync gitignored files
				ignored, _ := s.isGitIgnoredInContainer(ctx, rel)
				if ignored {
					s.debugf("skipping deletion of %s (gitignored in container)", rel)
					continue
				}

				if err := s.removeOnHost(rel); err != nil {
					s.debugf("remove failed for %s: %v", rel, err)
					continue
				}
				fmt.Fprintf(s.logOut, "container → host: removed %s\n", rel)
			}
			continue
		}

		// File exists, check if this file is tracked by git in container (not gitignored)
		tracked, err := s.isTrackedByGitInContainer(ctx, rel)
		if err != nil || !tracked {
			s.debugf("skipping %s (not tracked by git in container)", rel)
			continue
		}

		// File is tracked but git status didn't report it (it's clean)
		// Check if it actually differs from host (e.g., after git reset)
		same, err := s.hashesMatch(ctx, rel)
		if err != nil {
			if errors.Is(err, errTransient) {
				s.queueRetry(rel, sourceContainer)
				continue
			}
			s.debugf("hash check failed for %s: %v", rel, err)
			continue
		}
		if !same {
			if err := s.copyContainerToHost(ctx, rel); err != nil {
				if errors.Is(err, errSyncSkipped) {
					s.debugf("skipping container → host copy for %s (file vanished)", rel)
					s.deleteRetry(rel)
					continue
				}
				if errors.Is(err, errTransient) {
					s.queueRetry(rel, sourceContainer)
					continue
				}
				s.debugf("copy failed for %s: %v", rel, err)
				continue
			}
			s.deleteRetry(rel)
			fmt.Fprintf(s.logOut, "container → host: updated %s\n", rel)
		} else {
			s.debugf("container path %s already synchronized", rel)
			s.deleteRetry(rel)
		}
	}

	return nil
}

// runGitWatcher watches for git state changes on host (.git/HEAD and refs)
func (s *extractSync) runGitWatcher() error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()

	gitDir := filepath.Join(s.localRepo, ".git")

	// Watch HEAD file (changes on checkout, reset)
	headPath := filepath.Join(gitDir, "HEAD")
	if err := watcher.Add(headPath); err != nil {
		s.debugf("could not watch .git/HEAD: %v", err)
	}

	// Watch logs/HEAD - this is the most reliable way to detect commits,
	// as it gets appended to on every commit, merge, checkout, reset, etc.
	logsHead := filepath.Join(gitDir, "logs", "HEAD")
	if err := watcher.Add(logsHead); err != nil {
		s.debugf("could not watch .git/logs/HEAD: %v", err)
	}

	// Watch refs/heads directory (changes on commit, branch create/delete)
	refsHeads := filepath.Join(gitDir, "refs", "heads")
	if err := watcher.Add(refsHeads); err != nil {
		// refs/heads might not exist yet in a fresh repo
		s.debugf("could not watch refs/heads: %v", err)
	}

	// Also watch packed-refs for packed references
	packedRefs := filepath.Join(gitDir, "packed-refs")
	_ = watcher.Add(packedRefs) // Ignore error if doesn't exist

	for {
		select {
		case <-s.ctx.Done():
			return nil
		case err := <-watcher.Errors:
			if err != nil {
				s.debugf("git watcher error: %v", err)
			}
		case event := <-watcher.Events:
			if event.Op&(fsnotify.Write|fsnotify.Create) == 0 {
				continue
			}
			s.debugf("git state change detected: %s (%s)", event.Name, event.Op)
			// IMMEDIATELY mark git sync as pending - this prevents file sync
			// from processing stale events while we wait for debounce
			atomic.StoreInt32(&s.gitSyncPending, 1)
			// Signal git sync needed (non-blocking)
			select {
			case s.gitEvents <- struct{}{}:
				s.debugf("git event signal sent to processGitEvents")
			default:
				s.debugf("git event signal dropped (already pending)")
			}
		}
	}
}

// processGitEvents handles git sync events with debouncing
func (s *extractSync) processGitEvents() error {
	const gitSyncDelay = 500 * time.Millisecond
	s.debugf("processGitEvents: entering event loop")

	timer := time.NewTimer(gitSyncDelay)
	if !timer.Stop() {
		<-timer.C
	}
	timerActive := false

	for {
		select {
		case <-s.ctx.Done():
			if timerActive {
				timer.Stop()
			}
			return nil
		case <-s.gitEvents:
			s.debugf("git event received, starting %v debounce", gitSyncDelay)
			if timerActive {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
			}
			timer.Reset(gitSyncDelay)
			timerActive = true
		case <-timer.C:
			timerActive = false
			s.debugf("git sync debounce complete, starting sync")
			if err := s.performGitSync(); err != nil {
				fmt.Fprintf(s.errOut, "git sync error: %v\n", err)
			}
		}
	}
}

// performGitSync pauses file sync and performs git state synchronization
func (s *extractSync) performGitSync() error {
	s.debugf("performGitSync: waiting for file sync idle")
	// Wait for file sync to become idle
	s.waitForFileSyncIdle()
	s.debugf("performGitSync: file sync is idle, pausing")

	// Pause file sync during git operations
	s.fileSyncPaused.Store(true)
	defer func() {
		s.fileSyncPaused.Store(false)
		// Clear the pending flag - git sync is complete
		atomic.StoreInt32(&s.gitSyncPending, 0)
		s.debugf("performGitSync: complete, file sync resumed")
	}()

	preferHostReset := s.shouldPreferHostReset(s.ctx)
	if preferHostReset {
		fmt.Fprintln(s.logOut, "git sync: host reset detected; discarding container working-tree changes")
	}

	// Auto-sync container working tree changes to host before git sync unless the
	// host just performed a reset that intentionally discarded local changes.
	containerChanges, err := s.collectContainerChanges(s.ctx)
	if err != nil {
		return err
	}
	if !preferHostReset && len(containerChanges) > 0 {
		fmt.Fprintf(s.logOut, "git sync: container has %d uncommitted change(s), syncing to host\n", len(containerChanges))
		if err := s.applyContainerChangesToHost(s.ctx, containerChanges); err != nil {
			return err
		}
	}

	// Re-scan host changes after container → host sync so we preserve any
	// host-local edits that may have been waiting in the file-event queue when
	// git sync was triggered. This also captures the freshly copied container
	// changes so we can restore the full dirty working tree after git checkout
	// or reset operations inside the container.
	hostChanges, err := s.collectHostChanges(s.ctx)
	if err != nil {
		return err
	}
	if len(hostChanges) > 0 {
		fmt.Fprintf(s.logOut, "git sync: preserving %d uncommitted host change(s) across git sync\n", len(hostChanges))
	}

	// Drain queued file events - they're now stale since git state is changing.
	// After git sync completes, the working trees will match and any real
	// differences will generate fresh events.
	s.drainEventQueue()

	s.debugf("performGitSync: calling syncToContainer")
	if preferHostReset {
		if err := s.gitSyncer.forceSyncToContainer(); err != nil {
			return err
		}
	} else {
		if err := s.gitSyncer.syncToContainer(); err != nil {
			return err
		}
	}

	// Reapply host working-tree changes after git sync resets the container.
	if len(hostChanges) > 0 {
		if err := s.applyHostChangesToContainer(s.ctx, hostChanges); err != nil {
			return err
		}
	}
	return nil
}

func (s *extractSync) shouldPreferHostReset(ctx context.Context) bool {
	action, err := latestHostReflogAction(ctx, s.localRepo)
	if err != nil {
		s.debugf("performGitSync: could not inspect host reflog: %v", err)
		return false
	}
	if !strings.HasPrefix(action, "reset:") {
		return false
	}
	clean, err := hostTrackedWorktreeClean(ctx, s.localRepo)
	if err != nil {
		s.debugf("performGitSync: could not inspect host tracked status: %v", err)
		return false
	}
	if clean {
		s.debugf("performGitSync: latest host reflog action %q indicates a reset with clean tracked worktree", action)
	}
	return clean
}

func latestHostReflogAction(ctx context.Context, repo string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "reflog", "-1", "--format=%gs", "HEAD")
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git reflog: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func hostTrackedWorktreeClean(ctx context.Context, repo string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "status", "--porcelain", "-z", "--untracked-files=no")
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("git status tracked-only: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return len(out) == 0, nil
}

func (s *extractSync) collectHostChanges(ctx context.Context) ([]trackedChange, error) {
	entries, err := gitStatusPorcelainHost(ctx, s.localRepo, nil)
	if err != nil {
		return nil, err
	}
	changes := buildTrackedChanges(entries)
	if len(changes) == 0 {
		return nil, nil
	}
	return changes, nil
}

func (s *extractSync) collectContainerChanges(ctx context.Context) ([]trackedChange, error) {
	entries, err := gitStatusPorcelainContainer(ctx, s.containerName, s.workdir, nil)
	if err != nil {
		return nil, err
	}
	changes := buildTrackedChanges(entries)
	if len(changes) == 0 {
		return nil, nil
	}
	return changes, nil
}

func (s *extractSync) applyContainerChangesToHost(ctx context.Context, changes []trackedChange) error {
	for _, change := range changes {
		if change.kind == changeRename && change.oldPath != "" {
			if shouldIgnoreRelative(change.oldPath) {
				continue
			}
			if err := s.removeOnHost(change.oldPath); err != nil {
				return err
			}
			fmt.Fprintf(s.logOut, "container → host: removed %s\n", change.oldPath)
		}
		switch change.kind {
		case changeDelete:
			if err := s.removeOnHost(change.path); err != nil {
				return err
			}
			fmt.Fprintf(s.logOut, "container → host: removed %s\n", change.path)
		case changeModify, changeRename:
			same, err := s.hashesMatch(ctx, change.path)
			if err != nil {
				return err
			}
			if same {
				continue
			}
			if err := s.copyContainerToHost(ctx, change.path); err != nil {
				return err
			}
			fmt.Fprintf(s.logOut, "container → host: updated %s\n", change.path)
		}
	}
	return nil
}

func (s *extractSync) applyHostChangesToContainer(ctx context.Context, changes []trackedChange) error {
	for _, change := range changes {
		if change.kind == changeRename && change.oldPath != "" {
			if shouldIgnoreRelative(change.oldPath) {
				continue
			}
			if err := s.removeInContainer(ctx, change.oldPath); err != nil {
				return err
			}
			fmt.Fprintf(s.logOut, "host → container: removed %s\n", change.oldPath)
		}
		switch change.kind {
		case changeDelete:
			if err := s.removeInContainer(ctx, change.path); err != nil {
				return err
			}
			fmt.Fprintf(s.logOut, "host → container: removed %s\n", change.path)
		case changeModify, changeRename:
			same, err := s.hashesMatch(ctx, change.path)
			if err != nil {
				return err
			}
			if same {
				continue
			}
			if err := s.copyHostToContainer(ctx, change.path); err != nil {
				return err
			}
			fmt.Fprintf(s.logOut, "host → container: updated %s\n", change.path)
		}
	}
	return nil
}

// drainEventQueue discards all pending file events from the queue.
// Used when git sync is about to change the working tree state, making
// queued events stale.
func (s *extractSync) drainEventQueue() {
	drained := 0
	for {
		select {
		case <-s.events:
			drained++
		default:
			if drained > 0 {
				s.debugf("drained %d stale file events before git sync", drained)
			}
			// Also clear retry queue - those paths are stale too
			if cleared := s.clearRetryQueue(); cleared > 0 {
				s.debugf("cleared %d stale retry entries before git sync", cleared)
			}
			return
		}
	}
}

// waitForFileSyncIdle waits until file sync is idle (no pending events)
func (s *extractSync) waitForFileSyncIdle() {
	// The fileSyncIdle channel is closed when processEvents has no pending work
	// We create a new channel each cycle
	for {
		// Get channel reference under lock
		s.fileSyncIdleMu.Lock()
		ch := s.fileSyncIdle
		s.fileSyncIdleMu.Unlock()

		select {
		case <-s.ctx.Done():
			return
		case <-ch:
			return
		case <-time.After(50 * time.Millisecond):
			// Check again with fresh channel reference
		}
	}
}

// signalFileSyncIdle signals that file sync is currently idle
func (s *extractSync) signalFileSyncIdle() {
	s.fileSyncIdleMu.Lock()
	defer s.fileSyncIdleMu.Unlock()
	// Close and recreate the idle channel to signal waiting goroutines
	select {
	case <-s.fileSyncIdle:
		// Already closed, recreate it
	default:
		close(s.fileSyncIdle)
	}
	s.fileSyncIdle = make(chan struct{})
}

func gitStatusPorcelainHost(ctx context.Context, repo string, paths []string) ([]statusEntry, error) {
	args := []string{"status", "--porcelain", "-z", "--untracked-files=all"}
	if len(paths) > 0 {
		args = append(args, "--")
		for _, p := range paths {
			args = append(args, filepath.FromSlash(p))
		}
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git status (host): %w: %s", err, strings.TrimSpace(string(out)))
	}
	return parseStatusOutput(string(out)), nil
}

func gitStatusPorcelainContainer(ctx context.Context, name, workdir string, paths []string) ([]statusEntry, error) {
	args := []string{"git", "status", "--porcelain", "-z", "--untracked-files=all"}
	if len(paths) > 0 {
		args = append(args, "--")
		args = append(args, paths...)
	}
	out, err := dockerExecOutput(ctx, name, workdir, nil, args)
	if err != nil {
		return nil, fmt.Errorf("git status (container): %w: %s", err, strings.TrimSpace(out))
	}
	return parseStatusOutput(out), nil
}

func parseStatusOutput(out string) []statusEntry {
	if out == "" {
		return nil
	}

	records := strings.Split(out, "\x00")
	entries := make([]statusEntry, 0, len(records))
	for i := 0; i < len(records); i++ {
		record := records[i]
		if record == "" {
			continue
		}
		if len(record) < 3 {
			continue
		}

		entry := statusEntry{
			staged:   rune(record[0]),
			unstaged: rune(record[1]),
			path:     filepath.ToSlash(record[3:]),
		}
		if entry.staged == 'R' || entry.unstaged == 'R' || entry.staged == 'C' || entry.unstaged == 'C' {
			if i+1 >= len(records) {
				entries = append(entries, entry)
				continue
			}
			i++
			entry.oldPath = filepath.ToSlash(records[i])
		}
		entries = append(entries, entry)
	}
	return entries
}

func buildTrackedChanges(entries []statusEntry) []trackedChange {
	var out []trackedChange
	for _, e := range entries {
		if e.path == "" {
			continue
		}
		if shouldIgnoreRelative(e.path) {
			continue
		}
		if e.staged == 'R' || e.unstaged == 'R' {
			out = append(out, trackedChange{kind: changeRename, path: e.path, oldPath: e.oldPath})
			continue
		}
		if e.staged == 'D' || e.unstaged == 'D' {
			path := e.path
			if e.oldPath != "" {
				path = e.oldPath
			}
			out = append(out, trackedChange{kind: changeDelete, path: path})
			continue
		}
		// Treat all other changes (including untracked files ??) as modifications
		out = append(out, trackedChange{kind: changeModify, path: e.path})
	}
	return out
}

func (s *extractSync) hashesMatch(ctx context.Context, rel string) (bool, error) {
	hostHash, err := s.hostHash(ctx, rel)
	if err != nil {
		return false, err
	}
	containerHash, err := s.containerHash(ctx, rel)
	if err != nil {
		return false, err
	}
	return hostHash != "" && hostHash == containerHash, nil
}

func (s *extractSync) hostHash(ctx context.Context, rel string) (string, error) {
	abs := filepath.Join(s.localRepo, filepath.FromSlash(rel))
	if _, err := os.Stat(abs); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		if isTransientError(err) {
			return "", fmt.Errorf("%w: %v", errTransient, err)
		}
		return "", err
	}
	cmd := exec.CommandContext(ctx, "git", "-C", s.localRepo, "hash-object", "--", filepath.FromSlash(rel))
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if strings.Contains(msg, "does not exist") {
			return "", nil
		}
		fullErr := fmt.Errorf("git hash-object (host): %w: %s", err, msg)
		if isTransientError(fullErr) {
			return "", fmt.Errorf("%w: %s", errTransient, msg)
		}
		return "", fullErr
	}
	return strings.TrimSpace(string(out)), nil
}

func (s *extractSync) containerHash(ctx context.Context, rel string) (string, error) {
	// First check if file exists in container
	checkCmd := []string{"bash", "-lc", fmt.Sprintf("test -e %s && echo exists", shellQuote(rel))}
	out, _ := dockerExecOutput(ctx, s.containerName, s.workdir, nil, checkCmd)
	if !strings.Contains(out, "exists") {
		// File doesn't exist in container
		return "", nil
	}

	// File exists, get its hash
	args := []string{"git", "hash-object", "--", rel}
	out, err := dockerExecOutput(ctx, s.containerName, s.workdir, nil, args)
	if err != nil {
		msg := strings.TrimSpace(out)
		if strings.Contains(msg, "does not exist") || strings.Contains(msg, "No such file") {
			return "", nil
		}
		fullErr := fmt.Errorf("git hash-object (container): %w: %s", err, msg)
		if isTransientError(fullErr) {
			return "", fmt.Errorf("%w: %s", errTransient, msg)
		}
		return "", fullErr
	}
	return strings.TrimSpace(out), nil
}

func (s *extractSync) copyHostToContainer(ctx context.Context, rel string) error {
	hostPath := filepath.Join(s.localRepo, filepath.FromSlash(rel))
	info, err := os.Stat(hostPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errSyncSkipped
		}
		return err
	}
	if info.IsDir() {
		return nil
	}
	destDir := path.Join(s.workdir, path.Dir(rel))
	if destDir == s.workdir || destDir == "." || destDir == "" {
		destDir = s.workdir
	}
	if err := s.ensureContainerDir(ctx, path.Dir(rel)); err != nil {
		return err
	}
	if err := dockerCopyToContainerWithOwnership(ctx, s.containerName, hostPath, destDir, false); err != nil {
		// The file may have vanished between the initial stat and docker reading it.
		if _, statErr := os.Stat(hostPath); errors.Is(statErr, os.ErrNotExist) {
			return errSyncSkipped
		}
		return err
	}
	// Ensure the discourse user retains write permissions
	mode := fmt.Sprintf("%04o", info.Mode().Perm())
	if _, err := dockerExecAsRoot(ctx, s.containerName, s.workdir, nil, []string{"chmod", mode, rel}); err != nil {
		return fmt.Errorf("container chmod %s: %w", rel, err)
	}
	return nil
}

func (s *extractSync) copyContainerToHost(ctx context.Context, rel string) error {
	hostPath := filepath.Join(s.localRepo, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(hostPath), 0o755); err != nil {
		return err
	}
	containerPath := path.Join(s.workdir, rel)
	if err := dockerCopyFromContainer(ctx, s.containerName, containerPath, hostPath); err != nil {
		// The file may have vanished between event delivery and copying.
		checkCmd := []string{"bash", "-lc", fmt.Sprintf("test -e %s && echo exists", shellQuote(rel))}
		out, _ := dockerExecOutput(ctx, s.containerName, s.workdir, nil, checkCmd)
		if !strings.Contains(out, "exists") {
			return errSyncSkipped
		}
		return err
	}
	return nil
}

func (s *extractSync) removeInContainer(ctx context.Context, rel string) error {
	cmd := []string{"bash", "-lc", "rm -rf -- " + shellQuote(rel)}
	if _, err := dockerExecOutput(ctx, s.containerName, s.workdir, nil, cmd); err != nil {
		return fmt.Errorf("container remove %s: %w", rel, err)
	}
	return nil
}

func (s *extractSync) removeOnHost(rel string) error {
	pathOnHost := filepath.Join(s.localRepo, filepath.FromSlash(rel))
	if _, err := os.Stat(pathOnHost); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := os.Remove(pathOnHost); err != nil {
		if err := os.RemoveAll(pathOnHost); err != nil {
			return err
		}
	}
	return nil
}

func (s *extractSync) ensureContainerDir(ctx context.Context, rel string) error {
	dir := rel
	if dir == "." || dir == "" {
		return nil
	}
	cmd := []string{"bash", "-lc", "mkdir -p " + shellQuote(rel)}
	if _, err := dockerExecOutput(ctx, s.containerName, s.workdir, nil, cmd); err != nil {
		return fmt.Errorf("container mkdir %s: %w", rel, err)
	}
	return nil
}

func (s *extractSync) addHostWatchers(w *fsnotify.Watcher, root string) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		rel, ok := s.relativeFromLocal(path)
		if ok && shouldIgnoreRelative(rel) {
			return filepath.SkipDir
		}
		return w.Add(path)
	})
}

func (s *extractSync) relativeFromLocal(pathname string) (string, bool) {
	rel, err := filepath.Rel(s.localRepo, pathname)
	if err != nil {
		return "", false
	}
	if strings.HasPrefix(rel, "..") {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

func (s *extractSync) relativeFromContainer(abs string) (string, bool) {
	if abs == "" {
		return "", false
	}
	clean := path.Clean(abs)
	work := path.Clean(s.workdir)
	if work == "/" {
		rel := strings.TrimPrefix(clean, "/")
		return rel, true
	}
	if clean != work && !strings.HasPrefix(clean, work+"/") {
		return "", false
	}
	rel := strings.TrimPrefix(clean, work)
	rel = strings.TrimPrefix(rel, "/")
	return rel, true
}

func (s *extractSync) ensureInotify() error {
	out, err := dockerExecOutput(s.ctx, s.containerName, s.workdir, nil, []string{"bash", "-lc", "command -v inotifywait"})
	trimmed := strings.TrimSpace(out)
	if err != nil {
		if trimmed == "" {
			return fmt.Errorf("inotifywait not found in container; install inotify-tools (provides inotifywait)")
		}
		return fmt.Errorf("checking inotifywait: %w: %s", err, trimmed)
	}
	if trimmed == "" {
		return fmt.Errorf("inotifywait not found in container; install inotify-tools (provides inotifywait)")
	}
	return nil
}

func (s *extractSync) queueEvent(ev watcherEvent) {
	select {
	case <-s.ctx.Done():
		return
	case s.events <- ev:
	}
}

func (s *extractSync) debugf(format string, args ...interface{}) {
	if !s.debug {
		return
	}
	fmt.Fprintf(s.logOut, "[debug] "+format+"\n", args...)
}

func (s *extractSync) retrySnapshot() map[string]retryEntry {
	s.retryQueueMu.Lock()
	defer s.retryQueueMu.Unlock()
	snapshot := make(map[string]retryEntry, len(s.retryQueue))
	for path, entry := range s.retryQueue {
		snapshot[path] = entry
	}
	return snapshot
}

func (s *extractSync) deleteRetry(path string) {
	s.retryQueueMu.Lock()
	defer s.retryQueueMu.Unlock()
	delete(s.retryQueue, path)
}

func (s *extractSync) clearRetryQueue() int {
	s.retryQueueMu.Lock()
	defer s.retryQueueMu.Unlock()
	count := len(s.retryQueue)
	s.retryQueue = make(map[string]retryEntry)
	return count
}

func (s *extractSync) queueRetry(path string, source changeSource) {
	s.retryQueueMu.Lock()
	entry := s.retryQueue[path]
	entry.source = source
	entry.attempts++
	if entry.attempts >= maxRetryAttempts {
		fmt.Fprintf(s.errOut, "warning: giving up on %s after %d attempts\n", path, entry.attempts)
		delete(s.retryQueue, path)
		s.retryQueueMu.Unlock()
		return
	}
	fmt.Fprintf(s.errOut, "warning: %s failed, will retry (attempt %d/%d)\n", path, entry.attempts, maxRetryAttempts)
	s.retryQueue[path] = entry
	s.retryQueueMu.Unlock()
}

func (s *extractSync) isGitIgnored(ctx context.Context, repoDir, relPath string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "check-ignore", "-q", filepath.FromSlash(relPath))
	cmd.Dir = repoDir
	if err := cmd.Run(); err == nil {
		// Exit code 0 means file IS ignored
		return true, nil
	}
	return false, nil
}

func (s *extractSync) isGitIgnoredInContainer(ctx context.Context, relPath string) (bool, error) {
	_, err := dockerExecOutput(ctx, s.containerName, s.workdir, nil, []string{"git", "check-ignore", "-q", relPath})
	if err == nil {
		// Exit code 0 means file IS ignored
		return true, nil
	}
	return false, nil
}

func (s *extractSync) isTrackedByGit(ctx context.Context, repoDir, relPath string) (bool, error) {
	// First check if file is ignored by .gitignore
	ignored, _ := s.isGitIgnored(ctx, repoDir, relPath)
	if ignored {
		return false, nil
	}

	// Not ignored, check if file is tracked
	cmd := exec.CommandContext(ctx, "git", "ls-files", "--", filepath.FromSlash(relPath))
	cmd.Dir = repoDir
	out, err := cmd.Output()
	if err != nil {
		return false, nil // Not tracked or error
	}
	return strings.TrimSpace(string(out)) != "", nil
}

func (s *extractSync) isTrackedByGitInContainer(ctx context.Context, relPath string) (bool, error) {
	// First check if file is ignored by .gitignore
	ignored, _ := s.isGitIgnoredInContainer(ctx, relPath)
	if ignored {
		return false, nil
	}

	// Not ignored, check if file is tracked
	out, err := dockerExecOutput(ctx, s.containerName, s.workdir, nil, []string{"git", "ls-files", "--", relPath})
	if err != nil {
		return false, nil // Not tracked or error
	}
	return strings.TrimSpace(out) != "", nil
}

func shouldIgnoreRelative(rel string) bool {
	if rel == "" {
		return false
	}
	clean := strings.TrimPrefix(rel, "./")
	clean = strings.TrimPrefix(clean, "/")
	return clean == ".git" || strings.HasPrefix(clean, ".git/") || strings.Contains(clean, "/.git/")
}

func mapKeys(m map[string]struct{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func parseInotifyLine(line string) (string, bool) {
	if idx := strings.LastIndex(line, "|"); idx != -1 {
		abs := strings.TrimSpace(line[:idx])
		if abs == "" {
			return "", false
		}
		return path.Clean(abs), true
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return "", false
	}
	dir := fields[0]
	name := ""
	if len(fields) >= 3 {
		name = strings.Join(fields[2:], " ")
	}
	if name != "" {
		return path.Clean(path.Join(dir, name)), true
	}
	if dir == "" {
		return "", false
	}
	return path.Clean(dir), true
}
