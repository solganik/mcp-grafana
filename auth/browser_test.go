package auth

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestRemoveStaleChromeLock_NoLock(t *testing.T) {
	dir := t.TempDir()
	// Should not panic or error when no lock exists.
	removeStaleChromeLock(dir)
}

func TestRemoveStaleChromeLock_StaleLock(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "SingletonLock")
	// Create a symlink pointing to a hostname-pid with a dead PID.
	if err := os.Symlink("myhost-999999999", lockPath); err != nil {
		t.Fatal(err)
	}
	removeStaleChromeLock(dir)
	if _, err := os.Lstat(lockPath); !os.IsNotExist(err) {
		t.Fatal("expected stale SingletonLock to be removed")
	}
}

func TestRemoveStaleChromeLock_LiveProcess(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "SingletonLock")
	// Use our own PID — guaranteed to be alive.
	target := "myhost-" + strconv.Itoa(os.Getpid())
	if err := os.Symlink(target, lockPath); err != nil {
		t.Fatal(err)
	}
	removeStaleChromeLock(dir)
	if _, err := os.Lstat(lockPath); err != nil {
		t.Fatal("expected SingletonLock to be kept for live process")
	}
}

func TestRemoveStaleChromeLock_InvalidTarget(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "SingletonLock")
	// Symlink with no parseable PID — should be removed.
	if err := os.Symlink("garbage", lockPath); err != nil {
		t.Fatal(err)
	}
	removeStaleChromeLock(dir)
	if _, err := os.Lstat(lockPath); !os.IsNotExist(err) {
		t.Fatal("expected unparseable SingletonLock to be removed")
	}
}
