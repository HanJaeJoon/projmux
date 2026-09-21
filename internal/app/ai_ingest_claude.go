package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/crevissepartners/projmux/internal/app/usagecmd"
	"github.com/crevissepartners/projmux/internal/config"
	"github.com/crevissepartners/projmux/internal/core/notify"
	"github.com/crevissepartners/projmux/internal/core/usage/runtimemodel"
)

const claudeTranscriptTailLimit = 256 * 1024

type claudeHookPayload struct {
	EventName        string
	SessionID        string
	CWD              string
	TranscriptPath   string
	NotificationType string
	Message          string
	Prompt           string
	ToolName         string
	ToolUseID        string
	ToolInput        map[string]any
	ErrorType        string
	ErrorMessage     string
	SubagentType     string
	SubagentID       string
	TeammateName     string
	TeammateID       string
	TeammateContext  string
	// RuntimeModel is the `model` identifier a SessionStart payload may carry
	// (`claude-opus-5`); Claude Code omits it after /clear and on some
	// recoveries. FromRuntimeModel/ToRuntimeModel are PreModelSwitch and
	// PostModelSwitch's `from_model`/`to_model`. They are the provider's own
	// model, not the projmux usage "model" (the provider key).
	RuntimeModel     string
	FromRuntimeModel string
	ToRuntimeModel   string
}

func (c *aiCommand) ingestClaudeHook(data []byte, explicitPane string) error {
	payload, err := parseClaudeHookPayload(data)
	if err != nil {
		c.appendAIIngestLog(aiIngestLogEntry{Source: "claude-hook", Result: "error", Reason: aiIngestFailureReason(aiIngestReasonHookPayloadInvalid, err)})
		return err
	}

	paneID, matchReason := c.matchAIPane(aiPaneMatchInput{
		ExplicitPane: explicitPane,
		Provider:     aiModeClaude,
		CWD:          payload.CWD,
		SessionID:    payload.SessionID,
	})
	if paneID == "" {
		c.appendAIIngestLog(aiIngestLogEntry{Source: "claude-hook", Event: payload.EventName, Result: "ignored", Reason: aiIngestRecordReason(matchReason), CWD: payload.CWD, SessionID: payload.SessionID})
		return nil
	}
	defer c.flushPendingAgentSessionRef(paneID)

	binding, owned := c.markAIHookPaneBinding(paneID, aiModeClaude, payload.CWD, "", payload.SessionID, payload.TranscriptPath)
	metadata := payload.claudeMetadata()
	action := c.aiHookEffectiveAction(aiHookProviderClaude, payload.EventName)

	switch payload.EventName {
	case "SessionStart":
		if _, _, err := c.persistManagedAgentStartupReadiness(paneID, aiModeClaude); err != nil {
			c.appendAIIngestLog(claudeHookLogEntry(paneID, payload, "error", aiIngestFailureReason(aiIngestReasonReadinessWriteFailed, err)))
			return err
		}
		c.persistClaudeRuntimeModel(paneID, payload, payload.RuntimeModel, payload.EventName)
		c.quietClaudeHook(paneID, payload, aiIngestRecordReason(aiHookNoHandlerReason(action)))
		return nil
	case "PostModelSwitch":
		// The only event that follows the model through a session: /model,
		// an automatic fallback (source "auto") and a resume (source
		// "resume") all land here with the new identifier in to_model.
		c.persistClaudeRuntimeModel(paneID, payload, payload.ToRuntimeModel, payload.EventName)
		c.quietClaudeHook(paneID, payload, aiIngestRecordReason(aiHookNoHandlerReason(action)))
		return nil
	case "UserPromptSubmit":
		return c.ingestClaudeUserPromptSubmit(paneID, payload, metadata, action)
	case "Notification":
		return c.ingestClaudeNotification(paneID, payload, metadata, action)
	case "PermissionRequest":
		return c.ingestClaudePermissionRequest(paneID, payload, metadata, action)
	case "Stop":
		return c.ingestClaudeStop(paneID, payload, metadata, action)
	case "StopFailure":
		return c.ingestClaudeStopFailure(paneID, payload, metadata, action)
	case "SubagentStop":
		return c.ingestClaudeSubagentStop(paneID, payload, metadata, action)
	case "PostToolUse", "PostToolUseFailure", "PermissionDenied", "ElicitationResult":
		return c.ingestClaudeOperatorDialogClosed(paneID, payload, metadata, action, binding, owned)
	case "PreToolUse", "PostToolBatch", "UserPromptExpansion", "SubagentStart", "PreCompact", "PostCompact", "SessionEnd", "Setup", "TaskCreated", "TaskCompleted", "Elicitation", "ConfigChange", "InstructionsLoaded", "WorktreeCreate", "WorktreeRemove", "CwdChanged", "FileChanged", "PreModelSwitch":
		c.quietClaudeHook(paneID, payload, aiIngestRecordReason(aiHookNoHandlerReason(action)))
		return nil
	case "TeammateIdle":
		return c.ingestClaudeTeammateIdle(paneID, payload, metadata, action)
	default:
		c.quietClaudeHook(paneID, payload, aiIngestRecordReason(aiHookNoHandlerReason(action)))
		return nil
	}
}

func (c *aiCommand) ingestClaudeUserPromptSubmit(paneID string, payload claudeHookPayload, metadata map[string]string, action aiHookActionResolution) error {
	if action.Action == aiHookActionQuiet {
		c.quietClaudeHook(paneID, payload, aiIngestRecordReason(aiHookQuietReason(action)))
		return nil
	}
	if err := c.applyAIStatusWithNotify("thinking", paneID, attentionNotifyInput{
		Metadata:  metadata,
		BadgeKind: aiBadgeKindInProgress,
	}); err != nil {
		c.appendAIIngestLog(claudeHookLogEntry(paneID, payload, "error", aiIngestFailureReason(aiIngestReasonStatusApplyFailed, err)))
		return err
	}
	c.appendAIIngestLog(claudeHookLogEntry(paneID, payload, "state", ""))
	return nil
}

// ingestClaudeOperatorDialogClosed handles the events that follow an operator
// answer: a tool ran or failed after its permission dialog, a permission was
// denied, or an MCP elicitation returned. Only an Agent still recorded as
// awaiting its operator moves to in_progress, state only; every other Agent
// stays quiet and writes nothing. PostToolUse fires on every tool call, so the
// judgment reads only the binding the hook marking already loaded.
func (c *aiCommand) ingestClaudeOperatorDialogClosed(paneID string, payload claudeHookPayload, metadata map[string]string,
	action aiHookActionResolution, binding managedAgentBinding, owned bool,
) error {
	if !owned || binding.agent.Spec.Provider != aiModeClaude || !claudeAgentAwaitsOperator(binding.agent, c.sessionRefClock()()) {
		c.quietClaudeHook(paneID, payload, aiIngestRecordReason(aiHookNoHandlerReason(action)))
		return nil
	}
	if err := c.applyAIStatusStateOnly("thinking", paneID, attentionNotifyInput{
		Metadata:  metadata,
		BadgeKind: aiBadgeKindInProgress,
	}); err != nil {
		c.appendAIIngestLog(claudeHookLogEntry(paneID, payload, "error", aiIngestFailureReason(aiIngestReasonStatusApplyFailed, err)))
		return err
	}
	c.appendAIIngestLog(claudeHookLogEntry(paneID, payload, "state", ""))
	return nil
}

func (c *aiCommand) ingestClaudeNotification(paneID string, payload claudeHookPayload, metadata map[string]string, action aiHookActionResolution) error {
	if action.Action == aiHookActionQuiet {
		c.quietClaudeHook(paneID, payload, aiIngestRecordReason(aiHookQuietReason(action)))
		return nil
	}
	body := formatClaudeNotificationNotifyBody(payload)
	return c.emitClaudeHookStatus(paneID, payload, action, attentionNotifyInput{
		ID:        claudeNotifyID(payload),
		Text:      body.Text,
		Severity:  body.Severity,
		Metadata:  mergeAINotifyBodyMetadata(metadata, body),
		Force:     true,
		BadgeKind: aiBadgeKindForNotifyCategory(body.Category),
	})
}

func (c *aiCommand) ingestClaudePermissionRequest(paneID string, payload claudeHookPayload, metadata map[string]string, action aiHookActionResolution) error {
	if action.Action == aiHookActionQuiet {
		c.quietClaudeHook(paneID, payload, aiIngestRecordReason(aiHookQuietReason(action)))
		return nil
	}
	body := formatClaudePermissionNotifyBody(payload)
	return c.emitClaudeHookStatus(paneID, payload, action, attentionNotifyInput{
		ID:        claudePermissionNotifyID(payload),
		Text:      body.Text,
		Severity:  body.Severity,
		Metadata:  mergeAINotifyBodyMetadata(metadata, body),
		Force:     true,
		BadgeKind: aiBadgeKindApprovalRequired,
	})
}

func (c *aiCommand) ingestClaudeStop(paneID string, payload claudeHookPayload, metadata map[string]string, action aiHookActionResolution) error {
	if action.Action == aiHookActionQuiet {
		c.quietClaudeHook(paneID, payload, aiIngestRecordReason(aiHookQuietReason(action)))
		return nil
	}
	message, transcriptModel := readClaudeTranscriptLastAssistant(payload.TranscriptPath)
	// The Stop payload itself carries no model, but the transcript tail this
	// handler already reads records `message.model` on every assistant turn.
	// It covers the SessionStart payloads Claude Code ships without `model`.
	c.persistClaudeRuntimeModel(paneID, payload, transcriptModel, claudeRuntimeModelSourceTranscript)
	body := formatClaudeStopNotifyBody(message)
	return c.emitClaudeHookStatus(paneID, payload, action, attentionNotifyInput{
		ID:        claudeStopNotifyID(payload),
		Text:      body.Text,
		Severity:  body.Severity,
		Metadata:  mergeAINotifyBodyMetadata(metadata, body),
		Force:     true,
		BadgeKind: aiBadgeKindResponseComplete,
	})
}

func (c *aiCommand) ingestClaudeStopFailure(paneID string, payload claudeHookPayload, metadata map[string]string, action aiHookActionResolution) error {
	if action.Action == aiHookActionQuiet {
		c.quietClaudeHook(paneID, payload, aiIngestRecordReason(aiHookQuietReason(action)))
		return nil
	}
	body := formatClaudeStopFailureNotifyBody(payload)
	return c.emitClaudeHookStatus(paneID, payload, action, attentionNotifyInput{
		ID:       claudeExtraNotifyID(payload, "stop-failure", payload.ErrorType, payload.ErrorMessage),
		Text:     body.Text,
		Severity: body.Severity,
		Metadata: mergeAINotifyBodyMetadata(metadata, body),
		Force:    true,
	})
}

func (c *aiCommand) ingestClaudeSubagentStop(paneID string, payload claudeHookPayload, metadata map[string]string, action aiHookActionResolution) error {
	if action.Action == aiHookActionNotify {
		body := formatClaudeSubagentStopNotifyBody(payload)
		if err := c.applyAIStatusWithNotify("waiting", paneID, attentionNotifyInput{
			ID:       claudeExtraNotifyID(payload, "subagent-stop", payload.SubagentType, payload.SubagentID),
			Text:     body.Text,
			Severity: body.Severity,
			Metadata: mergeAINotifyBodyMetadata(metadata, body),
			Force:    true,
		}); err != nil {
			c.appendAIIngestLog(claudeHookLogEntry(paneID, payload, "error", aiIngestFailureReason(aiIngestReasonStatusApplyFailed, err)))
			return err
		}
		c.appendAIIngestLog(claudeHookLogEntry(paneID, payload, "notify", ""))
		return nil
	}
	if action.Action == aiHookActionState {
		if err := c.applyAIStatusStateOnly("waiting", paneID, attentionNotifyInput{
			ID:       claudeExtraNotifyID(payload, "subagent-stop", payload.SubagentType, payload.SubagentID),
			Text:     formatClaudeSubagentStopNotifyBody(payload).Text,
			Severity: notify.SeverityInfo,
			Metadata: mergeAINotifyBodyMetadata(metadata, formatClaudeSubagentStopNotifyBody(payload)),
			Force:    true,
		}); err != nil {
			c.appendAIIngestLog(claudeHookLogEntry(paneID, payload, "error", aiIngestFailureReason(aiIngestReasonStatusApplyFailed, err)))
			return err
		}
		c.appendAIIngestLog(claudeHookLogEntry(paneID, payload, "state", aiIngestRecordReason(aiHookStateReason(action))))
		return nil
	}
	c.appendAIIngestLog(claudeHookLogEntry(paneID, payload, "quiet", aiIngestReasonHighVolumeEvent))
	return nil
}

func (c *aiCommand) ingestClaudeTeammateIdle(paneID string, payload claudeHookPayload, metadata map[string]string, action aiHookActionResolution) error {
	if action.Action == aiHookActionQuiet {
		c.quietClaudeHook(paneID, payload, aiIngestRecordReason(aiHookQuietReason(action)))
		return nil
	}
	body := formatClaudeTeammateIdleNotifyBody(payload)
	return c.emitClaudeHookStatus(paneID, payload, action, attentionNotifyInput{
		ID:        claudeExtraNotifyID(payload, "teammate-idle", payload.TeammateName, payload.TeammateID, payload.TeammateContext),
		Text:      body.Text,
		Severity:  body.Severity,
		Metadata:  mergeAINotifyBodyMetadata(metadata, body),
		Force:     true,
		BadgeKind: aiBadgeKindResponseComplete,
	})
}

// emitClaudeHookStatus applies the state-only vs state+notify split shared by
// most claude hook handlers and writes the matching ingest log entry.
func (c *aiCommand) emitClaudeHookStatus(paneID string, payload claudeHookPayload, action aiHookActionResolution, input attentionNotifyInput) error {
	if action.Action == aiHookActionState {
		if err := c.applyAIStatusStateOnly("waiting", paneID, input); err != nil {
			c.appendAIIngestLog(claudeHookLogEntry(paneID, payload, "error", aiIngestFailureReason(aiIngestReasonStatusApplyFailed, err)))
			return err
		}
		c.appendAIIngestLog(claudeHookLogEntry(paneID, payload, "state", aiIngestRecordReason(aiHookStateReason(action))))
		return nil
	}
	if err := c.applyAIStatusWithNotify("waiting", paneID, input); err != nil {
		c.appendAIIngestLog(claudeHookLogEntry(paneID, payload, "error", aiIngestFailureReason(aiIngestReasonStatusApplyFailed, err)))
		return err
	}
	c.appendAIIngestLog(claudeHookLogEntry(paneID, payload, "notify", ""))
	return nil
}

func claudeHookLogEntry(paneID string, payload claudeHookPayload, result string, reason aiIngestReason) aiIngestLogEntry {
	return aiIngestLogEntry{Source: "claude-hook", Event: payload.EventName, Result: result, Reason: reason, Pane: paneID, CWD: payload.CWD, SessionID: payload.SessionID}
}

// claudeRuntimeModelSourceTranscript is the sidecar Source for a model read
// from the transcript tail rather than from a hook payload field.
const claudeRuntimeModelSourceTranscript = "transcript"

// persistClaudeRuntimeModel records the runtime model a Claude hook reported
// into the usage state directory, so the ambient usage HUD can print it after
// the `Claude` label. Best-effort in the sense PR #486 established for the
// Antigravity context sidecar: an empty identifier writes nothing (an earlier
// observation must survive a SessionStart that omitted `model`), and a
// resolution or write failure is swallowed because usage is a side channel of
// hook ingest, never a reason to fail the hook. With several Claude panes the
// newest observation wins regardless of pane; the sidecar keeps one record.
func (c *aiCommand) persistClaudeRuntimeModel(paneID string, payload claudeHookPayload, model, source string) {
	model = strings.TrimSpace(model)
	if model == "" {
		return
	}
	stateDir, err := c.usageStateDir()
	if err != nil {
		return
	}
	_ = runtimemodel.Write(stateDir, runtimemodel.Record{
		Provider:   aiModeClaude,
		Model:      model,
		Source:     source,
		SessionID:  strings.TrimSpace(payload.SessionID),
		PaneID:     paneID,
		ObservedAt: c.now().UTC(),
	})
}

// usageStateDir resolves the directory the usage snapshot cache and the
// runtime model sidecar live in. It mirrors usagecmd.Command.resolveStateDir
// so the ingest writer and the HUD reader agree even when
// PROJMUX_USAGE_STATE_DIR redirects the cache to a synced location.
func (c *aiCommand) usageStateDir() (string, error) {
	if override := strings.TrimSpace(c.env(usagecmd.StateDirEnvVar)); override != "" {
		return override, nil
	}
	homeDir := c.homeDir
	if homeDir == nil {
		homeDir = os.UserHomeDir
	}
	home, err := homeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	paths, err := config.Homes{
		HomeDir:    home,
		ConfigHome: c.env("XDG_CONFIG_HOME"),
		StateHome:  c.env("XDG_STATE_HOME"),
	}.Paths()
	if err != nil {
		return "", err
	}
	return filepath.Join(paths.StateDir, "usage"), nil
}

// quietClaudeHook only records the quiet outcome. Every caller is reached from
// ingestClaudeHook, which already marked the Pane before dispatching.
func (c *aiCommand) quietClaudeHook(paneID string, payload claudeHookPayload, reason aiIngestReason) {
	c.appendAIIngestLog(claudeHookLogEntry(paneID, payload, "quiet", reason))
}

func parseClaudeHookPayload(data []byte) (claudeHookPayload, error) {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return claudeHookPayload{}, fmt.Errorf("parse claude hook payload: %w", err)
	}
	payload := claudeHookPayload{
		EventName:        firstString(raw, "hook_event_name", "event_name"),
		SessionID:        firstString(raw, "session_id", "session-id"),
		CWD:              firstString(raw, "cwd", "workspace", "project_dir"),
		TranscriptPath:   firstString(raw, "transcript_path", "transcriptPath"),
		NotificationType: firstString(raw, "notification_type", "notificationType"),
		Message:          firstString(raw, "message", "text"),
		Prompt:           firstString(raw, "prompt", "user_prompt"),
		ToolName:         firstString(raw, "tool_name", "toolName"),
		ToolUseID:        firstString(raw, "tool_use_id", "toolUseID", "id"),
		ErrorType:        firstString(raw, "error_type", "errorType", "failure_type", "failureType"),
		ErrorMessage:     firstString(raw, "error_message", "errorMessage", "message", "reason"),
		SubagentType:     firstString(raw, "subagent_type", "subagentType", "agent_type", "agentType"),
		SubagentID:       firstString(raw, "subagent_id", "subagentId", "agent_id", "agentId"),
		TeammateName:     firstString(raw, "teammate_name", "teammateName", "teammate"),
		TeammateID:       firstString(raw, "teammate_id", "teammateId"),
		TeammateContext:  firstString(raw, "teammate_context", "teammateContext", "context", "reason", "message"),
		RuntimeModel:     firstString(raw, "model"),
		FromRuntimeModel: firstString(raw, "from_model"),
		ToRuntimeModel:   firstString(raw, "to_model"),
	}
	if payload.CWD == "" {
		payload.CWD = firstNestedString(raw["workspace"], "cwd", "path")
	}
	if payload.Message == "" {
		payload.Message = firstNestedString(raw["notification"], "message", "text")
	}
	if payload.NotificationType == "" {
		payload.NotificationType = firstNestedString(raw["notification"], "notification_type", "type")
	}
	if payload.ToolName == "" {
		payload.ToolName = firstNestedString(raw["tool"], "name", "tool_name")
	}
	if payload.ToolUseID == "" {
		payload.ToolUseID = firstNestedString(raw["tool"], "id", "tool_use_id")
	}
	if payload.ErrorType == "" {
		payload.ErrorType = firstNestedString(raw["error"], "type", "name", "code")
	}
	if payload.ErrorMessage == "" {
		payload.ErrorMessage = firstNestedString(raw["error"], "message", "text", "reason")
	}
	if payload.SubagentType == "" {
		payload.SubagentType = firstNestedString(raw["subagent"], "type", "name", "kind")
	}
	if payload.SubagentID == "" {
		payload.SubagentID = firstNestedString(raw["subagent"], "id", "subagent_id", "agent_id")
	}
	if payload.TeammateName == "" {
		payload.TeammateName = firstNestedString(raw["teammate"], "name", "type", "kind")
	}
	if payload.TeammateID == "" {
		payload.TeammateID = firstNestedString(raw["teammate"], "id", "teammate_id")
	}
	if payload.TeammateContext == "" {
		payload.TeammateContext = firstNestedString(raw["teammate"], "context", "status", "reason", "message")
	}
	payload.ToolInput = mapFromAny(raw["tool_input"])
	if len(payload.ToolInput) == 0 {
		payload.ToolInput = mapFromAny(raw["input"])
	}
	if len(payload.ToolInput) == 0 {
		payload.ToolInput = mapFromAny(raw["tool"])
		delete(payload.ToolInput, "name")
		delete(payload.ToolInput, "tool_name")
		delete(payload.ToolInput, "id")
		delete(payload.ToolInput, "tool_use_id")
	}
	return payload, nil
}

func (p claudeHookPayload) claudeMetadata() map[string]string {
	metadata := map[string]string{
		notify.MetaAgent:    aiModeClaude,
		notify.MetaEvent:    p.EventName,
		"session_id":        p.SessionID,
		"cwd":               p.CWD,
		"transcript_path":   p.TranscriptPath,
		"notification_type": p.NotificationType,
		"prompt":            truncateRunes(p.Prompt, 60),
		"tool_name":         p.ToolName,
		"tool_use_id":       p.ToolUseID,
		"error_type":        p.ErrorType,
		"error_message":     truncateRunes(p.ErrorMessage, 160),
		"subagent_type":     p.SubagentType,
		"subagent_id":       p.SubagentID,
		"teammate_name":     p.TeammateName,
		"teammate_id":       p.TeammateID,
		"teammate_context":  truncateRunes(p.TeammateContext, 160),
		"model":             truncateRunes(p.RuntimeModel, 80),
		"from_model":        truncateRunes(p.FromRuntimeModel, 80),
		"to_model":          truncateRunes(p.ToRuntimeModel, 80),
	}
	for key, value := range p.ToolInput {
		if text := stringFromAny(value); text != "" {
			metadata["tool_input."+key] = truncateRunes(text, 160)
		}
	}
	out := make(map[string]string, len(metadata))
	for k, v := range metadata {
		if value := strings.TrimSpace(v); value != "" {
			out[k] = value
		}
	}
	return out
}

func claudeNotifyID(p claudeHookPayload) string {
	parts := []string{"ai", "claude", "notification"}
	if value := strings.TrimSpace(p.SessionID); value != "" {
		parts = append(parts, value)
	}
	if value := strings.TrimSpace(p.NotificationType); value != "" {
		parts = append(parts, value)
	}
	if value := strings.TrimSpace(p.Message); value != "" {
		parts = append(parts, truncateRunes(value, 40))
	}
	return strings.Join(parts, ":")
}

func claudePermissionNotifyID(p claudeHookPayload) string {
	sessionID := strings.TrimSpace(p.SessionID)
	toolUseID := strings.TrimSpace(p.ToolUseID)
	switch {
	case sessionID != "" && toolUseID != "":
		return "ai:claude:permission:" + sessionID + ":" + toolUseID
	case toolUseID != "":
		return "ai:claude:permission:" + toolUseID
	default:
		return ""
	}
}

func claudeStopNotifyID(p claudeHookPayload) string {
	if sessionID := strings.TrimSpace(p.SessionID); sessionID != "" {
		return "ai:claude:stop:" + sessionID
	}
	return ""
}

func claudeExtraNotifyID(p claudeHookPayload, kind string, values ...string) string {
	parts := []string{"ai", "claude", kind}
	if value := strings.TrimSpace(p.SessionID); value != "" {
		parts = append(parts, value)
	}
	for _, value := range values {
		if trimmed := truncateRunes(value, 40); trimmed != "" {
			parts = append(parts, trimmed)
		}
	}
	return strings.Join(parts, ":")
}

// readClaudeTranscriptLastAssistant reads the transcript tail (at most
// claudeTranscriptTailLimit bytes) and returns the newest assistant text and
// the newest assistant `message.model` identifier. The two are searched
// independently: a trailing assistant line with a model but no text (a tool
// call) still yields the model, and vice versa. Both are "" when the
// transcript is unreadable.
func readClaudeTranscriptLastAssistant(path string) (text, model string) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", ""
	}
	file, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return "", ""
	}
	size := info.Size()
	if size <= 0 {
		return "", ""
	}
	start := int64(0)
	if size > claudeTranscriptTailLimit {
		start = size - claudeTranscriptTailLimit
	}
	buf := make([]byte, size-start)
	if _, err := file.ReadAt(buf, start); err != nil && !errors.Is(err, io.EOF) {
		return "", ""
	}
	lines := strings.Split(strings.TrimSpace(string(buf)), "\n")
	for i := len(lines) - 1; i >= 0 && (text == "" || model == ""); i-- {
		lineText, lineModel := claudeAssistantFromJSONLine(lines[i])
		if text == "" {
			text = lineText
		}
		if model == "" {
			model = lineModel
		}
	}
	return text, model
}

// claudeAssistantFromJSONLine returns the assistant text and the
// `message.model` identifier of one transcript line, or "" for each when the
// line is not an assistant entry. Only an assistant entry's model counts: user
// and system lines never name the model that will answer them.
func claudeAssistantFromJSONLine(line string) (text, model string) {
	var raw map[string]any
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return "", ""
	}
	if strings.EqualFold(stringFromAny(raw["role"]), "assistant") {
		return claudeContentText(raw["content"]), strings.TrimSpace(stringFromAny(raw["model"]))
	}
	message := mapFromAny(raw["message"])
	if strings.EqualFold(stringFromAny(message["role"]), "assistant") {
		return claudeContentText(message["content"]), strings.TrimSpace(stringFromAny(message["model"]))
	}
	return "", ""
}

func claudeContentText(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case []any:
		for _, item := range v {
			if text := firstNestedString(item, "text"); text != "" {
				return text
			}
			if text := stringFromAny(item); text != "" {
				return text
			}
		}
	case map[string]any:
		return firstString(v, "text", "content")
	}
	return ""
}
