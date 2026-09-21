// Package runtimemodel persists the runtime model identifier an AI provider
// reported through its official hook events, so the usage HUD can print it
// next to the provider label.
//
// Vocabulary: in projmux "model" usually means the usage provider (`--model
// claude`, `Snapshot.Model`). This package is about the OTHER thing, the
// provider's own runtime model (`claude-opus-5`), which is why every name here
// says "runtime model" and never plain "model".
//
// The record is a best-effort sidecar in the usage state directory, following
// the precedent PR #486 set for the Antigravity context sidecar: hook ingest
// writes it, the HUD reads it, and a missing or malformed file degrades to
// "no runtime model" rather than an error. The identifier is stored verbatim
// and only bounded and escaped at render time; projmux never maps it to a
// display alias.
package runtimemodel

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// FileNamePrefix is the sidecar file name prefix; the provider key follows.
const FileNamePrefix = "runtime-model-"

// Record is one runtime model observation.
//
// Several panes of the same provider can report different runtime models. The
// sidecar keeps ONE record per provider, and the most recent observation wins,
// so with two Claude panes on different models the HUD shows whichever one
// reported last. PR #486's context sidecar documented the same limitation.
type Record struct {
	// Provider is the canonical usage provider key (`claude`).
	Provider string `json:"provider"`
	// Model is the runtime model identifier exactly as the hook payload or
	// transcript spelled it (`claude-opus-5`). Never a display alias.
	Model string `json:"model"`
	// Source names the hook event or transcript field the value came from
	// (`SessionStart`, `PostModelSwitch`, `transcript`).
	Source string `json:"source"`
	// SessionID and PaneID identify the observation for diagnostics.
	SessionID string `json:"session_id,omitempty"`
	PaneID    string `json:"pane_id,omitempty"`
	// ObservedAt is the ingest wall clock, UTC.
	ObservedAt time.Time `json:"observed_at"`
}

// FilePath returns the sidecar path for a provider inside the usage state
// directory (the directory `snapshots.json` lives in).
func FilePath(stateDir, provider string) string {
	return filepath.Join(stateDir, FileNamePrefix+strings.ToLower(strings.TrimSpace(provider))+".json")
}

// Write persists rec atomically. An empty Model or Provider is refused so a
// hook that carried no identifier cannot erase an earlier observation.
func Write(stateDir string, rec Record) error {
	rec.Provider = strings.ToLower(strings.TrimSpace(rec.Provider))
	rec.Model = strings.TrimSpace(rec.Model)
	if rec.Provider == "" || rec.Model == "" {
		return errors.New("runtimemodel: provider and model are required")
	}
	if strings.TrimSpace(stateDir) == "" {
		return errors.New("runtimemodel: state dir is required")
	}
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		return fmt.Errorf("runtimemodel: create state dir: %w", err)
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("runtimemodel: encode: %w", err)
	}
	path := FilePath(stateDir, rec.Provider)
	tmp, err := os.CreateTemp(stateDir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("runtimemodel: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("runtimemodel: write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("runtimemodel: close temp file: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("runtimemodel: chmod temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("runtimemodel: rename temp file: %w", err)
	}
	return nil
}

// Read returns the provider's record. ok is false when the sidecar is missing,
// unreadable, malformed, or carries an empty model: every failure mode reads
// as "no runtime model known", never as an error the HUD would have to show.
func Read(stateDir, provider string) (Record, bool) {
	if strings.TrimSpace(stateDir) == "" {
		return Record{}, false
	}
	// #nosec G304 -- path is derived from the resolved usage state dir and a catalog provider key.
	data, err := os.ReadFile(FilePath(stateDir, provider))
	if err != nil {
		return Record{}, false
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return Record{}, false
	}
	rec.Model = strings.TrimSpace(rec.Model)
	if rec.Model == "" {
		return Record{}, false
	}
	return rec, true
}
