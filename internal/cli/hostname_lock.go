package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"dv/internal/xdg"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
)

// Serialize desired-state mutations with their live application. The config
// transaction lock alone cannot protect a later proxy delete or service update
// from racing a re-add. Use a separate lock because application calls config.Update.
func withHostnameOperationLock(cmd *cobra.Command, run func() error) error {
	dir, err := xdg.ConfigDir()
	if err != nil {
		return err
	}
	return withHostnameOperationLockAt(cmd, dir, run)
}

func withHostnameOperationLockAt(cmd *cobra.Command, dir string, run func() error) error {
	unlock, err := acquireHostnameOperationLock(cmd, dir)
	if err != nil {
		return err
	}
	defer unlock()
	return run()
}

func acquireHostnameOperationLock(cmd *cobra.Command, dir string) (func(), error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "hostname-operations.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	acquired := false
	defer func() {
		if !acquired {
			_ = lock.Close()
		}
	}()
	ctx := context.Background()
	if cmd != nil && cmd.Context() != nil {
		ctx = cmd.Context()
	}
	wait, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return nil, err
		}
		select {
		case <-wait.Done():
			return nil, fmt.Errorf("waiting for another hostname operation: %w", wait.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
	acquired = true
	var once sync.Once
	return func() { once.Do(func() { _ = unix.Flock(int(lock.Fd()), unix.LOCK_UN); _ = lock.Close() }) }, nil
}
