package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/yetone/magpie/internal/appdir"
)

const interactiveSessionStateVersion = 1

type interactiveSessionEntry struct {
	SessionID  string    `json:"session_id"`
	ConvKey    string    `json:"conv_key"`
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
	dir := filepath.Join(appdir.Cache(), "claude-interactive", interactiveSessionStateKey(owner, outerSession))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
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

// conversationSuffixAfter returns only what the caller said after a completed
// assistant checkpoint already present in a persisted inner Claude session.
// It deliberately refuses suffixes containing assistant/tool-result messages:
// those mean another path has advanced or rewritten the outer conversation,
// and replaying them as one interactive prompt would duplicate history.
func conversationSuffixAfter(owner string, msgs []Message, convKey string) ([]Message, bool) {
	if convKey == "" {
		return nil, false
	}
	h := sha256.New()
	_, _ = h.Write([]byte(owner + "\x00"))
	match := -1
	hashMessages(h, msgs, func(i int) {
		if msgs[i].Role == "assistant" && hex.EncodeToString(h.Sum(nil)) == convKey {
			match = i
		}
	})
	if match < 0 || match >= len(msgs)-1 {
		return nil, false
	}
	since := msgs[match+1:]
	for _, m := range since {
		if m.Role == "assistant" {
			return nil, false
		}
		for _, p := range m.Parts {
			if p.Kind == ToolResult {
				return nil, false
			}
		}
	}
	return since, true
}
