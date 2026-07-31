package cmd

import (
	"errors"
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/lock"
)

func TestAcquireIdentityLockUsesResolvedTmuxOwner(t *testing.T) {
	original := resolveIdentityLockOwner
	t.Cleanup(func() { resolveIdentityLockOwner = original })

	owner := lock.Owner{PID: os.Getpid(), SessionID: "gt-worker"}
	resolveIdentityLockOwner = func() (lock.Owner, error) { return owner, nil }
	ctx := RoleContext{Role: RolePolecat, Rig: "test-rig", Polecat: "worker", WorkDir: t.TempDir()}

	if err := acquireIdentityLock(ctx); err != nil {
		t.Fatalf("first acquireIdentityLock() error = %v", err)
	}
	if err := acquireIdentityLock(ctx); err != nil {
		t.Fatalf("second acquireIdentityLock() error = %v", err)
	}

	info, err := lock.New(ctx.WorkDir).Read()
	if err != nil {
		t.Fatal(err)
	}
	if info.PID != owner.PID || info.SessionID != owner.SessionID {
		t.Fatalf("lock owner = (%d, %q), want (%d, %q)", info.PID, info.SessionID, owner.PID, owner.SessionID)
	}
}

func TestAcquireIdentityLockFailsClosedWhenTmuxOwnerCannotResolve(t *testing.T) {
	original := resolveIdentityLockOwner
	t.Cleanup(func() { resolveIdentityLockOwner = original })

	resolveIdentityLockOwner = func() (lock.Owner, error) { return lock.Owner{}, errors.New("no pane ancestor") }
	ctx := RoleContext{Role: RolePolecat, Rig: "test-rig", Polecat: "worker", WorkDir: t.TempDir()}

	if err := acquireIdentityLock(ctx); err == nil {
		t.Fatal("acquireIdentityLock() error = nil, want owner resolution failure")
	}
}
