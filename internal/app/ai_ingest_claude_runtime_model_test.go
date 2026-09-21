package app

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/crevissepartners/projmux/internal/app/usagecmd"
	"github.com/crevissepartners/projmux/internal/core/usage/runtimemodel"
	"github.com/crevissepartners/projmux/internal/diagnostics"
)

// ai_ingest_claude_runtime_model_test.go covers the runtime model sidecar the
// Claude hook ingest writes for the usage HUD: which events feed it, that an
// empty identifier never erases an earlier observation, and that the writer
// follows PROJMUX_USAGE_STATE_DIR the way the HUD reader does.

// runtimeModelFixture is an unowned quiet-hook fixture whose usage state dir is
// redirected to a per-test directory.
func runtimeModelFixture(t *testing.T) (*claudeQuietHookFixture, string) {
	t.Helper()
	f := newClaudeQuietHookFixture(t, false)
	stateDir := filepath.Join(t.TempDir(), "usage")
	inner := f.cmd.lookupEnv
	f.cmd.lookupEnv = func(name string) string {
		if name == usagecmd.StateDirEnvVar {
			return stateDir
		}
		return inner(name)
	}
	return f, stateDir
}

// ingestWith runs one Claude hook event with extra payload fields.
func (f *claudeQuietHookFixture) ingestWith(t *testing.T, event string, extra map[string]any) {
	t.Helper()
	fields := map[string]any{
		"hook_event_name": event,
		"session_id":      claudeQuietHookSession,
		"cwd":             claudeQuietHookCWD,
		"transcript_path": claudeQuietHookTranscript,
	}
	maps.Copy(fields, extra)
	payload, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.cmd.ingestClaudeHook(payload, f.explicitPane); err != nil {
		t.Fatalf("ingest claude %s: %v", event, err)
	}
}

func TestParseClaudeHookPayloadReadsRuntimeModelFields(t *testing.T) {
	t.Parallel()

	payload, err := parseClaudeHookPayload([]byte(`{"hook_event_name":"SessionStart","session_id":"s","cwd":"/repo","source":"startup","model":"claude-opus-5"}`))
	if err != nil {
		t.Fatal(err)
	}
	if payload.RuntimeModel != "claude-opus-5" || payload.FromRuntimeModel != "" || payload.ToRuntimeModel != "" {
		t.Fatalf("SessionStart runtime model fields = %+v", payload)
	}
	// The observed Claude Code 2.1.278 PostModelSwitch payload, verbatim shape.
	payload, err = parseClaudeHookPayload([]byte(`{"session_id":"s","transcript_path":"/t.jsonl","cwd":"/repo","prompt_id":"p","hook_event_name":"PostModelSwitch","from_model":"claude-sonnet-5","to_model":"claude-opus-5","requested_model":"opus","source":"command","context_tokens":0,"prompt_cache_warm":false,"cache_ttl":"1h","estimated_cache_write_usd":0,"pricing":"catalog"}`))
	if err != nil {
		t.Fatal(err)
	}
	if payload.EventName != "PostModelSwitch" || payload.FromRuntimeModel != "claude-sonnet-5" || payload.ToRuntimeModel != "claude-opus-5" || payload.RuntimeModel != "" {
		t.Fatalf("PostModelSwitch runtime model fields = %+v", payload)
	}
	metadata := payload.claudeMetadata()
	if metadata["from_model"] != "claude-sonnet-5" || metadata["to_model"] != "claude-opus-5" {
		t.Fatalf("metadata = %#v", metadata)
	}
	if _, ok := metadata["model"]; ok {
		t.Fatalf("empty model must be dropped from metadata: %#v", metadata)
	}
}

func TestClaudeSessionStartWithModelWritesTheRuntimeModelSidecar(t *testing.T) {
	t.Parallel()

	f, stateDir := runtimeModelFixture(t)
	f.ingestWith(t, "SessionStart", map[string]any{"source": "startup", "model": "claude-opus-5"})

	rec, ok := runtimemodel.Read(stateDir, "claude")
	if !ok {
		t.Fatalf("sidecar not written under %s", stateDir)
	}
	if rec.Provider != "claude" || rec.Model != "claude-opus-5" || rec.Source != "SessionStart" || rec.SessionID != claudeQuietHookSession || rec.PaneID != claudeQuietHookPane || !rec.ObservedAt.Equal(claudeQuietHookClock) {
		t.Fatalf("sidecar = %+v", rec)
	}
	records := claudeQuietHookLogRecords(t, f.cmd)
	if len(records) != 1 || records[0].Event != "SessionStart" || records[0].Result != "quiet" {
		t.Fatalf("SessionStart stays a quiet event: %+v", records)
	}
}

// TestClaudeSessionStartWithoutModelKeepsTheEarlierObservation: Claude Code
// 2.1.278 ships SessionStart without `model` (observed on `source: startup`),
// and the docs say it is omitted after /clear too. That must not blank the HUD.
func TestClaudeSessionStartWithoutModelKeepsTheEarlierObservation(t *testing.T) {
	t.Parallel()

	f, stateDir := runtimeModelFixture(t)
	f.ingestWith(t, "PostModelSwitch", map[string]any{"from_model": "claude-sonnet-5", "to_model": "claude-opus-5", "source": "command"})
	f.ingestWith(t, "SessionStart", map[string]any{"source": "clear"})

	rec, ok := runtimemodel.Read(stateDir, "claude")
	if !ok || rec.Model != "claude-opus-5" || rec.Source != "PostModelSwitch" {
		t.Fatalf("sidecar after a model-less SessionStart = %+v, %v", rec, ok)
	}
}

func TestClaudePostModelSwitchFollowsTheModelAndStaysQuiet(t *testing.T) {
	t.Parallel()

	f, stateDir := runtimeModelFixture(t)
	f.ingestWith(t, "SessionStart", map[string]any{"model": "claude-sonnet-5"})
	f.ingestWith(t, "PreModelSwitch", map[string]any{"from_model": "claude-sonnet-5", "to_model": "claude-opus-5", "source": "command"})
	if rec, _ := runtimemodel.Read(stateDir, "claude"); rec.Model != "claude-sonnet-5" {
		t.Fatalf("PreModelSwitch must not record a switch that can still be blocked: %+v", rec)
	}
	f.ingestWith(t, "PostModelSwitch", map[string]any{"from_model": "claude-sonnet-5", "to_model": "claude-opus-5", "source": "command"})
	rec, ok := runtimemodel.Read(stateDir, "claude")
	if !ok || rec.Model != "claude-opus-5" || rec.Source != "PostModelSwitch" {
		t.Fatalf("sidecar after PostModelSwitch = %+v, %v", rec, ok)
	}
	// An automatic fallback reaches PostModelSwitch only; it is followed too.
	f.ingestWith(t, "PostModelSwitch", map[string]any{"from_model": "claude-opus-5", "to_model": "claude-sonnet-5", "requested_model": nil, "source": "auto"})
	if rec, _ := runtimemodel.Read(stateDir, "claude"); rec.Model != "claude-sonnet-5" {
		t.Fatalf("automatic fallback not followed: %+v", rec)
	}

	records := claudeQuietHookLogRecords(t, f.cmd)
	if len(records) != 4 {
		t.Fatalf("records = %+v", records)
	}
	for _, record := range records {
		if record.Result != "quiet" {
			t.Fatalf("model switch events must stay quiet (no notify row): %+v", record)
		}
	}
	if records[1].Event != "PreModelSwitch" || records[2].Event != "PostModelSwitch" {
		t.Fatalf("events = %+v", records)
	}
	// Quiet means no notify queue write and no status change.
	if store := f.cmd.notifyStore.(*stubNotifyStore); len(store.pushed) != 0 {
		t.Fatalf("model switch pushed notify rows: %+v", store.pushed)
	}
}

func TestClaudeStopRecordsTheTranscriptModelWhenNoPayloadCarriesOne(t *testing.T) {
	t.Parallel()

	f, stateDir := runtimeModelFixture(t)
	transcript := filepath.Join(t.TempDir(), "transcript.jsonl")
	content := `{"type":"user","message":{"role":"user","content":"reply ok"}}
{"type":"assistant","message":{"model":"claude-sonnet-5","role":"assistant","content":[{"type":"text","text":"ok"}]}}
`
	if err := os.WriteFile(transcript, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	f.ingestWith(t, "Stop", map[string]any{"transcript_path": transcript, "last_assistant_message": "ok"})

	rec, ok := runtimemodel.Read(stateDir, "claude")
	if !ok || rec.Model != "claude-sonnet-5" || rec.Source != claudeRuntimeModelSourceTranscript {
		t.Fatalf("sidecar after Stop = %+v, %v", rec, ok)
	}
	// The Stop handler's own outcome is unchanged by the sidecar write.
	records := claudeQuietHookLogRecords(t, f.cmd)
	if len(records) != 1 || records[0].Event != "Stop" || records[0].Result == "error" {
		t.Fatalf("Stop records = %+v", records)
	}
}

func TestClaudeRuntimeModelWriteFailureNeverFailsTheHook(t *testing.T) {
	t.Parallel()

	f, stateDir := runtimeModelFixture(t)
	// The state dir path is occupied by a regular file, so MkdirAll fails.
	if err := os.WriteFile(stateDir, []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.ingestWith(t, "PostModelSwitch", map[string]any{"to_model": "claude-opus-5"})
	records := claudeQuietHookLogRecords(t, f.cmd)
	if len(records) != 1 || records[0].Result != "quiet" {
		t.Fatalf("a sidecar write failure leaked into the hook outcome: %+v", records)
	}
}

func TestClaudeTranscriptReaderReturnsTextAndModelIndependently(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	// The newest assistant line is a tool call (no text) on opus; the newest
	// text is older and was answered by sonnet. Text and model are searched
	// independently, so the newest of EACH is returned.
	content := `{"type":"assistant","message":{"model":"claude-sonnet-5","role":"assistant","content":[{"type":"text","text":"first"}]}}
{"type":"user","message":{"role":"user","content":"go on","model":"not-an-assistant-line"}}
{"type":"assistant","message":{"model":"claude-opus-5","role":"assistant","content":[{"type":"tool_use","name":"Bash","input":{}}]}}
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	text, model := readClaudeTranscriptLastAssistant(path)
	if text != "first" || model != "claude-opus-5" {
		t.Fatalf("readClaudeTranscriptLastAssistant() = %q, %q", text, model)
	}
	if text, model := readClaudeTranscriptLastAssistant(filepath.Join(t.TempDir(), "missing.jsonl")); text != "" || model != "" {
		t.Fatalf("missing transcript = %q, %q", text, model)
	}
}

func TestClaudeModelSwitchEventsAreInstalledAndClassified(t *testing.T) {
	t.Parallel()

	installed := map[string]bool{}
	for _, event := range defaultAIHookInstallEvents(aiHookProviderClaude) {
		installed[event] = true
	}
	for _, event := range []string{"PreModelSwitch", "PostModelSwitch"} {
		if !installed[event] {
			t.Fatalf("%s is not in the default Claude install catalog", event)
		}
		if got := classifyAIHookKind(diagnostics.ProviderClaude, event); got != diagnostics.AIKindSession {
			t.Fatalf("classifyAIHookKind(%s) = %q, want %q", event, got, diagnostics.AIKindSession)
		}
	}
}
