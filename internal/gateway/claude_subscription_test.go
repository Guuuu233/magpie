package gateway

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
)

func TestClaudeSubscriptionPromptKeepsForeignHarnessOutOfSystem(t *testing.T) {
	r := &Request{
		System:   "You are an expert coding assistant operating inside pi\nsee docs/custom-provider.md and docs/packages.md",
		Messages: []Message{{Role: "user", Parts: []Part{{Kind: Text, Text: "hello"}}}},
	}
	blocks, err := renderClaudePrompt(r)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(blocks)
	s := string(b)
	if !strings.Contains(s, "external_system_instructions") || !strings.Contains(s, "operating inside pi") || !strings.Contains(s, "Human: hello") {
		t.Fatalf("prompt lost content: %s", b)
	}
	// renderClaudePrompt is the user content passed to the genuine CLI. The
	// foreign harness is never supplied through --system-prompt, where
	// Anthropic's subscription classifier rejects it.
	args := strings.Join(claudeCLIArgs("claude-sonnet-5", `{}`, "medium", false), " ")
	if strings.Contains(args, "system-prompt") {
		t.Fatal("Claude bridge must retain the genuine Claude Code preset")
	}
	for _, want := range []string{"--input-format stream-json", "--include-partial-messages", "--strict-mcp-config", "--effort medium"} {
		if !strings.Contains(args, want) {
			t.Fatalf("missing CLI contract %q in %q", want, args)
		}
	}
}

// TestClaudeCLIEffortXHigh: xhigh is a level of Claude Code's own, sent as
// output_config.effort "xhigh"; turned into max it used more of the plan (#385).
func TestClaudeCLIEffortXHigh(t *testing.T) {
	for _, e := range []string{"low", "medium", "high", "xhigh", "max"} {
		args := strings.Join(claudeCLIArgs("claude-opus-5-5", `{}`, e, false), " ")
		if !strings.Contains(args, "--effort "+e+" ") {
			t.Fatalf("effort %s: %q", e, args)
		}
	}
}

func TestClaudeInteractiveBridgeArgsStayOutOfRealClaudeHeadlessMode(t *testing.T) {
	args := claudeInteractiveCLIArgs("claude-opus-5-5", `{}`, "high", false, "session-123")
	joined := strings.Join(args, "\x00")
	for _, forbidden := range []string{"--input-format\x00stream-json", "--include-partial-messages", "--no-session-persistence", "--thinking-display", "--setting-sources"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("interactive bridge args contain headless-only option %q: %q", forbidden, args)
		}
	}
	for _, want := range []string{"-p", "--output-format", "stream-json", "--model", "claude-opus-5-5", "--effort", "high", "--resume", "session-123", "--strict-mcp-config", "--mcp-config"} {
		if !slices.Contains(args, want) {
			t.Fatalf("interactive bridge args missing %q: %q", want, args)
		}
	}
}

func TestClaudeSubscriptionBinaryUsesConfiguredInteractiveBridge(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executable test uses unix file mode")
	}
	dir := t.TempDir()
	bridge := filepath.Join(dir, "claude-bridge")
	if err := os.WriteFile(bridge, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAGPIE_CLAUDE_INTERACTIVE_BRIDGE", bridge)
	got, interactive, err := claudeSubscriptionBinary()
	if err != nil {
		t.Fatal(err)
	}
	if got != bridge || !interactive {
		t.Fatalf("binary = %q, interactive=%v; want %q, true", got, interactive, bridge)
	}
}

func TestClaudeInteractiveBridgeReadsWholeAssistantMessage(t *testing.T) {
	run := &subscriptionRun{interactive: true}
	seg := run.attach()
	lines := []string{
		`{"type":"assistant","session_id":"sess-1","request_id":"req-1","message":{"id":"m1","model":"claude-opus-5-5","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":3}}}`,
		`{"type":"result","subtype":"success","is_error":false,"session_id":"sess-1","result":"hello"}`,
	}
	go run.readOutput(strings.NewReader(strings.Join(lines, "\n") + "\n"))
	var got []string
	var usage Usage
	for ev := range seg {
		switch ev.Kind {
		case KStart:
			got = append(got, "start:"+ev.MsgID+":"+ev.Model)
			usage.add(ev.Usage)
		case KText:
			got = append(got, "text:"+ev.Text)
		case KUsage:
			usage.add(ev.Usage)
		case KStop:
			got = append(got, "stop:"+ev.Stop)
		}
	}
	if s := strings.Join(got, "|"); s != "start:m1:claude-opus-5-5|text:hello|stop:stop" {
		t.Fatalf("events: %s", s)
	}
	if usage.Input != 2 || usage.Output != 3 {
		t.Fatalf("usage: %+v", usage)
	}
	if run.sessionID != "sess-1" {
		t.Fatalf("session id = %q", run.sessionID)
	}
}

// Interactive Claude writes one transcript row per content block. Thinking
// and visible text can therefore share the same Anthropic message id while
// carrying different transcript UUIDs. The bridge must keep both blocks but
// count the message usage only once.
func TestClaudeInteractiveBridgeKeepsMultipleRowsOfOneAssistantMessage(t *testing.T) {
	run := &subscriptionRun{interactive: true}
	seg := run.attach()
	lines := []string{
		`{"type":"assistant","uuid":"row-think","session_id":"sess-1","request_id":"req-1","message":{"id":"m1","model":"claude-opus-5-5","content":[{"type":"thinking","thinking":"reason"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":5,"output_tokens_details":{"thinking_tokens":3}}}}`,
		`{"type":"assistant","uuid":"row-text","session_id":"sess-1","request_id":"req-1","message":{"id":"m1","model":"claude-opus-5-5","content":[{"type":"text","text":"answer"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":5,"output_tokens_details":{"thinking_tokens":3}}}}`,
		`{"type":"result","subtype":"success","is_error":false,"session_id":"sess-1","stop_reason":"end_turn","result":"answer"}`,
	}
	go run.readOutput(strings.NewReader(strings.Join(lines, "\n") + "\n"))
	var got []string
	var usage Usage
	for ev := range seg {
		switch ev.Kind {
		case KStart:
			got = append(got, "start:"+ev.MsgID)
			usage.add(ev.Usage)
		case KThink:
			got = append(got, "think:"+ev.Text)
		case KText:
			got = append(got, "text:"+ev.Text)
		case KStop:
			got = append(got, "stop:"+ev.Stop)
		}
	}
	if s := strings.Join(got, "|"); s != "start:m1|think:reason|text:answer|stop:stop" {
		t.Fatalf("events: %s", s)
	}
	if usage.Input != 2 || usage.Output != 5 || usage.Reasoning != 3 {
		t.Fatalf("usage counted more than once: %+v", usage)
	}
}

func TestClaudeInteractiveBridgeReturnsExternalToolCallBeforeTurnEnds(t *testing.T) {
	run := &subscriptionRun{interactive: true}
	seg := run.attach()
	line := `{"type":"assistant","session_id":"sess-tool","message":{"id":"m2","model":"claude-opus-5-5","content":[{"type":"tool_use","id":"toolu_1","name":"mcp__magpie__read","input":{"path":"a.txt"}}],"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":7}}}`
	done := make(chan struct{})
	go func() {
		run.readOutput(strings.NewReader(line + "\n"))
		close(done)
	}()
	var got []string
	for ev := range seg {
		switch ev.Kind {
		case KToolStart:
			got = append(got, "tool:"+ev.ID+":"+ev.Name)
		case KToolArgs:
			got = append(got, "args:"+ev.Text)
		case KStop:
			got = append(got, "stop:"+ev.Stop)
		}
	}
	<-done
	if s := strings.Join(got, "|"); s != `tool:toolu_1:read|args:{"path":"a.txt"}|stop:tool` {
		t.Fatalf("events: %s", s)
	}
	if len(run.asked) != 1 || run.asked[0] != "read" {
		t.Fatalf("asked: %v", run.asked)
	}
}

func TestClaudeInteractiveBridgeErrorResultUsesBridgeErrorMessage(t *testing.T) {
	run := &subscriptionRun{interactive: true}
	seg := run.attach()
	line := `{"type":"result","subtype":"error_during_execution","is_error":true,"api_error_status":429,"error":"interactive quota error","result":null}`
	go run.readOutput(strings.NewReader(line + "\n"))
	var got Event
	for ev := range seg {
		if ev.Kind == KError {
			got = ev
		}
	}
	if got.Text != "interactive quota error" || got.Status != 429 {
		t.Fatalf("error event: %+v", got)
	}
}

func TestClaudeSubscriptionInteractiveBridgeHandlesFirstTurn(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell scripts stand in for Claude binaries")
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "bridge.log")
	bridge := filepath.Join(dir, "claude-bridge")
	bridgeScript := `#!/bin/sh
printf 'args:%s\n' "$*" >> "$FAKE_BRIDGE_LOG"
printf 'stdin:' >> "$FAKE_BRIDGE_LOG"
cat | tee -a "$FAKE_BRIDGE_LOG" >/dev/null
printf '\n' >> "$FAKE_BRIDGE_LOG"
echo '{"type":"assistant","session_id":"sess-1","message":{"id":"m1","model":"claude-opus-5-5","content":[{"type":"text","text":"interactive-ok"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":3}}}'
echo '{"type":"result","subtype":"success","is_error":false,"session_id":"sess-1","result":"interactive-ok"}'
`
	if err := os.WriteFile(bridge, []byte(bridgeScript), 0o755); err != nil {
		t.Fatal(err)
	}
	// If the old headless path is used, fail the request immediately instead
	// of ever reaching a real Claude installation on the developer machine.
	headless := filepath.Join(dir, "claude")
	if err := os.WriteFile(headless, []byte("#!/bin/sh\necho '{\"type\":\"result\",\"is_error\":true,\"result\":\"headless-called\"}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("MAGPIE_CLAUDE_INTERACTIVE_BRIDGE", bridge)
	t.Setenv("FAKE_BRIDGE_LOG", logPath)

	s := New()
	t.Cleanup(s.subscription.abortAll)
	p := provider.Provider{ID: "claude", Account: &provider.Account{Agent: "claude", User: "u"}}
	body := `{"model":"claude-opus-5-5","max_tokens":100,"system":"be brief","tools":[{"name":"read","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hello"}]}`
	rec := httptest.NewRecorder()
	var u Usage
	code, msg := s.serveClaudeSubscription(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)), provider.Anthropic, p, "claude-opus-5-5", []byte(body), &u)
	if code != 200 {
		t.Fatalf("%d %s: %s", code, msg, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "interactive-ok") {
		t.Fatalf("answer: %s", rec.Body)
	}
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log := string(b)
	if !strings.Contains(log, "args:-p --output-format stream-json") || !strings.Contains(log, "--model claude-opus-5-5") {
		t.Fatalf("bridge args not used:\n%s", log)
	}
	if !strings.Contains(log, "stdin:<external_system_instructions>") || !strings.Contains(log, "Human: hello") {
		t.Fatalf("rendered prompt not piped to bridge:\n%s", log)
	}
}

func TestInteractiveRunEmitsAliveWhileBridgeIsQuiet(t *testing.T) {
	old := interactiveAliveEvery
	interactiveAliveEvery = 10 * time.Millisecond
	t.Cleanup(func() { interactiveAliveEvery = old })
	r := &subscriptionRun{interactive: true}
	ch := r.attach()
	done := make(chan struct{})
	go r.keepInteractiveAlive(done)
	defer close(done)
	select {
	case ev := <-ch:
		if ev.Kind != KAlive {
			t.Fatalf("got event kind %v, want KAlive", ev.Kind)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("interactive run emitted no liveness event")
	}
}

func TestClaudeInteractiveQuietTurnStreamsKeepaliveBeforeAnswer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script stands in for interactive bridge")
	}
	old := interactiveAliveEvery
	interactiveAliveEvery = 10 * time.Millisecond
	t.Cleanup(func() { interactiveAliveEvery = old })
	dir := t.TempDir()
	bridge := filepath.Join(dir, "claude-bridge")
	script := `#!/bin/sh
cat >/dev/null
sleep 0.08
echo '{"type":"assistant","session_id":"sess-quiet","uuid":"row-quiet","message":{"id":"m-quiet","model":"claude-opus-5-5","content":[{"type":"text","text":"QUIET_OK"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}}'
echo '{"type":"result","subtype":"success","is_error":false,"session_id":"sess-quiet","stop_reason":"end_turn","result":"QUIET_OK"}'
`
	if err := os.WriteFile(bridge, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAGPIE_CLAUDE_INTERACTIVE_BRIDGE", bridge)
	s := New()
	t.Cleanup(s.subscription.abortAll)
	p := provider.Provider{ID: "claude", Account: &provider.Account{Agent: "claude", User: "u"}}
	body := `{"model":"claude-opus-5-5","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"quiet"}]}`
	rec := httptest.NewRecorder()
	var u Usage
	if code, msg := s.serveClaudeSubscription(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)), provider.Anthropic, p, "claude-opus-5-5", []byte(body), &u); code != 200 {
		t.Fatalf("%d %s: %s", code, msg, rec.Body.String())
	}
	out := rec.Body.String()
	if !strings.Contains(out, `"type":"ping"`) || !strings.Contains(out, "QUIET_OK") {
		t.Fatalf("quiet interactive turn was not kept alive before its answer:\n%s", out)
	}
}

func TestClaudeInteractiveOneOffLetsBridgeCleanDetachedChild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script and setsid stand in for an interactive PTY bridge")
	}
	dir := t.TempDir()
	bridge := filepath.Join(dir, "claude-bridge")
	childPID := filepath.Join(dir, "child.pid")
	script := `#!/bin/sh
cleanup() {
  if [ -f "$FAKE_CHILD_PID" ]; then kill "$(cat "$FAKE_CHILD_PID")" 2>/dev/null || true; fi
  exit 0
}
trap cleanup INT TERM
python3 -c 'import os,sys,time; os.setsid(); open(sys.argv[1],"w").write(str(os.getpid())); time.sleep(60)' "$FAKE_CHILD_PID" &
for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
  [ -s "$FAKE_CHILD_PID" ] && break
  sleep 0.01
done
cat >/dev/null
echo '{"type":"assistant","session_id":"sess-oneoff","uuid":"row-oneoff","message":{"id":"m-oneoff","model":"claude-opus-5-5","content":[{"type":"text","text":"ONEOFF_OK"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}}'
echo '{"type":"result","subtype":"success","is_error":false,"session_id":"sess-oneoff","stop_reason":"end_turn","result":"ONEOFF_OK"}'
while :; do sleep 0.05; done
`
	if err := os.WriteFile(bridge, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAGPIE_CLAUDE_INTERACTIVE_BRIDGE", bridge)
	t.Setenv("FAKE_CHILD_PID", childPID)

	s := New()
	t.Cleanup(s.subscription.abortAll)
	p := provider.Provider{ID: "claude", Account: &provider.Account{Agent: "claude", User: "u"}}
	body := `{"model":"claude-opus-5-5","max_tokens":16,"messages":[{"role":"user","content":"one off"}]}`
	rec := httptest.NewRecorder()
	var u Usage
	if code, msg := s.serveClaudeSubscription(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)), provider.Anthropic, p, "claude-opus-5-5", []byte(body), &u); code != 200 {
		t.Fatalf("%d %s: %s", code, msg, rec.Body.String())
	}
	var pid int
	for deadline := time.Now().Add(2 * time.Second); pid == 0 && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		b, _ := os.ReadFile(childPID)
		pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}
	if pid == 0 {
		t.Fatal("fake bridge did not start detached child")
	}
	t.Cleanup(func() {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Kill()
		}
	})
	if n := leftover([]int{pid}, 2*time.Second); n != 0 {
		t.Fatalf("interactive bridge detached child survived one-off cleanup (pid %d)", pid)
	}
}

func TestClaudeSubscriptionInteractiveBridgeResumesNextTurn(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script stands in for interactive bridge")
	}
	dir := t.TempDir()
	bridge := filepath.Join(dir, "claude-bridge")
	logPath := filepath.Join(dir, "bridge.log")
	countPath := filepath.Join(dir, "count")
	script := `#!/bin/sh
n=0
if [ -f "$FAKE_BRIDGE_COUNT" ]; then n=$(cat "$FAKE_BRIDGE_COUNT"); fi
n=$((n+1))
printf '%s' "$n" > "$FAKE_BRIDGE_COUNT"
prompt=$(cat)
printf 'call:%s args:%s stdin:%s\n' "$n" "$*" "$prompt" >> "$FAKE_BRIDGE_LOG"
if [ "$n" -eq 1 ]; then text=turn1; else text=turn2; fi
if [ "$n" -gt 1 ]; then
  printf '{"type":"assistant","session_id":"sess-1","message":{"id":"m1","model":"claude-opus-5-5","content":[{"type":"text","text":"turn1"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":3}}}\n'
fi
printf '{"type":"assistant","session_id":"sess-1","message":{"id":"m%s","model":"claude-opus-5-5","content":[{"type":"text","text":"%s"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":3}}}\n' "$n" "$text"
printf '{"type":"result","subtype":"success","is_error":false,"session_id":"sess-1","result":"%s"}\n' "$text"
`
	if err := os.WriteFile(bridge, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAGPIE_CLAUDE_INTERACTIVE_BRIDGE", bridge)
	t.Setenv("FAKE_BRIDGE_LOG", logPath)
	t.Setenv("FAKE_BRIDGE_COUNT", countPath)

	s := New()
	t.Cleanup(s.subscription.abortAll)
	p := provider.Provider{ID: "claude", Account: &provider.Account{Agent: "claude", User: "u"}}
	ask := func(msgs string) string {
		t.Helper()
		body := `{"model":"claude-opus-5-5","max_tokens":100,"tools":[{"name":"read","input_schema":{"type":"object"}}],"messages":` + msgs + `}`
		rec := httptest.NewRecorder()
		var u Usage
		if code, msg := s.serveClaudeSubscription(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)), provider.Anthropic, p, "claude-opus-5-5", []byte(body), &u); code != 200 {
			t.Fatalf("%d %s: %s", code, msg, rec.Body.String())
		}
		var res struct {
			Content []struct{ Text string } `json:"content"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || len(res.Content) == 0 {
			t.Fatalf("answer: %s (%v)", rec.Body, err)
		}
		return res.Content[0].Text
	}
	msg := func(role, text string) string { return `{"role":"` + role + `","content":` + strconv.Quote(text) + `}` }

	first := ask(`[` + msg("user", "hello") + `]`)
	if first != "turn1" {
		t.Fatalf("first = %q", first)
	}
	second := ask(`[` + msg("user", "hello") + `,` + msg("assistant", first) + `,` + msg("user", "and?") + `]`)
	if second != "turn2" {
		t.Fatalf("second = %q", second)
	}
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		t.Fatalf("bridge calls:\n%s", b)
	}
	if !strings.Contains(lines[1], "--resume sess-1") {
		t.Fatalf("second turn did not resume the interactive Claude session:\n%s", b)
	}
	if !strings.Contains(lines[1], "stdin:and?") || strings.Contains(lines[1], "turn1") || strings.Contains(lines[1], "Human: hello") {
		t.Fatalf("second turn should send only the new user text:\n%s", b)
	}
}

func TestClaudeInteractiveSessionRestoresAcrossGatewayRestart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script stands in for interactive bridge")
	}
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	dir := t.TempDir()
	bridge := filepath.Join(dir, "claude-bridge")
	logPath := filepath.Join(dir, "bridge.log")
	script := `#!/bin/sh
prompt=$(cat)
printf 'args:%s\nstdin:%s\n' "$*" "$prompt" >> "$FAKE_BRIDGE_LOG"
case " $* " in
  *" --resume sess-persist "*) text=turn2 ;;
  *) text=turn1 ;;
esac
printf '{"type":"assistant","session_id":"sess-persist","uuid":"row-%s","message":{"id":"m-%s","model":"claude-opus-5-5","content":[{"type":"text","text":"%s"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":3}}}\n' "$text" "$text" "$text"
printf '{"type":"result","subtype":"success","is_error":false,"session_id":"sess-persist","stop_reason":"end_turn","result":"%s"}\n' "$text"
`
	if err := os.WriteFile(bridge, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAGPIE_CLAUDE_INTERACTIVE_BRIDGE", bridge)
	t.Setenv("FAKE_BRIDGE_LOG", logPath)
	p := provider.Provider{ID: "claude", Account: &provider.Account{Agent: "claude", User: "u"}}
	msg := func(role, text string) string { return `{"role":"` + role + `","content":` + strconv.Quote(text) + `}` }
	ask := func(s *Server, msgs string) string {
		t.Helper()
		body := `{"model":"claude-opus-5-5","max_tokens":100,"tools":[{"name":"read","input_schema":{"type":"object"}}],"messages":` + msgs + `}`
		req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
		req.Header.Set(SessionHeader, "desktop-session-1")
		rec := httptest.NewRecorder()
		var u Usage
		if code, why := s.serveClaudeSubscription(rec, req, provider.Anthropic, p, "claude-opus-5-5", []byte(body), &u); code != 200 {
			t.Fatalf("%d %s: %s", code, why, rec.Body.String())
		}
		var res struct {
			Content []struct{ Text string } `json:"content"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || len(res.Content) == 0 {
			t.Fatalf("answer: %s (%v)", rec.Body, err)
		}
		return res.Content[0].Text
	}

	s1 := New()
	first := ask(s1, `[`+msg("user", "hello")+`]`)
	if first != "turn1" {
		t.Fatalf("first = %q", first)
	}
	// Simulate a clean gateway restart after the completed turn was parked.
	s1.subscription.abortAll()

	s2 := New()
	t.Cleanup(s2.subscription.abortAll)
	second := ask(s2, `[`+msg("user", "hello")+`,`+msg("assistant", first)+`,`+msg("user", "and?")+`]`)
	if second != "turn2" {
		t.Fatalf("second = %q", second)
	}
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log := string(b)
	if !strings.Contains(log, "--resume sess-persist") {
		t.Fatalf("gateway restart did not restore the persisted inner Claude session:\n%s", log)
	}
	parts := strings.Split(log, "stdin:")
	if len(parts) < 3 {
		t.Fatalf("bridge log missing second stdin:\n%s", log)
	}
	secondPrompt := parts[len(parts)-1]
	if !strings.Contains(secondPrompt, "and?") || strings.Contains(secondPrompt, "Human: hello") || strings.Contains(secondPrompt, "turn1") {
		t.Fatalf("restored turn replayed old history instead of only the suffix:\n%s", secondPrompt)
	}
}

func TestClaudeDesktopSuggestionDoesNotAdvanceInteractiveConversation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script stands in for interactive bridge")
	}
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	dir := t.TempDir()
	bridge := filepath.Join(dir, "claude-bridge")
	logPath := filepath.Join(dir, "bridge.log")
	script := `#!/bin/sh
prompt=$(cat)
printf 'args:%s\nstdin:%s\n' "$*" "$prompt" >> "$FAKE_BRIDGE_LOG"
case "$prompt" in
  *"[SUGGESTION MODE:"*) sid=sess-suggest; text=SUGGEST ;;
  *)
    sid=sess-main
    case " $* " in
      *" --resume sess-main "*) text=MAIN2 ;;
      *) text=MAIN1 ;;
    esac
    ;;
esac
printf '{"type":"assistant","session_id":"%s","uuid":"row-%s","message":{"id":"m-%s","model":"claude-opus-5-5","content":[{"type":"text","text":"%s"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":3}}}\n' "$sid" "$text" "$text" "$text"
printf '{"type":"result","subtype":"success","is_error":false,"session_id":"%s","stop_reason":"end_turn","result":"%s"}\n' "$sid" "$text"
`
	if err := os.WriteFile(bridge, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAGPIE_CLAUDE_INTERACTIVE_BRIDGE", bridge)
	t.Setenv("FAKE_BRIDGE_LOG", logPath)
	p := provider.Provider{ID: "claude", Account: &provider.Account{Agent: "claude", User: "u"}}
	msg := func(role, text string) string { return `{"role":"` + role + `","content":` + strconv.Quote(text) + `}` }
	ask := func(s *Server, tools, msgs string) (int, string) {
		t.Helper()
		body := `{"model":"claude-opus-5-5","max_tokens":100,"tools":` + tools + `,"messages":` + msgs + `}`
		req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
		req.Header.Set(SessionHeader, "desktop-session-suggestion")
		rec := httptest.NewRecorder()
		var u Usage
		code, why := s.serveClaudeSubscription(rec, req, provider.Anthropic, p, "claude-opus-5-5", []byte(body), &u)
		if code != 200 {
			return code, why + "\n" + rec.Body.String()
		}
		var res struct {
			Content []struct{ Text string } `json:"content"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || len(res.Content) == 0 {
			t.Fatalf("answer: %s (%v)", rec.Body, err)
		}
		return code, res.Content[0].Text
	}

	s := New()
	t.Cleanup(s.subscription.abortAll)
	tools := `[{"name":"read","input_schema":{"type":"object"}}]`
	code, first := ask(s, tools, `[`+msg("user", "hello")+`]`)
	if code != 200 || first != "MAIN1" {
		t.Fatalf("main first = %d %q", code, first)
	}

	owner := "claude\x00u\x00" + ownHome
	before, ok := s.subscription.interactiveSession(owner, "desktop-session-suggestion")
	if !ok || before.SessionID != "sess-main" {
		t.Fatalf("main persisted state missing before suggestion: %#v, %v", before, ok)
	}
	suggestionPrompt := "[SUGGESTION MODE: Suggest what the user might naturally type next into Claude Code.]\nReturn one short suggestion."
	code, suggestion := ask(s, `[]`, `[`+msg("user", "hello")+`,`+msg("assistant", first)+`,`+msg("user", suggestionPrompt)+`]`)
	if code != 200 || suggestion != "SUGGEST" {
		t.Fatalf("suggestion = %d %q", code, suggestion)
	}

	after, ok := s.subscription.interactiveSession(owner, "desktop-session-suggestion")
	if !ok || after.SessionID != before.SessionID || after.ConvKey != before.ConvKey || after.ReplyKey != before.ReplyKey {
		t.Fatalf("suggestion advanced main persisted checkpoint:\nbefore=%#v\nafter=%#v", before, after)
	}

	code, second := ask(s, tools, `[`+msg("user", "hello")+`,`+msg("assistant", first)+`,`+msg("user", "real next turn")+`]`)
	if code != 200 || second != "MAIN2" {
		t.Fatalf("real turn after suggestion = %d %q", code, second)
	}
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log := string(b)
	var suggestionIsolated, realResumed bool
	for _, block := range strings.Split(log, "args:") {
		switch {
		case strings.Contains(block, "SUGGESTION MODE:"):
			suggestionIsolated = !strings.Contains(block, "--resume sess-main") && !strings.Contains(block, "--fork-session") && strings.Contains(block, "--effort low")
		case strings.Contains(block, "real next turn"):
			realResumed = strings.Contains(block, "--resume sess-main") && !strings.Contains(block, "--fork-session")
		}
	}
	if !suggestionIsolated || !realResumed {
		t.Fatalf("suggestion must use an isolated low-effort run while the real turn resumes the main session:\n%s", log)
	}
}

func TestClaudeDesktopTurnCompanionDetection(t *testing.T) {
	for _, text := range []string{
		"[Your previous response had no visible output. Please continue and produce a user-visible response.]",
		"Your response above was cut off mid-stream. Resume directly from where it stops — no apology, no recap. If none of it survived, answer the request from the start.",
	} {
		req := &Request{Messages: []Message{{Role: "user", Parts: []Part{{Kind: Text, Text: text}}}}}
		if got := claudeDesktopTurnCompanionSuffix(req); len(got) != 1 || got[0].Role != "user" {
			t.Fatalf("turn companion %q not detected: %#v", text, got)
		}
	}
	req := &Request{Messages: []Message{{Role: "user", Parts: []Part{{Kind: Text, Text: "normal user message"}}}}}
	if got := claudeDesktopTurnCompanionSuffix(req); got != nil {
		t.Fatalf("ordinary user message detected as turn companion: %#v", got)
	}
}

func TestClaudeDesktopTurnCompanionResumesDirtyInteractiveSession(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script stands in for interactive bridge")
	}
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	dir := t.TempDir()
	bridge := filepath.Join(dir, "claude-bridge")
	logPath := filepath.Join(dir, "bridge.log")
	script := `#!/bin/sh
prompt=$(cat)
printf 'args:%s\nstdin:%s\n' "$*" "$prompt" >> "$FAKE_BRIDGE_LOG"
echo '{"type":"assistant","session_id":"sess-main","uuid":"row-recovered","message":{"id":"m-recovered","model":"claude-opus-5-5","content":[{"type":"text","text":"RECOVERED"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":3}}}'
echo '{"type":"result","subtype":"success","is_error":false,"session_id":"sess-main","stop_reason":"end_turn","result":"RECOVERED"}'
`
	if err := os.WriteFile(bridge, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAGPIE_CLAUDE_INTERACTIVE_BRIDGE", bridge)
	t.Setenv("FAKE_BRIDGE_LOG", logPath)
	p := provider.Provider{ID: "claude", Account: &provider.Account{Agent: "claude", User: "u"}}
	owner := "claude\x00u\x00" + ownHome
	outer := "desktop-session-turn-companion"
	baseReq := &Request{Model: "claude-opus-5-5", Tools: []Tool{{Name: "read", Schema: json.RawMessage(`{"type":"object"}`)}}}
	b := newSubscriptionBridge()
	b.saveInteractiveSession(owner, outer, interactiveSessionEntry{
		SessionID:  "sess-main",
		ConvKey:    "stale-conversation-checkpoint",
		ReplyKey:   "stale-reply-checkpoint",
		ContextKey: turnKey(owner, baseReq, nil),
		Dirty:      true,
	})

	s := New()
	t.Cleanup(s.subscription.abortAll)
	companion := "Your response above was cut off mid-stream. Resume directly from where it stops — no apology, no recap. If none of it survived, answer the request from the start."
	body := `{"model":"claude-opus-5-5","max_tokens":100,"tools":[{"name":"read","input_schema":{"type":"object"}}],"messages":[` +
		`{"role":"user","content":"old outer history"},` +
		`{"role":"assistant","content":"rewritten outer reply"},` +
		`{"role":"user","content":` + strconv.Quote(companion) + `}]}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req.Header.Set(SessionHeader, outer)
	rec := httptest.NewRecorder()
	var u Usage
	if code, why := s.serveClaudeSubscription(rec, req, provider.Anthropic, p, "claude-opus-5-5", []byte(body), &u); code != 200 {
		t.Fatalf("%d %s: %s", code, why, rec.Body.String())
	}
	var res struct {
		Content []struct{ Text string } `json:"content"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || len(res.Content) == 0 || res.Content[0].Text != "RECOVERED" {
		t.Fatalf("answer: %s (%v)", rec.Body, err)
	}
	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log := string(logBytes)
	if !strings.Contains(log, "--resume sess-main") || !strings.Contains(log, companion) {
		t.Fatalf("turn companion did not resume the dirty inner session:\n%s", log)
	}
	if strings.Contains(log, "old outer history") || strings.Contains(log, "rewritten outer reply") {
		t.Fatalf("turn companion replayed outer history:\n%s", log)
	}
	after, ok := s.subscription.interactiveSession(owner, outer)
	wantReply := assistantReplyKey(Message{Role: "assistant", Parts: []Part{{Kind: Text, Text: "RECOVERED"}}})
	if !ok || after.Dirty || after.SessionID != "sess-main" || after.ReplyKey != wantReply {
		t.Fatalf("turn companion did not leave a clean recovered checkpoint: %#v, %v", after, ok)
	}
}

func TestClaudeInteractiveSessionRestoresAfterEarlierHistoryRewrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script stands in for interactive bridge")
	}
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	dir := t.TempDir()
	bridge := filepath.Join(dir, "claude-bridge")
	logPath := filepath.Join(dir, "bridge.log")
	script := `#!/bin/sh
prompt=$(cat)
printf 'args:%s\nstdin:%s\n' "$*" "$prompt" >> "$FAKE_BRIDGE_LOG"
case " $* " in
  *" --resume sess-persist "*) text=turn2 ;;
  *) text=turn1 ;;
esac
printf '{"type":"assistant","session_id":"sess-persist","uuid":"row-%s","message":{"id":"m-%s","model":"claude-opus-5-5","content":[{"type":"text","text":"%s"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":3}}}\n' "$text" "$text" "$text"
printf '{"type":"result","subtype":"success","is_error":false,"session_id":"sess-persist","stop_reason":"end_turn","result":"%s"}\n' "$text"
`
	if err := os.WriteFile(bridge, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAGPIE_CLAUDE_INTERACTIVE_BRIDGE", bridge)
	t.Setenv("FAKE_BRIDGE_LOG", logPath)
	p := provider.Provider{ID: "claude", Account: &provider.Account{Agent: "claude", User: "u"}}
	msg := func(role, text string) string { return `{"role":"` + role + `","content":` + strconv.Quote(text) + `}` }
	ask := func(s *Server, msgs string) string {
		t.Helper()
		body := `{"model":"claude-opus-5-5","max_tokens":100,"tools":[{"name":"read","input_schema":{"type":"object"}}],"messages":` + msgs + `}`
		req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
		req.Header.Set(SessionHeader, "desktop-session-rewritten")
		rec := httptest.NewRecorder()
		var u Usage
		if code, why := s.serveClaudeSubscription(rec, req, provider.Anthropic, p, "claude-opus-5-5", []byte(body), &u); code != 200 {
			t.Fatalf("%d %s: %s", code, why, rec.Body.String())
		}
		var res struct {
			Content []struct{ Text string } `json:"content"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || len(res.Content) == 0 {
			t.Fatalf("answer: %s (%v)", rec.Body, err)
		}
		return res.Content[0].Text
	}

	s1 := New()
	first := ask(s1, `[`+msg("user", "hello")+`]`)
	if first != "turn1" {
		t.Fatalf("first = %q", first)
	}
	s1.subscription.abortAll()

	// Claude Desktop can rewrite/compact older history while preserving the
	// last completed assistant reply. That must not turn a restart into a
	// full-history replay: the persisted inner session already owns that
	// history and only the new user suffix belongs in its next prompt.
	s2 := New()
	t.Cleanup(s2.subscription.abortAll)
	second := ask(s2, `[`+msg("user", "hello rewritten by outer client")+`,`+msg("assistant", first)+`,`+msg("user", "and?")+`]`)
	if second != "turn2" {
		t.Fatalf("rewritten outer history lost persisted inner session: second = %q", second)
	}
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log := string(b)
	if !strings.Contains(log, "--resume sess-persist") {
		t.Fatalf("rewritten outer history did not resume persisted inner session:\n%s", log)
	}
	parts := strings.Split(log, "stdin:")
	if len(parts) < 3 {
		t.Fatalf("bridge log missing second stdin:\n%s", log)
	}
	secondPrompt := parts[len(parts)-1]
	if !strings.Contains(secondPrompt, "and?") || strings.Contains(secondPrompt, "hello rewritten") || strings.Contains(secondPrompt, "turn1") {
		t.Fatalf("rewritten outer history was replayed instead of only the suffix:\n%s", secondPrompt)
	}
}

func TestClaudeInteractiveCheckpointMissRefusesFullHistoryReplay(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script stands in for interactive bridge")
	}
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	dir := t.TempDir()
	bridge := filepath.Join(dir, "claude-bridge")
	logPath := filepath.Join(dir, "bridge.log")
	script := `#!/bin/sh
prompt=$(cat)
printf 'args:%s\nstdin:%s\n' "$*" "$prompt" >> "$FAKE_BRIDGE_LOG"
echo '{"type":"assistant","session_id":"sess-persist","uuid":"row-turn1","message":{"id":"m-turn1","model":"claude-opus-5-5","content":[{"type":"text","text":"turn1"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":3}}}'
echo '{"type":"result","subtype":"success","is_error":false,"session_id":"sess-persist","stop_reason":"end_turn","result":"turn1"}'
`
	if err := os.WriteFile(bridge, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAGPIE_CLAUDE_INTERACTIVE_BRIDGE", bridge)
	t.Setenv("FAKE_BRIDGE_LOG", logPath)
	p := provider.Provider{ID: "claude", Account: &provider.Account{Agent: "claude", User: "u"}}
	outer := "desktop-session-checkpoint-miss"
	request := func(s *Server, msgs string) (int, string) {
		t.Helper()
		body := `{"model":"claude-opus-5-5","max_tokens":100,"tools":[{"name":"read","input_schema":{"type":"object"}}],"messages":` + msgs + `}`
		req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
		req.Header.Set(SessionHeader, outer)
		rec := httptest.NewRecorder()
		var u Usage
		code, why := s.serveClaudeSubscription(rec, req, provider.Anthropic, p, "claude-opus-5-5", []byte(body), &u)
		return code, why + "\n" + rec.Body.String()
	}
	msg := func(role, text string) string { return `{"role":"` + role + `","content":` + strconv.Quote(text) + `}` }

	s1 := New()
	if code, body := request(s1, `[`+msg("user", "hello")+`]`); code != 200 {
		t.Fatalf("first request: %d %s", code, body)
	}
	s1.subscription.abortAll()

	// Both the old history and the last completed assistant reply were
	// rewritten. There is no safe checkpoint from which the persisted inner
	// session can accept only a suffix. Replaying the entire outer history into
	// a fresh inner session is precisely the corruption this test forbids.
	s2 := New()
	t.Cleanup(s2.subscription.abortAll)
	code, body := request(s2, `[`+
		msg("user", "hello rewritten")+`,`+
		msg("assistant", "turn1 rewritten")+`,`+
		msg("user", "and?")+`]`)
	if code != 502 || !strings.Contains(body, "checkpoint_not_found") {
		t.Fatalf("checkpoint miss = %d %s; want 502 checkpoint_not_found", code, body)
	}
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(b), "args:"); got != 1 {
		t.Fatalf("checkpoint miss started a second bridge and replayed history (%d calls):\n%s", got, b)
	}
}

func TestClaudeInteractiveMissingStateRefusesEstablishedHistoryReplay(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script stands in for interactive bridge")
	}
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	dir := t.TempDir()
	bridge := filepath.Join(dir, "claude-bridge")
	logPath := filepath.Join(dir, "bridge.log")
	script := `#!/bin/sh
prompt=$(cat)
printf 'args:%s\nstdin:%s\n' "$*" "$prompt" >> "$FAKE_BRIDGE_LOG"
echo '{"type":"assistant","session_id":"sess-new","uuid":"row-new","message":{"id":"m-new","model":"claude-opus-5-5","content":[{"type":"text","text":"fresh"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":3}}}'
echo '{"type":"result","subtype":"success","is_error":false,"session_id":"sess-new","stop_reason":"end_turn","result":"fresh"}'
`
	if err := os.WriteFile(bridge, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAGPIE_CLAUDE_INTERACTIVE_BRIDGE", bridge)
	t.Setenv("FAKE_BRIDGE_LOG", logPath)
	p := provider.Provider{ID: "claude", Account: &provider.Account{Agent: "claude", User: "u"}}
	body := `{"model":"claude-opus-5-5","max_tokens":100,"tools":[{"name":"read","input_schema":{"type":"object"}}],"messages":[` +
		`{"role":"user","content":"old user"},` +
		`{"role":"assistant","content":"old assistant"},` +
		`{"role":"user","content":"new user"}]}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req.Header.Set(SessionHeader, "desktop-session-missing-established")
	rec := httptest.NewRecorder()
	var u Usage
	code, why := New().serveClaudeSubscription(rec, req, provider.Anthropic, p, "claude-opus-5-5", []byte(body), &u)
	if code != 502 || !strings.Contains(why+rec.Body.String(), "state_missing on established conversation") {
		t.Fatalf("missing established state = %d %s %s", code, why, rec.Body.String())
	}
	if b, err := os.ReadFile(logPath); err == nil && len(b) > 0 {
		t.Fatalf("missing established state replayed history into a fresh bridge:\n%s", b)
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestClaudeInteractiveDirtyPersistedSessionIsNotRestored(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script stands in for interactive bridge")
	}
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	dir := t.TempDir()
	bridge := filepath.Join(dir, "claude-bridge")
	logPath := filepath.Join(dir, "bridge.log")
	script := `#!/bin/sh
prompt=$(cat)
printf 'args:%s stdin:%s\n' "$*" "$prompt" >> "$FAKE_BRIDGE_LOG"
echo '{"type":"assistant","session_id":"sess-new","uuid":"row-new","message":{"id":"m-new","model":"claude-opus-5-5","content":[{"type":"text","text":"fresh"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":3}}}'
echo '{"type":"result","subtype":"success","is_error":false,"session_id":"sess-new","stop_reason":"end_turn","result":"fresh"}'
`
	if err := os.WriteFile(bridge, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAGPIE_CLAUDE_INTERACTIVE_BRIDGE", bridge)
	t.Setenv("FAKE_BRIDGE_LOG", logPath)
	owner := "claude\x00u\x00" + ownHome
	outer := "desktop-session-dirty"
	reqForKey := &Request{Model: "claude-opus-5-5", Tools: []Tool{{Name: "read", Schema: json.RawMessage(`{"type":"object"}`)}}}
	b := newSubscriptionBridge()
	b.saveInteractiveSession(owner, outer, interactiveSessionEntry{
		SessionID: "sess-old", ConvKey: "checkpoint", ContextKey: turnKey(owner, reqForKey, nil), Dirty: true,
	})

	s := New()
	t.Cleanup(s.subscription.abortAll)
	p := provider.Provider{ID: "claude", Account: &provider.Account{Agent: "claude", User: "u"}}
	body := `{"model":"claude-opus-5-5","max_tokens":100,"tools":[{"name":"read","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hello"}]}`
	h := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	h.Header.Set(SessionHeader, outer)
	rec := httptest.NewRecorder()
	var u Usage
	if code, why := s.serveClaudeSubscription(rec, h, provider.Anthropic, p, "claude-opus-5-5", []byte(body), &u); code != 502 || !strings.Contains(why, "dirty") {
		t.Fatalf("dirty persisted session = %d %s: %s; want 502 dirty", code, why, rec.Body.String())
	}
	data, _ := os.ReadFile(logPath)
	if len(data) != 0 {
		t.Fatalf("dirty persisted session launched a bridge instead of failing closed:\n%s", data)
	}
}

func TestConversationSuffixAfterRequiresExactCompletedAssistantCheckpoint(t *testing.T) {
	owner := "claude\x00u"
	base := []Message{
		{Role: "user", Parts: []Part{{Kind: Text, Text: "hello"}}},
		{Role: "assistant", Parts: []Part{{Kind: Text, Text: "turn1"}}},
	}
	keys := conversationKeys(owner, base)
	if len(keys) != 1 {
		t.Fatalf("keys = %v", keys)
	}
	msgs := append(slices.Clone(base), Message{Role: "user", Parts: []Part{{Kind: Text, Text: "and?"}}})
	since, ok := conversationSuffixAfter(owner, msgs, keys[0])
	if !ok || len(since) != 1 || since[0].Parts[0].Text != "and?" {
		t.Fatalf("suffix = %#v, %v", since, ok)
	}
	diverged := []Message{
		{Role: "user", Parts: []Part{{Kind: Text, Text: "hello"}}},
		{Role: "assistant", Parts: []Part{{Kind: Text, Text: "edited"}}},
		{Role: "user", Parts: []Part{{Kind: Text, Text: "and?"}}},
	}
	if got, ok := conversationSuffixAfter(owner, diverged, keys[0]); ok || got != nil {
		t.Fatalf("diverged history restored: %#v", got)
	}
}

func TestClaudeInteractiveRestoreReasons(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	owner := "claude\x00u"
	baseReq := Request{
		Model: "claude-opus-5-5",
		Tools: []Tool{{Name: "read", Schema: json.RawMessage(`{"type":"object"}`)}},
	}
	base := []Message{
		{Role: "user", Parts: []Part{{Kind: Text, Text: "hello"}}},
		{Role: "assistant", Parts: []Part{{Kind: Text, Text: "turn1"}}},
	}
	keys := conversationKeys(owner, base)
	if len(keys) != 1 {
		t.Fatalf("keys = %v", keys)
	}
	contextKey := turnKey(owner, &baseReq, nil)
	replyKey := assistantReplyKey(base[1])
	b := newSubscriptionBridge()

	missingReq := baseReq
	missingReq.Messages = append(slices.Clone(base), Message{Role: "user", Parts: []Part{{Kind: Text, Text: "and?"}}})
	if _, _, got := b.restoreInteractiveSession(owner, "missing", &missingReq); got != interactiveRestoreStateMissing {
		t.Fatalf("state missing reason = %q", got)
	}

	b.saveInteractiveSession(owner, "dirty", interactiveSessionEntry{
		SessionID: "sess-dirty", ConvKey: keys[0], ReplyKey: replyKey, ContextKey: contextKey, Dirty: true,
	})
	if _, _, got := b.restoreInteractiveSession(owner, "dirty", &missingReq); got != interactiveRestoreDirty {
		t.Fatalf("dirty reason = %q", got)
	}

	b.saveInteractiveSession(owner, "context", interactiveSessionEntry{
		SessionID: "sess-context", ConvKey: keys[0], ReplyKey: replyKey, ContextKey: "different",
	})
	if _, since, got := b.restoreInteractiveSession(owner, "context", &missingReq); got != interactiveRestoreExact || len(since) != 1 || since[0].Parts[0].Text != "and?" {
		t.Fatalf("context change should preserve exact history continuity: status=%q suffix=%#v", got, since)
	}

	b.saveInteractiveSession(owner, "checkpoint", interactiveSessionEntry{
		SessionID: "sess-checkpoint", ConvKey: "missing", ReplyKey: "missing", ContextKey: contextKey,
	})
	if _, _, got := b.restoreInteractiveSession(owner, "checkpoint", &missingReq); got != interactiveRestoreCheckpointNotFound {
		t.Fatalf("checkpoint reason = %q", got)
	}

	b.saveInteractiveSession(owner, "suffix", interactiveSessionEntry{
		SessionID: "sess-suffix", ConvKey: keys[0], ReplyKey: replyKey, ContextKey: contextKey,
	})
	suffixReq := baseReq
	suffixReq.Messages = append(slices.Clone(base), Message{Role: "user", Parts: []Part{{Kind: ToolResult, CallID: "toolu_1", Text: "old tool output"}}})
	if _, _, got := b.restoreInteractiveSession(owner, "suffix", &suffixReq); got != interactiveRestoreSuffixRejected {
		t.Fatalf("suffix rejected reason = %q", got)
	}
}

func TestClaudeInteractiveContextRefreshCarriesCurrentInstructionsOnly(t *testing.T) {
	req := &Request{System: "CURRENT_SYSTEM", ToolChoice: "required"}
	got := renderClaudeInteractiveContextRefresh(req)
	if !strings.Contains(got, "CURRENT_SYSTEM") || !strings.Contains(got, "must call at least one available tool") {
		t.Fatalf("context refresh lost current instructions: %q", got)
	}
	if !strings.Contains(got, "supersede") || !strings.Contains(got, "without replaying prior history") {
		t.Fatalf("context refresh does not clearly replace prior outer context: %q", got)
	}
}

func TestClaudeInteractiveSessionWorkDirIsStableAndInTemp(t *testing.T) {
	tmp := t.TempDir()
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(key, tmp)
	}
	dir1, err := interactiveSessionWorkDir("claude\x00u", "desktop-session-workdir")
	if err != nil {
		t.Fatal(err)
	}
	dir2, err := interactiveSessionWorkDir("claude\x00u", "desktop-session-workdir")
	if err != nil {
		t.Fatal(err)
	}
	if dir1 != dir2 {
		t.Fatalf("interactive cwd changed: %q != %q", dir1, dir2)
	}
	root, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		root = tmp
	}
	got, err := filepath.EvalSymlinks(dir1)
	if err != nil {
		t.Fatal(err)
	}
	if got != root && !strings.HasPrefix(got, root+string(os.PathSeparator)) {
		t.Fatalf("interactive cwd %q is not under temp %q", got, root)
	}
	fi, err := os.Stat(dir1)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		t.Fatalf("interactive cwd is writable by others: %v", fi.Mode().Perm())
	}
}

func TestClaudeInteractiveSessionWorkDirRejectsWritableByOthers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows permission bits do not model a shared Unix temp directory")
	}
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	owner, outer := "claude\x00u", "desktop-session-unsafe-workdir"
	dir := filepath.Join(tmp, "magpie-claude-interactive-"+interactiveSessionStateKey(owner, outer))
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := interactiveSessionWorkDir(owner, outer); err == nil {
		t.Fatalf("world-writable interactive cwd %q was accepted", dir)
	}
}

func TestClaudeInteractiveEffortChangeKeepsInnerSession(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script stands in for interactive bridge")
	}
	dir := t.TempDir()
	bridge := filepath.Join(dir, "claude-bridge")
	logPath := filepath.Join(dir, "bridge.log")
	script := `#!/bin/sh
prompt=$(cat)
printf 'args:%s stdin:%s\n' "$*" "$prompt" >> "$FAKE_BRIDGE_LOG"
case " $* " in *" --resume sess-effort "*) text=turn2 ;; *) text=turn1 ;; esac
printf '{"type":"assistant","session_id":"sess-effort","uuid":"row-%s","message":{"id":"m-%s","model":"claude-opus-5-5","content":[{"type":"text","text":"%s"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":3}}}\n' "$text" "$text" "$text"
printf '{"type":"result","subtype":"success","is_error":false,"session_id":"sess-effort","stop_reason":"end_turn","result":"%s"}\n' "$text"
`
	if err := os.WriteFile(bridge, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAGPIE_CLAUDE_INTERACTIVE_BRIDGE", bridge)
	t.Setenv("FAKE_BRIDGE_LOG", logPath)
	s := New()
	t.Cleanup(s.subscription.abortAll)
	p := provider.Provider{ID: "claude", Account: &provider.Account{Agent: "claude", User: "u"}}
	ask := func(effort, msgs string) string {
		body := `{"model":"claude-opus-5-5","max_tokens":100,"thinking":{"type":"adaptive"},"output_config":{"effort":"` + effort + `"},"tools":[{"name":"read","input_schema":{"type":"object"}}],"messages":` + msgs + `}`
		rec := httptest.NewRecorder()
		var u Usage
		if code, why := s.serveClaudeSubscription(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)), provider.Anthropic, p, "claude-opus-5-5", []byte(body), &u); code != 200 {
			t.Fatalf("%d %s: %s", code, why, rec.Body.String())
		}
		var res struct {
			Content []struct{ Text string } `json:"content"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &res)
		return res.Content[0].Text
	}
	first := ask("max", `[{"role":"user","content":"hello"}]`)
	second := ask("high", `[{"role":"user","content":"hello"},{"role":"assistant","content":"`+first+`"},{"role":"user","content":"and?"}]`)
	if second != "turn2" {
		t.Fatalf("second = %q", second)
	}
	b, _ := os.ReadFile(logPath)
	log := string(b)
	if !strings.Contains(log, "--resume sess-effort") || !strings.Contains(log, "--effort high") {
		t.Fatalf("effort change started a new inner session instead of resuming it at high:\n%s", log)
	}
}

func TestCleanClaudeEnvRemovesGatewayOverrides(t *testing.T) {
	got := cleanClaudeEnv([]string{
		"PATH=/bin", "ANTHROPIC_BASE_URL=http://127.0.0.1:3425",
		"ANTHROPIC_API_KEY=x", "ANTHROPIC_AUTH_TOKEN=y", "CLAUDECODE=1",
		"CLAUDE_CODE_ENTRYPOINT=cli", "CLAUDE_CODE_SSE_PORT=9999", "KEEP=yes",
	})
	joined := strings.Join(got, "\n")
	for _, forbidden := range []string{
		"ANTHROPIC_BASE_URL=", "ANTHROPIC_API_KEY=", "ANTHROPIC_AUTH_TOKEN=", "CLAUDECODE=",
		"CLAUDE_CODE_ENTRYPOINT=", "CLAUDE_CODE_SSE_PORT=",
	} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("kept %s in %q", forbidden, joined)
		}
	}
	for _, want := range []string{"PATH=/bin", "KEEP=yes", "ENABLE_CLAUDEAI_MCP_SERVERS=0", "DISABLE_AUTO_COMPACT=1"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %s in %q", want, joined)
		}
	}
}

// fakeClaude answers each line it is given with the process it runs in and
// how many turns that process has had, as Claude Code's stream-json does.
func fakeClaude(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("a shell script stands in for Claude Code")
	}
	dir := t.TempDir()
	script := `#!/bin/sh
n=0
while read -r line; do
  n=$((n+1))
  echo '{"type":"stream_event","event":{"type":"message_start","message":{"id":"m","model":"claude-sonnet-5","usage":{"input_tokens":1}}}}'
  echo '{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"pid '$$' turn '$n'"}}}'
  echo '{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}}'
  echo '{"type":"stream_event","event":{"type":"message_stop"}}'
  echo '{"type":"result","subtype":"success","is_error":false,"result":""}'
done
`
	os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// A conversation's next turn goes to the Claude Code that had its last,
// told only what the user said since; another conversation, or the same one
// with its reply changed, gets a Claude Code of its own.
func TestClaudeRunKeptForTheNextTurn(t *testing.T) {
	fakeClaude(t)
	s := New()
	p := provider.Provider{ID: "claude", Account: &provider.Account{Agent: "claude", User: "u"}}
	ask := func(msgs string) string {
		t.Helper()
		body := `{"model":"claude-sonnet-5","max_tokens":100,"system":"be brief","tools":[{"name":"read","input_schema":{"type":"object"}}],"messages":` + msgs + `}`
		rec := httptest.NewRecorder()
		var u Usage
		if code, msg := s.serveClaudeSubscription(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)), provider.Anthropic, p, "claude-sonnet-5", []byte(body), &u); code != 200 {
			t.Fatalf("%d %s", code, msg)
		}
		var res struct {
			Content []struct{ Text string } `json:"content"`
		}
		json.Unmarshal(rec.Body.Bytes(), &res)
		if len(res.Content) == 0 {
			t.Fatalf("no answer: %s", rec.Body)
		}
		return res.Content[0].Text
	}
	msg := func(role, text string) string { return `{"role":"` + role + `","content":` + strconv.Quote(text) + `}` }

	first := ask(`[` + msg("user", "hi") + `]`)
	pid, _, _ := strings.Cut(strings.TrimPrefix(first, "pid "), " ")
	if !strings.HasSuffix(first, "turn 1") {
		t.Fatalf("first: %q", first)
	}
	second := ask(`[` + msg("user", "hi") + `,` + msg("assistant", " "+first+"\n") + `,` + msg("user", "and?") + `]`)
	if second != "pid "+pid+" turn 2" {
		t.Fatalf("second: %q, first %q", second, first)
	}
	if other := ask(`[` + msg("user", "bye") + `,` + msg("assistant", second) + `,` + msg("user", "and?") + `]`); strings.Contains(other, "pid "+pid) {
		t.Fatalf("another conversation: %q", other)
	}
	third := ask(`[` + msg("user", "hi") + `,` + msg("assistant", first) + `,` + msg("user", "and?") + `,` + msg("assistant", "edited") + `,` + msg("user", "so?") + `]`)
	if strings.Contains(third, "pid "+pid) {
		t.Fatalf("an edited reply: %q", third)
	}
}

// A one-off ask — a lone message and no tools, as an agent's title or the
// router's classifier sends — leaves no Claude Code waiting for a next
// turn that won't come; a conversation already going on keeps its run.
func TestClaudeOneOffAskNotKept(t *testing.T) {
	fakeClaude(t)
	s := New()
	p := provider.Provider{ID: "claude", Account: &provider.Account{Agent: "claude", User: "u"}}
	ask := func(msgs string) string {
		t.Helper()
		body := `{"model":"claude-sonnet-5","max_tokens":100,"messages":` + msgs + `}`
		rec := httptest.NewRecorder()
		var u Usage
		if code, msg := s.serveClaudeSubscription(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)), provider.Anthropic, p, "claude-sonnet-5", []byte(body), &u); code != 200 {
			t.Fatalf("%d %s", code, msg)
		}
		var res struct {
			Content []struct{ Text string } `json:"content"`
		}
		json.Unmarshal(rec.Body.Bytes(), &res)
		if len(res.Content) == 0 {
			t.Fatalf("no answer: %s", rec.Body)
		}
		return res.Content[0].Text
	}
	msg := func(role, text string) string { return `{"role":"` + role + `","content":` + strconv.Quote(text) + `}` }
	idle := func() int {
		s.subscription.mu.Lock()
		defer s.subscription.mu.Unlock()
		return len(s.subscription.idle)
	}

	first := ask(`[` + msg("user", "a title for this") + `]`)
	if n := idle(); n != 0 {
		t.Fatalf("a one-off ask left %d runs waiting", n)
	}
	second := ask(`[` + msg("user", "a title for this") + `,` + msg("assistant", first) + `,` + msg("user", "and?") + `]`)
	if n := idle(); n != 1 {
		t.Fatalf("a conversation going on: %d runs waiting, want 1", n)
	}
	pid, _, _ := strings.Cut(strings.TrimPrefix(second, "pid "), " ")
	if third := ask(`[` + msg("user", "a title for this") + `,` + msg("assistant", first) + `,` + msg("user", "and?") + `,` + msg("assistant", second) + `,` + msg("user", "so?") + `]`); third != "pid "+pid+" turn 2" {
		t.Fatalf("third: %q, second %q", third, second)
	}
}

// An agent's run that fails after it began answering ends the stream with
// the error alone, not with a stop that reads as a finished reply.
func TestSubscriptionStreamErrorIsTheEnd(t *testing.T) {
	s := New()
	for _, from := range []provider.Protocol{provider.Anthropic, provider.Chat, provider.Responses} {
		start := func(ctx context.Context, req *Request) (*subscriptionRun, <-chan Event, error) {
			ch := make(chan Event, 3)
			ch <- Event{Kind: KStart}
			ch <- Event{Kind: KText, Text: "half"}
			ch <- Event{Kind: KError, Text: "it died"}
			close(ch)
			return &subscriptionRun{bridge: s.subscription}, ch, nil
		}
		body := `{"model":"m","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}],"input":"hi"}`
		rec := httptest.NewRecorder()
		var u Usage
		code, failed := s.serveSubscription(rec, httptest.NewRequest("POST", "/", strings.NewReader(body)), from, "Agent", "m", []byte(body), &u, start)
		out := rec.Body.String()
		if code != 200 || failed != "it died" || !strings.Contains(out, "it died") {
			t.Fatalf("%s: %d %q\n%s", from, code, failed, out)
		}
		for _, end := range []string{"message_stop", "[DONE]", `"stop"`, "response.completed"} {
			if strings.Contains(out, end) {
				t.Fatalf("%s: %s after the error:\n%s", from, end, out)
			}
		}
	}
}

// Claude Code's own WebSearch runs inside the turn: the client hears one
// message, with no call it did not offer, and the args ask for WebSearch
// only when the client offered a web search.
func TestClaudeOwnWebSearchStaysInside(t *testing.T) {
	body := `{"model":"claude-sonnet-5","max_tokens":10,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":8}]}`
	req, err := parseAnthropic([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if !req.WebSearch || len(req.Tools) != 0 {
		t.Fatalf("web search not noted: %+v", req)
	}
	on := strings.Join(claudeCLIArgs("m", `{}`, "", true), "\x00")
	off := strings.Join(claudeCLIArgs("m", `{}`, "", false), "\x00")
	if !strings.Contains(on, "--tools\x00WebSearch\x00") || !strings.Contains(off, "--tools\x00\x00") {
		t.Fatalf("tools: %q / %q", on, off)
	}

	ev := func(e string) string { return `{"type":"stream_event","event":` + e + `}` }
	lines := []string{
		ev(`{"type":"message_start","message":{"id":"m1","model":"x","usage":{"input_tokens":10}}}`),
		ev(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		ev(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Let me look. "}}`),
		ev(`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"t1","name":"WebSearch"}}`),
		ev(`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"go\"}"}}`),
		ev(`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}`),
		ev(`{"type":"message_stop"}`),
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"results"}]}}`,
		ev(`{"type":"message_start","message":{"id":"m2","model":"x","usage":{"input_tokens":30}}}`),
		ev(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		ev(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Go 1.27.1"}}`),
		ev(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`),
		ev(`{"type":"message_stop"}`),
	}
	run := &subscriptionRun{}
	seg := run.attach()
	go run.readOutput(strings.NewReader(strings.Join(lines, "\n") + "\n"))
	var got []string
	var usage Usage
	for e := range seg {
		switch e.Kind {
		case KStart:
			got = append(got, "start:"+e.MsgID)
			usage.add(e.Usage)
		case KText:
			got = append(got, "text:"+e.Text)
		case KToolStart, KToolArgs:
			got = append(got, "tool:"+e.Name+e.Text)
		case KStop:
			got = append(got, "stop:"+e.Stop)
		case KUsage:
			usage.add(e.Usage)
		}
	}
	if s := strings.Join(got, "|"); s != "start:m1|text:Let me look. |text:Go 1.27.1|stop:stop" {
		t.Fatalf("events: %s", s)
	}
	if usage.Input != 40 || usage.Output != 12 {
		t.Fatalf("usage: %+v", usage)
	}
}

func TestClaudeLimits(t *testing.T) {
	got := claudeLimits(json.RawMessage(`{"status":"allowed","resetsAt":1800000000,"rateLimitType":"five_hour","utilization":0.42,
		"unifiedWindows":{"five_hour":{"utilization":0.42,"resetsAt":1800000000},"seven_day":{"utilization":0.1,"resetsAt":1800500000}}}`))
	slices.SortFunc(got, func(a, b provider.ClaudeLimit) int { return strings.Compare(a.Kind, b.Kind) })
	want := []provider.ClaudeLimit{{Kind: "five_hour", Used: 0.42, ResetsAt: 1800000000}, {Kind: "seven_day", Used: 0.1, ResetsAt: 1800500000}}
	if !slices.Equal(got, want) {
		t.Fatalf("windows: %+v", got)
	}
	// without the windows: the one it is about, and one turned away is full
	if got := claudeLimits(json.RawMessage(`{"status":"allowed_warning","rateLimitType":"seven_day","utilization":0.9,"resetsAt":5}`)); !slices.Equal(got, []provider.ClaudeLimit{{Kind: "seven_day", Used: 0.9, ResetsAt: 5}}) {
		t.Fatalf("top level: %+v", got)
	}
	if got := claudeLimits(json.RawMessage(`{"status":"rejected","rateLimitType":"five_hour","resetsAt":7}`)); !slices.Equal(got, []provider.ClaudeLimit{{Kind: "five_hour", Used: 1, ResetsAt: 7}}) {
		t.Fatalf("rejected: %+v", got)
	}
	if got := claudeLimits(json.RawMessage(`{"status":"allowed"}`)); len(got) != 0 {
		t.Fatalf("nothing said: %+v", got)
	}
}

// A conversation switched to another model and back (KevinXC on Discord)
// has a new run carry it on: the one left waiting before the switch is
// let go then, not kept idleLongest for a turn that can't come back to it —
// a Claude Code process more with every switch.
func TestClaudeRunLetGoWhenTheConversationMovedOn(t *testing.T) {
	fakeClaude(t)
	s := New()
	p := provider.Provider{ID: "claude", Account: &provider.Account{Agent: "claude", User: "u"}}
	ask := func(model, msgs string) string {
		t.Helper()
		body := `{"model":"` + model + `","max_tokens":100,"tools":[{"name":"read","input_schema":{"type":"object"}}],"messages":` + msgs + `}`
		rec := httptest.NewRecorder()
		var u Usage
		if code, msg := s.serveClaudeSubscription(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)), provider.Anthropic, p, model, []byte(body), &u); code != 200 {
			t.Fatalf("%d %s", code, msg)
		}
		var res struct {
			Content []struct{ Text string } `json:"content"`
		}
		json.Unmarshal(rec.Body.Bytes(), &res)
		if len(res.Content) == 0 {
			t.Fatalf("no answer: %s", rec.Body)
		}
		return res.Content[0].Text
	}
	msg := func(role, text string) string { return `{"role":"` + role + `","content":` + strconv.Quote(text) + `}` }
	waiting := func() []string {
		s.subscription.mu.Lock()
		defer s.subscription.mu.Unlock()
		var pids []string
		for _, run := range s.subscription.idle {
			pids = append(pids, strconv.Itoa(run.cmd.Process.Pid))
		}
		return pids
	}
	pidOf := func(said string) string { pid, _, _ := strings.Cut(strings.TrimPrefix(said, "pid "), " "); return pid }

	conv := msg("user", "one")
	first := ask("claude-sonnet-5", `[`+conv+`]`)
	conv += `,` + msg("assistant", first) + `,` + msg("user", "two")
	if w := waiting(); len(w) != 1 || w[0] != pidOf(first) {
		t.Fatalf("after the first turn: %v, ran %q", w, first)
	}
	// a turn answered elsewhere, then back on the subscription
	conv += `,` + msg("assistant", "an answer from another provider") + `,` + msg("user", "three")
	back := ask("claude-sonnet-5", `[`+conv+`]`)
	if pidOf(back) == pidOf(first) {
		t.Fatalf("the run before the switch answered after it: %q", back)
	}
	if w := waiting(); len(w) != 1 || w[0] != pidOf(back) {
		t.Fatalf("after switching back: %v waiting, want only %s", w, pidOf(back))
	}
	// and another Claude model of the same account, for the next turn
	conv += `,` + msg("assistant", back) + `,` + msg("user", "four")
	other := ask("claude-opus-5-5", `[`+conv+`]`)
	if w := waiting(); len(w) != 1 || w[0] != pidOf(other) {
		t.Fatalf("after another Claude model: %v waiting, want only %s", w, pidOf(other))
	}
	// a conversation of its own keeps its run beside it
	ask("claude-sonnet-5", `[`+msg("user", "elsewhere")+`,`+msg("assistant", "x")+`,`+msg("user", "y")+`]`)
	if w := waiting(); len(w) != 2 {
		t.Fatalf("two conversations: %v waiting", w)
	}
}

func TestClaudeBridgeKeepsNoSessionsOnDisk(t *testing.T) {
	args := claudeCLIArgs("m", `{}`, "", false)
	if !slices.Contains(args, "--no-session-persistence") || !slices.Contains(args, "-p") {
		t.Fatalf("bridge runs must not persist sessions: %q", args)
	}
}

func TestSweepBridgeProjectsTakesOnlyTheBridgesFolders(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real", "T")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	real = evalSymlinks(real) // what Claude Code names a folder after
	link := filepath.Join(base, "link")
	if err := os.Symlink(filepath.Join(base, "real"), link); err != nil {
		t.Fatal(err)
	}
	claudeDir := filepath.Join(base, "claude")
	projects := filepath.Join(claudeDir, "projects")
	gone := []string{
		claudeProjectName(filepath.Join(real, "magpie-claude-1097091858")),
		claudeProjectName(filepath.Join(link, "T", "magpie-claude-42")),
	}
	kept := []string{
		claudeProjectName(filepath.Join(real, "magpie-claude-")),
		claudeProjectName(filepath.Join(real, "magpie-claude-12-x")),
		claudeProjectName(filepath.Join(real, "other")),
		claudeProjectName("/Users/me/code/magpie-claude-7"),
	}
	for _, d := range append(append([]string{}, gone...), kept...) {
		if err := os.MkdirAll(filepath.Join(projects, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	sweepBridgeProjects(claudeDir, filepath.Join(link, "T"))
	for _, d := range gone {
		if _, err := os.Stat(filepath.Join(projects, d)); !os.IsNotExist(err) {
			t.Fatalf("%s should be swept", d)
		}
	}
	for _, d := range kept {
		if _, err := os.Stat(filepath.Join(projects, d)); err != nil {
			t.Fatalf("%s should be kept: %v", d, err)
		}
	}
	sweepBridgeProjects(filepath.Join(base, "missing"), real) // no projects folder: nothing to do
}

// Out of quota, Claude Code ends the turn with an error result and no
// message_stop, then waits on its next input: the reply is a 429 at once,
// streamed or not, so another account can take over (#177).
func TestClaudeQuotaResultEndsTheReply(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a shell script stands in for Claude Code")
	}
	dir := t.TempDir()
	script := `#!/bin/sh
while read -r line; do
  echo '{"type":"result","subtype":"success","is_error":true,"result":"You'"'"'ve hit your limit · resets 3am"}'
done
`
	os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	s := New()
	p := provider.Provider{ID: "claude", Account: &provider.Account{Agent: "claude", User: "u"}}
	for _, stream := range []string{"true", "false"} {
		body := `{"model":"claude-sonnet-5","max_tokens":100,"stream":` + stream + `,"messages":[{"role":"user","content":"ping"}]}`
		done := make(chan int, 1)
		go func() {
			var u Usage
			code, _ := s.serveClaudeSubscription(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)), provider.Anthropic, p, "claude-sonnet-5", []byte(body), &u)
			done <- code
		}()
		select {
		case code := <-done:
			if code != 429 {
				t.Fatalf("stream=%s: status %d", stream, code)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("stream=%s: the reply waited on the CLI", stream)
		}
	}
}

// A conversation's next turn at another effort — the router picked it
// (#502) — goes on in the same Claude Code, told the level by the control
// request its SDK's applyFlagSettings sends, before the turn: a Claude Code
// started anew is told the whole conversation in one message, and wrote all
// of it to the cache again (430k tokens a switch between high and low).
func TestClaudeRunKeptAcrossEffort(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a shell script stands in for Claude Code")
	}
	dir := t.TempDir()
	stdin := filepath.Join(dir, "stdin.log")
	script := `#!/bin/sh
echo "args $*" >> ` + stdin + `
n=0
while read -r line; do
  printf '%s\n' "$line" >> ` + stdin + `
  case "$line" in *control_request*)
    echo '{"type":"control_response","response":{"subtype":"success","request_id":"x"}}'
    continue;;
  esac
  n=$((n+1))
  echo '{"type":"stream_event","event":{"type":"message_start","message":{"id":"m","model":"claude-opus-5-5","usage":{"input_tokens":1}}}}'
  echo '{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"pid '$$' turn '$n'"}}}'
  echo '{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}}'
  echo '{"type":"stream_event","event":{"type":"message_stop"}}'
  echo '{"type":"result","subtype":"success","is_error":false,"result":""}'
done
`
	os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	s := New()
	p := provider.Provider{ID: "claude", Account: &provider.Account{Agent: "claude", User: "u"}}
	ask := func(effort, msgs string) string {
		t.Helper()
		body := `{"model":"claude-opus-5-5","max_tokens":32000,"thinking":{"type":"adaptive"},"output_config":{"effort":"` + effort + `"},"system":"be brief","tools":[{"name":"read","input_schema":{"type":"object"}}],"messages":` + msgs + `}`
		rec := httptest.NewRecorder()
		var u Usage
		if code, msg := s.serveClaudeSubscription(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)), provider.Anthropic, p, "claude-opus-5-5", []byte(body), &u); code != 200 {
			t.Fatalf("%d %s", code, msg)
		}
		var res struct {
			Content []struct{ Text string } `json:"content"`
		}
		json.Unmarshal(rec.Body.Bytes(), &res)
		if len(res.Content) == 0 {
			t.Fatalf("no answer: %s", rec.Body)
		}
		return res.Content[0].Text
	}
	msg := func(role, text string) string { return `{"role":"` + role + `","content":` + strconv.Quote(text) + `}` }

	first := ask("high", `[`+msg("user", "hi")+`]`)
	pid, _, _ := strings.Cut(strings.TrimPrefix(first, "pid "), " ")
	conv := msg("user", "hi") + `,` + msg("assistant", first) + `,` + msg("user", "and?")
	second := ask("low", `[`+conv+`]`)
	if second != "pid "+pid+" turn 2" {
		t.Fatalf("at another effort: %q, first %q", second, first)
	}
	third := ask("low", `[`+conv+`,`+msg("assistant", second)+`,`+msg("user", "so?")+`]`)
	if third != "pid "+pid+" turn 3" {
		t.Fatalf("third: %q", third)
	}
	b, _ := os.ReadFile(stdin)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 5 || !strings.Contains(lines[0], "--effort high") {
		t.Fatalf("Claude Code was told:\n%s", b)
	}
	var ctl struct {
		Type    string
		Request struct {
			Subtype  string
			Settings map[string]any
		}
	}
	if json.Unmarshal([]byte(lines[2]), &ctl) != nil || ctl.Type != "control_request" || ctl.Request.Subtype != "apply_flag_settings" || ctl.Request.Settings["effortLevel"] != "low" {
		t.Fatalf("effort not set before the second turn: %s", lines[2])
	}
	if !strings.Contains(lines[3], `"type":"user"`) || !strings.Contains(lines[4], `"type":"user"`) {
		t.Fatalf("turns: %s", b)
	}
}
