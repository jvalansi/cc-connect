package core

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLastActiveRoundTrip(t *testing.T) {
	dir := t.TempDir()

	// No file yet → nil.
	if got := ConsumeLastActive(dir); got != nil {
		t.Fatalf("ConsumeLastActive on empty dir = %+v, want nil", got)
	}

	req := RestartRequest{SessionKey: "slack:C123:167.89", Platform: "slack"}
	if err := SaveLastActive(dir, req); err != nil {
		t.Fatalf("SaveLastActive: %v", err)
	}

	got := ConsumeLastActive(dir)
	if got == nil {
		t.Fatal("ConsumeLastActive returned nil after save")
	}
	if got.SessionKey != req.SessionKey || got.Platform != req.Platform {
		t.Fatalf("round-trip = %+v, want %+v", got, req)
	}

	// Consume deletes the file → second read is nil.
	if got := ConsumeLastActive(dir); got != nil {
		t.Fatalf("ConsumeLastActive after consume = %+v, want nil", got)
	}
}

func TestConsumeLastActiveRejectsIncomplete(t *testing.T) {
	dir := t.TempDir()
	runDir := filepath.Join(dir, "run")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Missing Platform → treated as absent.
	if err := os.WriteFile(filepath.Join(runDir, "last_active"), []byte(`{"session_key":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ConsumeLastActive(dir); got != nil {
		t.Fatalf("ConsumeLastActive on incomplete record = %+v, want nil", got)
	}
}

func TestRecordLastActiveDisabledByDefault(t *testing.T) {
	dir := t.TempDir()
	e := &Engine{}
	// Not enabled → nothing persisted.
	e.recordLastActive("slack", "slack:C1:1.2")
	if got := ConsumeLastActive(dir); got != nil {
		t.Fatalf("recordLastActive persisted while disabled: %+v", got)
	}
}

func TestRecordLastActivePersistsAndDedupes(t *testing.T) {
	dir := t.TempDir()
	e := &Engine{}
	e.EnableBackOnlineNotify(dir)

	e.recordLastActive("slack", "slack:C1:1.2")
	got := ConsumeLastActive(dir)
	if got == nil || got.SessionKey != "slack:C1:1.2" || got.Platform != "slack" {
		t.Fatalf("first record not persisted correctly: %+v", got)
	}

	// Same key again should not rewrite (dedupe): after consuming above, the
	// file is gone, and recording the identical key is skipped, so it stays gone.
	e.recordLastActive("slack", "slack:C1:1.2")
	if got := ConsumeLastActive(dir); got != nil {
		t.Fatalf("duplicate key rewrote file: %+v", got)
	}

	// A new key writes again.
	e.recordLastActive("telegram", "telegram:999:5")
	got = ConsumeLastActive(dir)
	if got == nil || got.SessionKey != "telegram:999:5" || got.Platform != "telegram" {
		t.Fatalf("new key not persisted: %+v", got)
	}
}
