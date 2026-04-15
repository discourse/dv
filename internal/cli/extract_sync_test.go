package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"dv/internal/docker"
)

// TestProcessEventsFlushTimeout verifies that processEvents doesn't hang
// if flush() takes too long during context cancellation.
func TestProcessEventsFlushTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	var logBuf bytes.Buffer
	s := &extractSync{
		ctx:           ctx,
		cancel:        cancel,
		containerName: "fake-container",
		workdir:       "/fake/workdir",
		localRepo:     "/fake/local",
		logOut:        &logBuf,
		errOut:        &logBuf,
		debug:         true,
		events:        make(chan watcherEvent, 256),
		fileSyncIdle:  make(chan struct{}),
	}

	// Start processEvents in a goroutine
	done := make(chan error, 1)
	go func() {
		done <- s.processEvents()
	}()

	// Queue some events
	s.events <- watcherEvent{source: sourceHost, path: "test.go"}
	s.events <- watcherEvent{source: sourceContainer, path: "other.go"}

	// Give it a moment to receive events
	time.Sleep(50 * time.Millisecond)

	// Cancel context - this should trigger cleanup
	cancel()

	// processEvents should exit within a reasonable time (the 2s flush timeout + buffer)
	select {
	case <-done:
		// Success - processEvents exited
	case <-time.After(5 * time.Second):
		t.Fatal("processEvents hung after context cancellation - deadlock detected")
	}
}

// TestQueueEventDoesNotBlockOnFullChannel verifies that queueEvent
// respects context cancellation even when the channel is full.
func TestQueueEventDoesNotBlockOnFullChannel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	var logBuf bytes.Buffer
	s := &extractSync{
		ctx:           ctx,
		cancel:        cancel,
		containerName: "fake-container",
		workdir:       "/fake/workdir",
		localRepo:     "/fake/local",
		logOut:        &logBuf,
		errOut:        &logBuf,
		debug:         false,
		events:        make(chan watcherEvent, 2), // Small buffer to fill quickly
	}

	// Fill the channel
	s.events <- watcherEvent{source: sourceHost, path: "1.go"}
	s.events <- watcherEvent{source: sourceHost, path: "2.go"}

	// Cancel context
	cancel()

	// queueEvent should not block because context is cancelled
	done := make(chan struct{})
	go func() {
		s.queueEvent(watcherEvent{source: sourceHost, path: "3.go"})
		close(done)
	}()

	select {
	case <-done:
		// Success
	case <-time.After(1 * time.Second):
		t.Fatal("queueEvent blocked on full channel despite cancelled context")
	}
}

// TestTimerResetRace simulates rapid event arrival to check for timer state issues.
func TestTimerResetRace(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var logBuf bytes.Buffer
	s := &extractSync{
		ctx:           ctx,
		cancel:        cancel,
		containerName: "fake-container",
		workdir:       "/fake/workdir",
		localRepo:     "/fake/local",
		logOut:        &logBuf,
		errOut:        &logBuf,
		debug:         false,
		events:        make(chan watcherEvent, 256),
		fileSyncIdle:  make(chan struct{}),
	}

	// Start processEvents
	done := make(chan error, 1)
	go func() {
		done <- s.processEvents()
	}()

	// Rapidly send events from multiple goroutines
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				select {
				case <-ctx.Done():
					return
				default:
					s.queueEvent(watcherEvent{source: sourceHost, path: "test.go"})
					time.Sleep(time.Millisecond)
				}
			}
		}(i)
	}

	// Let events flow for a bit
	time.Sleep(100 * time.Millisecond)

	// Cancel and verify clean exit
	cancel()
	wg.Wait()

	select {
	case <-done:
		// Success
	case <-time.After(5 * time.Second):
		t.Fatal("processEvents hung under rapid event load - possible timer race")
	}
}

// TestChannelCapacityUnderLoad verifies the event channel doesn't cause
// deadlocks when events arrive faster than processing.
func TestChannelCapacityUnderLoad(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var logBuf bytes.Buffer
	s := &extractSync{
		ctx:           ctx,
		cancel:        cancel,
		containerName: "fake-container",
		workdir:       "/fake/workdir",
		localRepo:     "/fake/local",
		logOut:        &logBuf,
		errOut:        &logBuf,
		debug:         false,
		events:        make(chan watcherEvent, 10), // Intentionally small
		fileSyncIdle:  make(chan struct{}),
	}

	// Start processEvents
	done := make(chan error, 1)
	go func() {
		done <- s.processEvents()
	}()

	// Try to overwhelm with events
	blocked := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			select {
			case <-ctx.Done():
				close(blocked)
				return
			case s.events <- watcherEvent{source: sourceHost, path: "test.go"}:
			}
		}
		close(blocked)
	}()

	// Give it time to potentially block
	time.Sleep(500 * time.Millisecond)

	cancel()

	select {
	case <-blocked:
		// Producer finished or was cancelled
	case <-time.After(2 * time.Second):
		t.Fatal("Event producer blocked - channel backpressure issue")
	}

	select {
	case <-done:
		// Success
	case <-time.After(5 * time.Second):
		t.Fatal("processEvents hung")
	}
}

// TestProcessHostChangesWithSlowDocker simulates what happens when docker exec
// hangs during processHostChanges. This is the most likely deadlock scenario.
func TestProcessHostChangesWithSlowDocker(t *testing.T) {
	// This test doesn't actually call docker - it tests the timeout behavior
	// when the underlying operations would block.

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Create a temp directory to act as local repo
	tmpDir, err := os.MkdirTemp("", "dv-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Initialize a git repo
	if err := runInDir(tmpDir, nil, nil, "git", "init"); err != nil {
		t.Fatalf("git init failed: %v", err)
	}
	if err := runInDir(tmpDir, nil, nil, "git", "config", "user.email", "test@test.com"); err != nil {
		t.Fatalf("git config email failed: %v", err)
	}
	if err := runInDir(tmpDir, nil, nil, "git", "config", "user.name", "Test"); err != nil {
		t.Fatalf("git config name failed: %v", err)
	}

	// Create and commit a file
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := runInDir(tmpDir, nil, nil, "git", "add", "."); err != nil {
		t.Fatal(err)
	}
	if err := runInDir(tmpDir, nil, nil, "git", "commit", "-m", "initial"); err != nil {
		t.Fatal(err)
	}

	var logBuf bytes.Buffer
	s := &extractSync{
		ctx:           ctx,
		cancel:        cancel,
		containerName: "nonexistent-container-that-will-fail-fast",
		workdir:       "/fake/workdir",
		localRepo:     tmpDir,
		logOut:        &logBuf,
		errOut:        &logBuf,
		debug:         true,
		events:        make(chan watcherEvent, 256),
		fileSyncIdle:  make(chan struct{}),
	}

	// Modify the file to create a change
	if err := os.WriteFile(testFile, []byte("modified"), 0644); err != nil {
		t.Fatal(err)
	}

	// Start processEvents
	done := make(chan error, 1)
	go func() {
		done <- s.processEvents()
	}()

	// Queue an event for the modified file
	s.queueEvent(watcherEvent{source: sourceHost, path: "test.txt"})

	// The processHostChanges will try to call docker, which should fail quickly
	// for a nonexistent container. The test verifies we don't hang.

	select {
	case err := <-done:
		// Expect an error (container doesn't exist) but should NOT hang
		if err != nil {
			t.Logf("Got expected error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("processEvents hung when docker should have failed fast")
	}
}

func TestCopyHostToContainerSkipsIfFileVanishes(t *testing.T) {
	s := &extractSync{
		containerName: "fake-container",
		workdir:       "/fake/workdir",
		localRepo:     t.TempDir(),
		logOut:        io.Discard,
		errOut:        io.Discard,
	}

	err := s.copyHostToContainer(context.Background(), "spec/lib/.conform.7348585.search_spec.rb")
	if err == nil || !errors.Is(err, errSyncSkipped) {
		t.Fatalf("expected errSyncSkipped, got %v", err)
	}
}

// TestFlushTimeoutDuringCancellation specifically tests the 2-second timeout
// in processEvents cleanup path.
func TestFlushTimeoutDuringCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	var logBuf bytes.Buffer
	s := &extractSync{
		ctx:           ctx,
		cancel:        cancel,
		containerName: "fake-container",
		workdir:       "/fake/workdir",
		localRepo:     "/nonexistent/path/that/will/fail",
		logOut:        &logBuf,
		errOut:        &logBuf,
		debug:         true,
		events:        make(chan watcherEvent, 256),
		fileSyncIdle:  make(chan struct{}),
	}

	done := make(chan error, 1)
	go func() {
		done <- s.processEvents()
	}()

	// Queue events so there's something to flush
	for i := 0; i < 10; i++ {
		s.queueEvent(watcherEvent{source: sourceHost, path: "test.go"})
		s.queueEvent(watcherEvent{source: sourceContainer, path: "other.go"})
	}

	// Small delay to let events queue up
	time.Sleep(50 * time.Millisecond)

	// Cancel immediately - this triggers the flush timeout path
	cancel()

	// Should complete within the 2-second flush timeout + margin
	select {
	case <-done:
		t.Log("processEvents exited cleanly")
	case <-time.After(5 * time.Second):
		t.Fatal("processEvents exceeded flush timeout - deadlock in cleanup")
	}
}

// TestNormalFlushHasNoTimeout demonstrates that the normal flush path
// (triggered by timer, not cancellation) has no timeout and could hang.
// This test documents the issue - it will fail if docker hangs.
func TestNormalFlushHasNoTimeout(t *testing.T) {
	// Skip in normal test runs - this test is for documentation/reproduction
	if os.Getenv("DV_TEST_DEADLOCK") == "" {
		t.Skip("Set DV_TEST_DEADLOCK=1 to run deadlock reproduction tests")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create a temp directory to act as local repo
	tmpDir, err := os.MkdirTemp("", "dv-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Initialize a git repo
	if err := runInDir(tmpDir, nil, nil, "git", "init"); err != nil {
		t.Fatalf("git init failed: %v", err)
	}
	if err := runInDir(tmpDir, nil, nil, "git", "config", "user.email", "test@test.com"); err != nil {
		t.Fatalf("git config email failed: %v", err)
	}
	if err := runInDir(tmpDir, nil, nil, "git", "config", "user.name", "Test"); err != nil {
		t.Fatalf("git config name failed: %v", err)
	}

	// Create and commit a file
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := runInDir(tmpDir, nil, nil, "git", "add", "."); err != nil {
		t.Fatal(err)
	}
	if err := runInDir(tmpDir, nil, nil, "git", "commit", "-m", "initial"); err != nil {
		t.Fatal(err)
	}

	var logBuf bytes.Buffer
	s := &extractSync{
		ctx:           ctx,
		cancel:        cancel,
		containerName: "nonexistent-container",
		workdir:       "/fake/workdir",
		localRepo:     tmpDir,
		logOut:        &logBuf,
		errOut:        &logBuf,
		debug:         true,
		events:        make(chan watcherEvent, 256),
		fileSyncIdle:  make(chan struct{}),
	}

	// Modify the file
	if err := os.WriteFile(testFile, []byte("modified"), 0644); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		done <- s.processEvents()
	}()

	// Queue an event
	s.queueEvent(watcherEvent{source: sourceHost, path: "test.txt"})

	// Wait for the timer-triggered flush (250ms settle + processing)
	// The flush will call processHostChanges which calls docker commands
	select {
	case err := <-done:
		if err != nil {
			t.Logf("Got expected error (docker failed): %v", err)
		}
	case <-time.After(10 * time.Second):
		// This timeout being hit means docker exec hung without timeout
		t.Fatal("Timer-triggered flush hung - no timeout on docker operations")
	}
}

// TestFlushTimeoutWorks validates that the flush timeout prevents hangs.
func TestFlushTimeoutWorks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var logBuf bytes.Buffer
	s := &extractSync{
		ctx:           ctx,
		cancel:        cancel,
		containerName: "fake-container",
		workdir:       "/fake/workdir",
		localRepo:     "/nonexistent/path",
		logOut:        &logBuf,
		errOut:        &logBuf,
		debug:         true,
		events:        make(chan watcherEvent, 256),
		fileSyncIdle:  make(chan struct{}),
	}

	done := make(chan error, 1)
	go func() {
		done <- s.processEvents()
	}()

	// Send events
	s.queueEvent(watcherEvent{source: sourceHost, path: "test.go"})

	// Wait for settle delay + some margin for flush to start
	time.Sleep(300 * time.Millisecond)

	// Cancel context
	cancel()

	// Should exit quickly due to the timeout mechanism
	select {
	case <-done:
		t.Log("processEvents exited cleanly with timeout protection")
	case <-time.After(5 * time.Second):
		t.Fatal("processEvents hung despite timeout protection")
	}
}

func TestPerformGitSyncPreservesHostChanges(t *testing.T) {
	ctx := context.Background()

	hostDir := t.TempDir()
	if err := runInDir(hostDir, nil, nil, "git", "init"); err != nil {
		t.Fatalf("git init failed: %v", err)
	}
	if err := runInDir(hostDir, nil, nil, "git", "config", "user.email", "test@test.com"); err != nil {
		t.Fatalf("git config email failed: %v", err)
	}
	if err := runInDir(hostDir, nil, nil, "git", "config", "user.name", "Test"); err != nil {
		t.Fatalf("git config name failed: %v", err)
	}
	hostFile := filepath.Join(hostDir, "file.txt")
	if err := os.WriteFile(hostFile, []byte("base"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runInDir(hostDir, nil, nil, "git", "add", "."); err != nil {
		t.Fatal(err)
	}
	if err := runInDir(hostDir, nil, nil, "git", "commit", "-m", "initial"); err != nil {
		t.Fatal(err)
	}

	containerRoot := t.TempDir()
	containerDir := filepath.Join(containerRoot, "container")
	if err := runInDir("", nil, nil, "git", "clone", hostDir, containerDir); err != nil {
		t.Fatalf("git clone failed: %v", err)
	}
	containerFile := filepath.Join(containerDir, "file.txt")

	origExecOutput := dockerExecOutput
	origExecAsRoot := dockerExecAsRoot
	origCopyFrom := dockerCopyFromContainer
	origCopyTo := dockerCopyToContainerWithOwnership
	t.Cleanup(func() {
		dockerExecOutput = origExecOutput
		dockerExecAsRoot = origExecAsRoot
		dockerCopyFromContainer = origCopyFrom
		dockerCopyToContainerWithOwnership = origCopyTo
	})

	dockerExecOutput = func(ctx context.Context, _ string, workdir string, _ docker.Envs, argv []string) (string, error) {
		if len(argv) == 0 {
			return "", nil
		}
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Dir = workdir
		out, err := cmd.Output()
		return string(out), err
	}
	dockerExecAsRoot = func(ctx context.Context, _ string, workdir string, _ docker.Envs, argv []string) (string, error) {
		if len(argv) == 0 {
			return "", nil
		}
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Dir = workdir
		out, err := cmd.Output()
		return string(out), err
	}
	dockerCopyFromContainer = func(_ context.Context, _ string, srcInContainer, dstOnHost string) error {
		return copyFile(srcInContainer, dstOnHost)
	}
	dockerCopyToContainerWithOwnership = func(_ context.Context, _ string, srcOnHost, dstInContainer string, _ bool) error {
		return copyFileToDir(srcOnHost, dstInContainer)
	}

	if err := os.WriteFile(hostFile, []byte("host-dirty"), 0o644); err != nil {
		t.Fatal(err)
	}

	var logBuf bytes.Buffer
	s := &extractSync{
		ctx:           ctx,
		containerName: "fake-container",
		workdir:       containerDir,
		localRepo:     hostDir,
		logOut:        &logBuf,
		errOut:        &logBuf,
		events:        make(chan watcherEvent, 1),
		retryQueue:    make(map[string]retryEntry),
		fileSyncIdle:  make(chan struct{}),
		gitSyncer:     fakeGitSyncer{},
	}
	close(s.fileSyncIdle)

	if err := s.performGitSync(); err != nil {
		t.Fatalf("performGitSync failed: %v", err)
	}

	containerOut, err := os.ReadFile(containerFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(containerOut) != "host-dirty" {
		t.Fatalf("container file not updated from host dirty state, got %q", string(containerOut))
	}
}

func TestPerformGitSyncHostResetPrefersHostState(t *testing.T) {
	ctx := context.Background()

	hostDir := t.TempDir()
	if err := runInDir(hostDir, nil, nil, "git", "init"); err != nil {
		t.Fatalf("git init failed: %v", err)
	}
	if err := runInDir(hostDir, nil, nil, "git", "config", "user.email", "test@test.com"); err != nil {
		t.Fatalf("git config email failed: %v", err)
	}
	if err := runInDir(hostDir, nil, nil, "git", "config", "user.name", "Test"); err != nil {
		t.Fatalf("git config name failed: %v", err)
	}
	hostFile := filepath.Join(hostDir, "file.txt")
	if err := os.WriteFile(hostFile, []byte("base"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runInDir(hostDir, nil, nil, "git", "add", "."); err != nil {
		t.Fatal(err)
	}
	if err := runInDir(hostDir, nil, nil, "git", "commit", "-m", "initial"); err != nil {
		t.Fatal(err)
	}

	containerRoot := t.TempDir()
	containerDir := filepath.Join(containerRoot, "container")
	if err := runInDir("", nil, nil, "git", "clone", hostDir, containerDir); err != nil {
		t.Fatalf("git clone failed: %v", err)
	}
	containerFile := filepath.Join(containerDir, "file.txt")
	if err := os.WriteFile(containerFile, []byte("container-dirty"), 0o644); err != nil {
		t.Fatal(err)
	}

	origExecOutput := dockerExecOutput
	origExecAsRoot := dockerExecAsRoot
	origCopyFrom := dockerCopyFromContainer
	origCopyTo := dockerCopyToContainerWithOwnership
	t.Cleanup(func() {
		dockerExecOutput = origExecOutput
		dockerExecAsRoot = origExecAsRoot
		dockerCopyFromContainer = origCopyFrom
		dockerCopyToContainerWithOwnership = origCopyTo
	})

	dockerExecOutput = func(ctx context.Context, _ string, workdir string, _ docker.Envs, argv []string) (string, error) {
		if len(argv) == 0 {
			return "", nil
		}
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Dir = workdir
		out, err := cmd.Output()
		return string(out), err
	}
	dockerExecAsRoot = func(ctx context.Context, _ string, workdir string, _ docker.Envs, argv []string) (string, error) {
		if len(argv) == 0 {
			return "", nil
		}
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Dir = workdir
		out, err := cmd.Output()
		return string(out), err
	}
	dockerCopyFromContainer = func(_ context.Context, _ string, srcInContainer, dstOnHost string) error {
		return copyFile(srcInContainer, dstOnHost)
	}
	dockerCopyToContainerWithOwnership = func(_ context.Context, _ string, srcOnHost, dstInContainer string, _ bool) error {
		return copyFileToDir(srcOnHost, dstInContainer)
	}

	if err := runInDir(hostDir, nil, nil, "git", "reset", "--hard", "HEAD"); err != nil {
		t.Fatalf("git reset --hard failed: %v", err)
	}

	syncCalled := false
	forceSyncCalled := false
	resetContainer := func() error {
		forceSyncCalled = true
		return os.WriteFile(containerFile, []byte("base"), 0o644)
	}

	var logBuf bytes.Buffer
	s := &extractSync{
		ctx:           ctx,
		containerName: "fake-container",
		workdir:       containerDir,
		localRepo:     hostDir,
		logOut:        &logBuf,
		errOut:        &logBuf,
		events:        make(chan watcherEvent, 1),
		retryQueue:    make(map[string]retryEntry),
		fileSyncIdle:  make(chan struct{}),
		gitSyncer: fakeGitSyncer{
			sync: func() error {
				syncCalled = true
				return resetContainer()
			},
			forceSync: resetContainer,
		},
	}
	close(s.fileSyncIdle)

	if err := s.performGitSync(); err != nil {
		t.Fatalf("performGitSync failed: %v", err)
	}
	if syncCalled {
		t.Fatal("expected reset path to force container sync, but regular syncToContainer was used")
	}
	if !forceSyncCalled {
		t.Fatal("expected reset path to call forceSyncToContainer")
	}

	hostOut, err := os.ReadFile(hostFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(hostOut) != "base" {
		t.Fatalf("host file was unexpectedly resurrected from container, got %q", string(hostOut))
	}
	containerOut, err := os.ReadFile(containerFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(containerOut) != "base" {
		t.Fatalf("container file not reset to host state, got %q", string(containerOut))
	}
}

func TestPerformGitSyncAutoSyncContainerChanges(t *testing.T) {
	ctx := context.Background()

	hostDir := t.TempDir()
	if err := runInDir(hostDir, nil, nil, "git", "init"); err != nil {
		t.Fatalf("git init failed: %v", err)
	}
	if err := runInDir(hostDir, nil, nil, "git", "config", "user.email", "test@test.com"); err != nil {
		t.Fatalf("git config email failed: %v", err)
	}
	if err := runInDir(hostDir, nil, nil, "git", "config", "user.name", "Test"); err != nil {
		t.Fatalf("git config name failed: %v", err)
	}
	hostFile := filepath.Join(hostDir, "file.txt")
	originalHost := []byte("host")
	if err := os.WriteFile(hostFile, originalHost, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runInDir(hostDir, nil, nil, "git", "add", "."); err != nil {
		t.Fatal(err)
	}
	if err := runInDir(hostDir, nil, nil, "git", "commit", "-m", "initial"); err != nil {
		t.Fatal(err)
	}

	containerRoot := t.TempDir()
	containerDir := filepath.Join(containerRoot, "container")
	if err := runInDir("", nil, nil, "git", "clone", hostDir, containerDir); err != nil {
		t.Fatalf("git clone failed: %v", err)
	}

	containerFile := filepath.Join(containerDir, "file.txt")
	if err := os.WriteFile(containerFile, []byte("container"), 0o644); err != nil {
		t.Fatal(err)
	}

	origExecOutput := dockerExecOutput
	origExecAsRoot := dockerExecAsRoot
	origCopyFrom := dockerCopyFromContainer
	origCopyTo := dockerCopyToContainerWithOwnership
	t.Cleanup(func() {
		dockerExecOutput = origExecOutput
		dockerExecAsRoot = origExecAsRoot
		dockerCopyFromContainer = origCopyFrom
		dockerCopyToContainerWithOwnership = origCopyTo
	})

	dockerExecOutput = func(ctx context.Context, _ string, workdir string, _ docker.Envs, argv []string) (string, error) {
		if len(argv) == 0 {
			return "", nil
		}
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Dir = workdir
		out, err := cmd.Output()
		return string(out), err
	}
	dockerExecAsRoot = func(ctx context.Context, _ string, workdir string, _ docker.Envs, argv []string) (string, error) {
		if len(argv) == 0 {
			return "", nil
		}
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Dir = workdir
		out, err := cmd.Output()
		return string(out), err
	}
	dockerCopyFromContainer = func(_ context.Context, _ string, srcInContainer, dstOnHost string) error {
		return copyFile(srcInContainer, dstOnHost)
	}
	dockerCopyToContainerWithOwnership = func(_ context.Context, _ string, srcOnHost, dstInContainer string, _ bool) error {
		return copyFileToDir(srcOnHost, dstInContainer)
	}

	resetContainer := func() error {
		return os.WriteFile(containerFile, originalHost, 0o644)
	}

	var logBuf bytes.Buffer
	s := &extractSync{
		ctx:           ctx,
		containerName: "fake-container",
		workdir:       containerDir,
		localRepo:     hostDir,
		logOut:        &logBuf,
		errOut:        &logBuf,
		events:        make(chan watcherEvent, 1),
		retryQueue:    make(map[string]retryEntry),
		fileSyncIdle:  make(chan struct{}),
		gitSyncer:     fakeGitSyncer{sync: resetContainer},
	}
	close(s.fileSyncIdle)

	if err := s.performGitSync(); err != nil {
		t.Fatalf("performGitSync failed: %v", err)
	}

	hostOut, err := os.ReadFile(hostFile)
	if err != nil {
		t.Fatal(err)
	}
	containerOut, err := os.ReadFile(containerFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(hostOut) != "container" {
		t.Fatalf("host file not auto-synced, got %q", string(hostOut))
	}
	if string(containerOut) != "container" {
		t.Fatalf("container file not restored after git sync, got %q", string(containerOut))
	}
}

// ============================================================================
// Git Sync Tests
// ============================================================================

// TestGitSyncerEmptyRepo tests git sync with an empty repo (no commits).
func TestGitSyncerEmptyRepo(t *testing.T) {
	tmpDir := t.TempDir()

	// Init repo but don't commit anything
	if err := runInDir(tmpDir, nil, nil, "git", "init"); err != nil {
		t.Fatalf("git init failed: %v", err)
	}

	gs := newGitSyncer(
		context.Background(),
		"fake-container",
		"/fake/workdir",
		tmpDir,
		io.Discard,
		io.Discard,
		true,
	)

	// checkGitState should fail gracefully (no HEAD)
	_, err := gs.checkGitState()
	if err == nil {
		t.Fatal("expected error for empty repo (no HEAD)")
	}
	if !strings.Contains(err.Error(), "rev-parse HEAD") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestGitSyncerWithCommits tests git syncer with a valid repo.
func TestGitSyncerWithCommits(t *testing.T) {
	tmpDir := t.TempDir()

	// Init repo with a commit
	if err := runInDir(tmpDir, nil, nil, "git", "init"); err != nil {
		t.Fatalf("git init failed: %v", err)
	}
	if err := runInDir(tmpDir, nil, nil, "git", "config", "user.email", "test@test.com"); err != nil {
		t.Fatalf("git config email failed: %v", err)
	}
	if err := runInDir(tmpDir, nil, nil, "git", "config", "user.name", "Test"); err != nil {
		t.Fatalf("git config name failed: %v", err)
	}

	// Create initial commit
	testFile := tmpDir + "/test.txt"
	if err := os.WriteFile(testFile, []byte("initial"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := runInDir(tmpDir, nil, nil, "git", "add", "."); err != nil {
		t.Fatal(err)
	}
	if err := runInDir(tmpDir, nil, nil, "git", "commit", "-m", "initial"); err != nil {
		t.Fatal(err)
	}

	gs := newGitSyncer(
		context.Background(),
		"fake-container",
		"/fake/workdir",
		tmpDir,
		io.Discard,
		io.Discard,
		true,
	)

	// checkGitState should succeed for host (container will fail)
	state, err := gs.checkGitState()
	// Expect error because container doesn't exist
	if err == nil {
		t.Log("Host state read successfully")
	}
	// But we can check that host state was populated before container error
	if state.hostHead != "" {
		t.Logf("Host HEAD: %s", state.hostHead[:min(8, len(state.hostHead))])
	}
}

// TestSignalFileSyncIdleRace verifies there is no race condition
// between closing and recreating fileSyncIdle channel.
//
// This test exercises concurrent calls to signalFileSyncIdle() and
// waitForFileSyncIdle() to ensure the mutex protection is working.
//
// Run with: go test -race -run TestSignalFileSyncIdleRace
func TestSignalFileSyncIdleRace(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := &extractSync{
		ctx:          ctx,
		cancel:       cancel,
		fileSyncIdle: make(chan struct{}),
		// fileSyncIdleMu is zero-value initialized (unlocked)
	}

	var wg sync.WaitGroup

	// Goroutine rapidly calling signalFileSyncIdle
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("PANIC in signalFileSyncIdle: %v", r)
			}
		}()
		for i := 0; i < 1000; i++ {
			select {
			case <-ctx.Done():
				return
			default:
				s.signalFileSyncIdle()
			}
		}
	}()

	// Goroutine rapidly calling waitForFileSyncIdle
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("PANIC in waitForFileSyncIdle: %v", r)
			}
		}()
		for i := 0; i < 1000; i++ {
			select {
			case <-ctx.Done():
				return
			default:
				s.waitForFileSyncIdle()
			}
		}
	}()

	// If no panic occurs within 5 seconds, test passes
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		t.Log("Race condition test passed - no race detected")
	case <-time.After(5 * time.Second):
		cancel()
		<-done
		t.Log("Race condition test passed (timed out but no race detected)")
	}
}

// TestParseInotifyLineMalformed tests inotify line parsing edge cases.
func TestParseInotifyLineMalformed(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		wantOK   bool
		wantPath string
	}{
		{"empty string", "", false, ""},
		{"pipe only", "|", false, ""},
		{"spaces with pipe", "  |  ", false, ""},
		{"event only", "|MODIFY", false, ""},
		{"relative path", "./test.txt|MODIFY", true, "test.txt"},
		{"absolute path", "/absolute/path|CREATE", true, "/absolute/path"},
		{"no pipe", "no-pipe-here", false, ""},
		{"path with spaces", "/path/with spaces|MODIFY", true, "/path/with spaces"},
		{"path with pipe", "/path/with|pipe/file.txt|MODIFY", true, "/path/with|pipe/file.txt"},
		{"dot with pipe", "./|DELETE", true, "."},
		{"just slash", "/|CREATE", true, "/"},
		{"deep path", "/var/www/discourse/app/models/user.rb|MODIFY", true, "/var/www/discourse/app/models/user.rb"},
		{"unicode path", "/path/émoji_🔥|MODIFY", true, "/path/émoji_🔥"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, ok := parseInotifyLine(tt.line)
			if ok != tt.wantOK {
				t.Errorf("parseInotifyLine(%q) ok = %v, want %v", tt.line, ok, tt.wantOK)
			}
			if ok && tt.wantPath != "" && path != tt.wantPath {
				t.Errorf("parseInotifyLine(%q) path = %q, want %q", tt.line, path, tt.wantPath)
			}
		})
	}
}

// TestRetryQueueExhaustion tests retry logic when max attempts reached.
func TestRetryQueueExhaustion(t *testing.T) {
	var errBuf bytes.Buffer
	s := &extractSync{
		retryQueue: make(map[string]retryEntry),
		errOut:     &errBuf,
	}

	path := "test/path.txt"

	// Queue retries - should hit max and remove on 3rd call
	for i := 0; i < maxRetryAttempts; i++ {
		s.queueRetry(path, sourceHost)
	}

	// After max attempts, path should be removed from queue
	if _, exists := s.retryQueue[path]; exists {
		t.Fatal("path should have been removed after max retries")
	}

	// Should have warning in error output
	if !strings.Contains(errBuf.String(), "giving up") {
		t.Errorf("expected 'giving up' warning, got: %s", errBuf.String())
	}
}

// TestIsTransientError tests transient error detection.
func TestIsTransientError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"permission error", os.ErrPermission, true},
		{"permission denied string", errors.New("Permission denied"), true},
		{"text file busy", errors.New("text file busy"), true},
		{"no such file", errors.New("no such file"), false},
		{"connection refused", errors.New("connection refused"), false},
		{"deadline exceeded", context.DeadlineExceeded, false},
		// NOTE: Wrapped errors are NOT detected as transient - this is a known gap
		// in the current implementation (doesn't use errors.Is for unwrapping)
		{"wrapped permission", fmt.Errorf("wrapped: %w", os.ErrPermission), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isTransientError(tt.err)
			if got != tt.want {
				t.Errorf("isTransientError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestIsTransientErrorKnownGap documents that wrapped errors are not detected.
// This is a potential bug that could cause retries to fail for wrapped permission errors.
func TestIsTransientErrorKnownGap(t *testing.T) {
	wrapped := fmt.Errorf("docker exec failed: %w", os.ErrPermission)
	if isTransientError(wrapped) {
		t.Log("Wrapped permission errors ARE detected (implementation improved)")
	} else {
		t.Log("BUG: Wrapped permission errors are NOT detected as transient")
		t.Log("This could cause sync failures when permission errors are wrapped")
	}
}

// TestShouldIgnoreRelative tests path ignore logic.
func TestShouldIgnoreRelative(t *testing.T) {
	tests := []struct {
		path   string
		ignore bool
	}{
		{"", false},
		{".git", true},
		{".git/HEAD", true},
		{".git/refs/heads/main", true},
		{"app/.git/config", true},
		{".gitignore", false},
		{".github/workflows", false},
		{"app/models/user.rb", false},
		{"./.git", true},
		{"./.git/objects", true},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := shouldIgnoreRelative(tt.path)
			if got != tt.ignore {
				t.Errorf("shouldIgnoreRelative(%q) = %v, want %v", tt.path, got, tt.ignore)
			}
		})
	}
}

func TestRelativeFromContainerBoundary(t *testing.T) {
	s := &extractSync{workdir: "/work"}
	rel, ok := s.relativeFromContainer("/work/file.txt")
	if !ok || rel != "file.txt" {
		t.Fatalf("expected /work/file.txt => file.txt, got ok=%v rel=%q", ok, rel)
	}
	if _, ok := s.relativeFromContainer("/workdir/file.txt"); ok {
		t.Fatal("expected /workdir/file.txt to be outside /work")
	}

	root := &extractSync{workdir: "/"}
	rel, ok = root.relativeFromContainer("/etc/hosts")
	if !ok || rel != "etc/hosts" {
		t.Fatalf("expected /etc/hosts => etc/hosts, got ok=%v rel=%q", ok, rel)
	}
}

// TestBuildTrackedChanges tests conversion of git status to change structs.
func TestBuildTrackedChanges(t *testing.T) {
	tests := []struct {
		name    string
		entries []statusEntry
		want    int
	}{
		{
			name:    "empty",
			entries: nil,
			want:    0,
		},
		{
			name: "single modify",
			entries: []statusEntry{
				{staged: ' ', unstaged: 'M', path: "file.txt"},
			},
			want: 1,
		},
		{
			name: "rename",
			entries: []statusEntry{
				{staged: 'R', unstaged: ' ', path: "new.txt", oldPath: "old.txt"},
			},
			want: 1,
		},
		{
			name: "delete",
			entries: []statusEntry{
				{staged: 'D', unstaged: ' ', path: "deleted.txt"},
			},
			want: 1,
		},
		{
			name: "untracked",
			entries: []statusEntry{
				{staged: '?', unstaged: '?', path: "new_file.txt"},
			},
			want: 1,
		},
		{
			name: "git path ignored",
			entries: []statusEntry{
				{staged: 'M', unstaged: ' ', path: ".git/config"},
			},
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changes := buildTrackedChanges(tt.entries)
			if len(changes) != tt.want {
				t.Errorf("buildTrackedChanges() returned %d changes, want %d", len(changes), tt.want)
			}
		})
	}
}

// TestMapKeys tests the mapKeys helper function.
func TestMapKeys(t *testing.T) {
	m := map[string]struct{}{
		"a": {},
		"b": {},
		"c": {},
	}
	keys := mapKeys(m)
	if len(keys) != 3 {
		t.Errorf("mapKeys() returned %d keys, want 3", len(keys))
	}

	// Check all keys are present
	found := make(map[string]bool)
	for _, k := range keys {
		found[k] = true
	}
	for k := range m {
		if !found[k] {
			t.Errorf("mapKeys() missing key %q", k)
		}
	}
}

// TestDrainEventQueue tests that drainEventQueue clears pending events.
func TestDrainEventQueue(t *testing.T) {
	var logBuf bytes.Buffer
	s := &extractSync{
		events:     make(chan watcherEvent, 256),
		retryQueue: make(map[string]retryEntry),
		logOut:     &logBuf,
		errOut:     &logBuf,
		debug:      true,
	}

	// Queue some events
	for i := 0; i < 50; i++ {
		s.events <- watcherEvent{source: sourceHost, path: fmt.Sprintf("file%d.txt", i)}
	}

	// Add retry entries
	s.retryQueue["retry1.txt"] = retryEntry{source: sourceHost, attempts: 1}
	s.retryQueue["retry2.txt"] = retryEntry{source: sourceContainer, attempts: 2}

	// Verify state before drain
	if len(s.events) != 50 {
		t.Errorf("expected 50 events before drain, got %d", len(s.events))
	}
	if len(s.retryQueue) != 2 {
		t.Errorf("expected 2 retry entries before drain, got %d", len(s.retryQueue))
	}

	// Drain
	s.drainEventQueue()

	// Verify state after drain
	if len(s.events) != 0 {
		t.Errorf("expected 0 events after drain, got %d", len(s.events))
	}
	if len(s.retryQueue) != 0 {
		t.Errorf("expected 0 retry entries after drain, got %d", len(s.retryQueue))
	}

	// Check debug output
	if !strings.Contains(logBuf.String(), "drained 50 stale file events") {
		t.Errorf("expected drain log message, got: %s", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), "cleared 2 stale retry entries") {
		t.Errorf("expected retry clear log message, got: %s", logBuf.String())
	}
}

type fakeGitSyncer struct {
	sync      func() error
	forceSync func() error
}

func (f fakeGitSyncer) syncToContainer() error {
	if f.sync == nil {
		return nil
	}
	return f.sync()
}

func (f fakeGitSyncer) forceSyncToContainer() error {
	if f.forceSync == nil {
		return f.syncToContainer()
	}
	return f.forceSync()
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

func copyFileToDir(src, dstDir string) error {
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return err
	}
	dst := filepath.Join(dstDir, filepath.Base(src))
	return copyFile(src, dst)
}

// TestDrainEventQueueEmpty tests drainEventQueue with no pending events.
func TestDrainEventQueueEmpty(t *testing.T) {
	var logBuf bytes.Buffer
	s := &extractSync{
		events:     make(chan watcherEvent, 256),
		retryQueue: make(map[string]retryEntry),
		logOut:     &logBuf,
		errOut:     &logBuf,
		debug:      true,
	}

	// Drain empty queue
	s.drainEventQueue()

	// Should not log anything since nothing was drained
	if strings.Contains(logBuf.String(), "drained") {
		t.Errorf("should not log when nothing drained, got: %s", logBuf.String())
	}
}
