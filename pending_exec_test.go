package main

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// newPersistBot builds a Bot wired only for pending-exec persistence (no Telegram
// API), so we can exercise load/save without a live token.
func newPersistBot(path string) *Bot {
	return &Bot{
		pendingExec:     make(map[int64]string),
		pendingExecPath: path,
		mu:              sync.Mutex{},
	}
}

// TestPendingExecSurvivesRestart is the core guarantee: a plan stored before a
// restart is still confirmable afterwards (eng-3c — fixes "No pending plan to confirm.").
func TestPendingExecSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "pending_exec.json")

	b1 := newPersistBot(path)
	b1.mu.Lock()
	b1.pendingExec[12345] = "Resume the harness-loop."
	b1.pendingExec[-67890] = "Another plan."
	b1.savePendingExecLocked()
	b1.mu.Unlock()

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("store file not written: %v", err)
	}

	// Simulate a fresh process pointed at the same store.
	b2 := newPersistBot(path)
	b2.loadPendingExec()

	if got := b2.pendingExec[12345]; got != "Resume the harness-loop." {
		t.Errorf("chat 12345: got %q, want restored plan", got)
	}
	if got := b2.pendingExec[-67890]; got != "Another plan." {
		t.Errorf("negative chat id not restored: got %q", got)
	}
	if len(b2.pendingExec) != 2 {
		t.Errorf("expected 2 restored plans, got %d", len(b2.pendingExec))
	}
}

// TestPendingExecDeletePersists confirms consuming a plan (e.g. via /confirm) clears
// it from disk too, so a restart doesn't resurrect an already-confirmed plan.
func TestPendingExecDeletePersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pending_exec.json")

	b1 := newPersistBot(path)
	b1.mu.Lock()
	b1.pendingExec[111] = "plan"
	b1.savePendingExecLocked()
	delete(b1.pendingExec, 111)
	b1.savePendingExecLocked()
	b1.mu.Unlock()

	b2 := newPersistBot(path)
	b2.loadPendingExec()
	if len(b2.pendingExec) != 0 {
		t.Errorf("deleted plan resurrected after reload: %v", b2.pendingExec)
	}
}

// TestPendingExecMissingFileIsClean verifies first-run (no store file) is non-fatal.
func TestPendingExecMissingFileIsClean(t *testing.T) {
	b := newPersistBot(filepath.Join(t.TempDir(), "does_not_exist.json"))
	b.loadPendingExec()
	if len(b.pendingExec) != 0 {
		t.Errorf("expected empty map on missing store, got %v", b.pendingExec)
	}
}

// TestPendingExecCorruptFileIsClean verifies a corrupt store doesn't crash startup.
func TestPendingExecCorruptFileIsClean(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pending_exec.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := newPersistBot(path)
	b.loadPendingExec()
	if len(b.pendingExec) != 0 {
		t.Errorf("corrupt store should yield empty map, got %v", b.pendingExec)
	}
}
