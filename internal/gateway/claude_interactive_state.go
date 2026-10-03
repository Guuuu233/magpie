package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/yetone/magpie/internal/appdir"
)

const interactiveSessionStateVersion = 1

type interactiveRestoreStatus string

const (
	interactiveRestoreExact              interactiveRestoreStatus = "exact"
	interactiveRestoreReplyAnchor        interactiveRestoreStatus = "reply_anchor"
	interactiveRestoreStateMissing       interactiveRestoreStatus = "state_missing"
	interactiveRestoreDirty              interactiveRestoreStatus = "dirty"
	interactiveRestoreContextMismatch    interactiveRestoreStatus = "context_mismatch"
	interactiveRestoreCheckpointNotFound interactiveRestoreStatus = "checkpoint_not_found"
	interactiveRestoreSuffixRejected     interactiveRestoreStatus = "suffix_rejected"
)

type conversationSuffixStatus uint8

const (
	conversationSuffixOK conversationSuffixStatus = iota
	conversationSuffixCheckpointNotFound
	conversationSuffixRejected
)

type interactiveSessionEntry struct {
	SessionID  string    `json:"session_id"`
	ConvKey    string    `json:"conv_key"`
	ReplyKey   string    `json:"reply_key,omitempty"`
	ContextKey string    `json:"context_key"`
	Dirty      bool      `json:"dirty,omitempty"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type interactiveSessionState struct {
	Version  int                                `json:"version"`
	Sessions map[string]interactiveSessionEntry `json:"sessions"`
}

func interactiveSessionStatePath() string {
	return filepath.Join(appdir.Config(), "claude-interactive-sessions.json")
}

func interactiveSessionStateKey(owner, outerSession string) string {
	sum := sha256.Sum256([]byte(owner + "\x00" + outerSession))
	return hex.EncodeToString(sum[:])
}

func interactiveSessionWorkDir(owner, outerSession string) (string, error) {
	// Keep the cwd stable for this outer conversation, but outside the home:
	// Claude Code includes git status for a repository containing its cwd in
	// the system prompt, so a cache directory under a dotfiles-managed home can
	// both leak unrelated paths and invalidate the prompt-cache prefix. This is
	// the same concern addressed by #626 for headless subscription runs; the
	// interactive backend needs a per-conversation directory because --resume
	// resolves sessions by project/cwd.
	dir := filepath.Join(os.TempDir(), "magpie-claude-interactive-"+interactiveSessionStateKey(owner, outerSession))
	if err := os.Mkdir(dir, 0o700); err != nil && !os.IsExist(err) {
		return "", err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("%s is not a folder", dir)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return "", fmt.Errorf("%s can be written by others (%v)", dir, fi.Mode().Perm())
	}
	// On a shared /tmp, a different user may have pre-created a 0700 path.
	// Creating a file inside proves this process can actually own/use the cwd
	// without platform-specific uid code; remove the probe immediately.
	probe, err := os.CreateTemp(dir, ".magpie-owner-check-*")
	if err != nil {
		return "", fmt.Errorf("%s is not writable by this user: %w", dir, err)
	}
	probeName := probe.Name()
	if err := probe.Close(); err != nil {
		_ = os.Remove(probeName)
		return "", err
	}
	_ = os.Remove(probeName)
	return dir, nil
}

func readInteractiveSessionState() interactiveSessionState {
	out := interactiveSessionState{Version: interactiveSessionStateVersion, Sessions: map[string]interactiveSessionEntry{}}
	b, err := os.ReadFile(interactiveSessionStatePath())
	if err != nil {
		return out
	}
	if json.Unmarshal(b, &out) != nil || out.Version != interactiveSessionStateVersion || out.Sessions == nil {
		return interactiveSessionState{Version: interactiveSessionStateVersion, Sessions: map[string]interactiveSessionEntry{}}
	}
	return out
}

func writeInteractiveSessionState(state interactiveSessionState) error {
	dir := filepath.Dir(interactiveSessionStatePath())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// A bounded state file is enough for continuity while avoiding an
	// unbounded collection of conversations that can no longer share a live
	// Anthropic prompt cache anyway.
	cutoff := time.Now().Add(-30 * 24 * time.Hour)
	for k, v := range state.Sessions {
		if !v.UpdatedAt.IsZero() && v.UpdatedAt.Before(cutoff) {
			delete(state.Sessions, k)
		}
	}
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".claude-interactive-sessions-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, interactiveSessionStatePath()); err != nil {
		// Windows cannot replace an existing file with Rename. The experimental
		// backend is primarily Unix/PTY based, but keep the state helper portable.
		_ = os.Remove(interactiveSessionStatePath())
		if err2 := os.Rename(name, interactiveSessionStatePath()); err2 != nil {
			return err
		}
	}
	return nil
}

func (b *subscriptionBridge) interactiveSession(owner, outerSession string) (interactiveSessionEntry, bool) {
	if outerSession == "" {
		return interactiveSessionEntry{}, false
	}
	b.stateMu.Lock()
	defer b.stateMu.Unlock()
	state := readInteractiveSessionState()
	e, ok := state.Sessions[interactiveSessionStateKey(owner, outerSession)]
	return e, ok
}

func (b *subscriptionBridge) saveInteractiveSession(owner, outerSession string, e interactiveSessionEntry) {
	if outerSession == "" || e.SessionID == "" || e.ConvKey == "" || e.ContextKey == "" {
		return
	}
	b.stateMu.Lock()
	defer b.stateMu.Unlock()
	state := readInteractiveSessionState()
	e.UpdatedAt = time.Now().UTC()
	state.Sessions[interactiveSessionStateKey(owner, outerSession)] = e
	if err := writeInteractiveSessionState(state); err != nil {
		log.Printf("Claude interactive session state: %v", err)
	}
}

func (b *subscriptionBridge) markInteractiveSessionDirty(owner, outerSession string) {
	if outerSession == "" {
		return
	}
	b.stateMu.Lock()
	defer b.stateMu.Unlock()
	state := readInteractiveSessionState()
	key := interactiveSessionStateKey(owner, outerSession)
	e, ok := state.Sessions[key]
	if !ok || e.Dirty {
		return
	}
	e.Dirty = true
	e.UpdatedAt = time.Now().UTC()
	state.Sessions[key] = e
	if err := writeInteractiveSessionState(state); err != nil {
		log.Printf("Claude interactive session state: %v", err)
	}
}

// restoreInteractiveSession decides whether a persisted inner Claude session
// can safely accept only the caller's new suffix. A persisted entry is never
// permission to replay the whole outer history: if continuity cannot be
// proven, the caller must fail closed rather than duplicate old tool output
// and messages into a fresh inner session.
func (b *subscriptionBridge) restoreInteractiveSession(owner, outerSession string, req *Request) (interactiveSessionEntry, []Message, interactiveRestoreStatus) {
	saved, ok := b.interactiveSession(owner, outerSession)
	if !ok {
		return interactiveSessionEntry{}, nil, interactiveRestoreStateMissing
	}
	if saved.Dirty {
		return saved, nil, interactiveRestoreDirty
	}
	if saved.ContextKey != turnKey(owner, req, nil) {
		return saved, nil, interactiveRestoreContextMismatch
	}
	since, status := conversationSuffixAfterDetailed(owner, req.Messages, saved.ConvKey)
	switch status {
	case conversationSuffixOK:
		return saved, since, interactiveRestoreExact
	case conversationSuffixRejected:
		return saved, nil, interactiveRestoreSuffixRejected
	}
	if saved.ReplyKey == "" {
		return saved, nil, interactiveRestoreCheckpointNotFound
	}
	since, status = conversationSuffixAfterReplyDetailed(req.Messages, saved.ReplyKey)
	switch status {
	case conversationSuffixOK:
		return saved, since, interactiveRestoreReplyAnchor
	case conversationSuffixRejected:
		return saved, nil, interactiveRestoreSuffixRejected
	default:
		return saved, nil, interactiveRestoreCheckpointNotFound
	}
}

// conversationSuffixAfter returns only what the caller said after a completed
// assistant checkpoint already present in a persisted inner Claude session.
// It deliberately refuses suffixes containing assistant/tool-result messages:
// those mean another path has advanced or rewritten the outer conversation,
// and replaying them as one interactive prompt would duplicate history.
func conversationSuffixAfter(owner string, msgs []Message, convKey string) ([]Message, bool) {
	since, status := conversationSuffixAfterDetailed(owner, msgs, convKey)
	return since, status == conversationSuffixOK
}

func conversationSuffixAfterDetailed(owner string, msgs []Message, convKey string) ([]Message, conversationSuffixStatus) {
	if convKey == "" {
		return nil, conversationSuffixCheckpointNotFound
	}
	h := sha256.New()
	_, _ = h.Write([]byte(owner + "\x00"))
	match := -1
	hashMessages(h, msgs, func(i int) {
		if msgs[i].Role == "assistant" && hex.EncodeToString(h.Sum(nil)) == convKey {
			match = i
		}
	})
	return safeConversationSuffixAfterDetailed(msgs, match)
}

// assistantReplyKey is a history-independent anchor for the last completed
// reply saved in the inner Claude session. Unlike ConvKey it survives an
// outer client compacting or rewriting messages that came before that reply.
func assistantReplyKey(msg Message) string {
	if msg.Role != "assistant" {
		return ""
	}
	h := sha256.New()
	hashMessages(h, []Message{msg}, nil)
	return hex.EncodeToString(h.Sum(nil))
}

// conversationSuffixAfterReply finds a persisted completed reply after an
// outer client rewrote earlier history. The reply must occur exactly once:
// resuming from an ambiguous repeated answer could attach the new user turn
// to the wrong point in the inner conversation.
func conversationSuffixAfterReply(msgs []Message, replyKey string) ([]Message, bool) {
	since, status := conversationSuffixAfterReplyDetailed(msgs, replyKey)
	return since, status == conversationSuffixOK
}

func conversationSuffixAfterReplyDetailed(msgs []Message, replyKey string) ([]Message, conversationSuffixStatus) {
	if replyKey == "" {
		return nil, conversationSuffixCheckpointNotFound
	}
	match := -1
	for i, m := range msgs {
		if m.Role != "assistant" || assistantReplyKey(m) != replyKey {
			continue
		}
		if match >= 0 {
			return nil, conversationSuffixRejected
		}
		match = i
	}
	return safeConversationSuffixAfterDetailed(msgs, match)
}

func safeConversationSuffixAfter(msgs []Message, match int) ([]Message, bool) {
	since, status := safeConversationSuffixAfterDetailed(msgs, match)
	return since, status == conversationSuffixOK
}

func safeConversationSuffixAfterDetailed(msgs []Message, match int) ([]Message, conversationSuffixStatus) {
	if match < 0 || match >= len(msgs)-1 {
		if match < 0 {
			return nil, conversationSuffixCheckpointNotFound
		}
		return nil, conversationSuffixRejected
	}
	since := msgs[match+1:]
	for _, m := range since {
		if m.Role == "assistant" {
			return nil, conversationSuffixRejected
		}
		for _, p := range m.Parts {
			if p.Kind == ToolResult {
				return nil, conversationSuffixRejected
			}
		}
	}
	return since, conversationSuffixOK
}
