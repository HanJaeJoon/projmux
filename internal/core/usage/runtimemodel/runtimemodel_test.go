package runtimemodel

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteThenReadRoundTrips(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "usage")
	observed := time.Date(2026, 9, 21, 4, 0, 0, 0, time.UTC)
	rec := Record{Provider: "Claude", Model: " claude-opus-5 ", Source: "PostModelSwitch", SessionID: "s-1", PaneID: "%3", ObservedAt: observed}
	if err := Write(dir, rec); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "runtime-model-claude.json")); err != nil {
		t.Fatalf("sidecar not created: %v", err)
	}
	got, ok := Read(dir, "claude")
	if !ok {
		t.Fatal("Read() ok = false, want true")
	}
	if got.Provider != "claude" || got.Model != "claude-opus-5" || got.Source != "PostModelSwitch" || got.SessionID != "s-1" || got.PaneID != "%3" || !got.ObservedAt.Equal(observed) {
		t.Fatalf("Read() = %+v", got)
	}
}

func TestWriteRefusesEmptyModelSoAnEarlierObservationSurvives(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := Write(dir, Record{Provider: "claude", Model: "claude-sonnet-5", Source: "SessionStart"}); err != nil {
		t.Fatalf("seed Write() error = %v", err)
	}
	if err := Write(dir, Record{Provider: "claude", Model: "   ", Source: "SessionStart"}); err == nil {
		t.Fatal("Write() with an empty model succeeded, want error")
	}
	if err := Write(dir, Record{Provider: "", Model: "claude-opus-5"}); err == nil {
		t.Fatal("Write() with an empty provider succeeded, want error")
	}
	got, ok := Read(dir, "claude")
	if !ok || got.Model != "claude-sonnet-5" {
		t.Fatalf("Read() = %+v, %v; want the seeded record", got, ok)
	}
}

func TestReadDegradesToNoModel(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if _, ok := Read(dir, "claude"); ok {
		t.Fatal("Read() of a missing sidecar ok = true")
	}
	if _, ok := Read("", "claude"); ok {
		t.Fatal("Read() with an empty state dir ok = true")
	}
	if err := os.WriteFile(FilePath(dir, "claude"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := Read(dir, "claude"); ok {
		t.Fatal("Read() of a malformed sidecar ok = true")
	}
	if err := os.WriteFile(FilePath(dir, "claude"), []byte(`{"provider":"claude","model":""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := Read(dir, "claude"); ok {
		t.Fatal("Read() of an empty-model sidecar ok = true")
	}
}

func TestLatestWriteWins(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	for _, model := range []string{"claude-sonnet-5", "claude-opus-5"} {
		if err := Write(dir, Record{Provider: "claude", Model: model, Source: "PostModelSwitch"}); err != nil {
			t.Fatalf("Write(%s) error = %v", model, err)
		}
	}
	got, _ := Read(dir, "claude")
	if got.Model != "claude-opus-5" {
		t.Fatalf("Read().Model = %q, want the last write", got.Model)
	}
}
