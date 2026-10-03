package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
)

func TestInteractiveSubscriptionPromptImagesUsePrivateMCPTool(t *testing.T) {
	const png = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
	req := &Request{Messages: []Message{{Role: "user", Parts: []Part{
		{Kind: Text, Text: "what is this?"},
		{Kind: Image, MediaType: "image/png", Data: png},
	}}}}
	prompt, images, err := renderClaudeBridgePromptWithImages(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 1 {
		t.Fatalf("images: %#v", images)
	}
	var id string
	var image Part
	for id, image = range images {
	}
	if image.Data != png || image.MediaType != "image/png" {
		t.Fatalf("image: %#v", image)
	}
	if !strings.Contains(prompt, "what is this?") || !strings.Contains(prompt, interactiveImageTool) || !strings.Contains(prompt, id) {
		t.Fatalf("prompt did not point Claude at the private image tool: %q", prompt)
	}

	tools := claudeInteractiveTools(req, len(images) > 0)
	if len(tools) != 1 || tools[0].Name != interactiveImageTool {
		t.Fatalf("private image tool missing: %#v", tools)
	}
}

func TestInteractiveSubscriptionPrivateImageToolReturnsMCPImage(t *testing.T) {
	const png = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
	b := newSubscriptionBridge()
	im := Part{Kind: Image, MediaType: "image/png", Data: png}
	id := interactiveImageID(im)
	run := &subscriptionRun{bridge: b, token: "tok", interactive: true, pending: map[string]chan mcpToolResult{}, images: map[string]Part{id: im}}
	b.runs["tok"] = run
	mux := http.NewServeMux()
	mux.HandleFunc("POST /cb/{token}", b.mcpCall)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	res, err := http.Post(srv.URL+"/cb/tok", "application/json", strings.NewReader(`{"tool_call_id":"img_1","name":"`+interactiveImageTool+`","arguments":{"id":"`+id+`"}}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("status = %d", res.StatusCode)
	}
	var got mcpToolResult
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Content) < 2 || got.Content[1]["type"] != "image" || got.Content[1]["data"] != png || got.Content[1]["mimeType"] != "image/png" {
		b, _ := json.Marshal(got)
		t.Fatalf("image result: %s", b)
	}
}

func TestInteractiveSubscriptionPrivateImageToolIsHiddenFromCaller(t *testing.T) {
	run := &subscriptionRun{interactive: true}
	seg := run.attach()
	lines := []string{
		`{"type":"assistant","uuid":"img-row","session_id":"sess-1","message":{"id":"m1","model":"claude-opus-5-5","content":[{"type":"tool_use","id":"toolu_img","name":"mcp__magpie__` + interactiveImageTool + `","input":{"id":"img_1"}}],"stop_reason":"tool_use","usage":{"input_tokens":2,"output_tokens":2}}}`,
		`{"type":"assistant","uuid":"text-row","session_id":"sess-1","message":{"id":"m2","model":"claude-opus-5-5","content":[{"type":"text","text":"seen"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}}`,
		`{"type":"result","subtype":"success","is_error":false,"session_id":"sess-1","stop_reason":"end_turn","result":"seen"}`,
	}
	go run.readOutput(strings.NewReader(strings.Join(lines, "\n") + "\n"))
	var got []string
	for ev := range seg {
		switch ev.Kind {
		case KToolStart:
			got = append(got, "tool:"+ev.Name)
		case KText:
			got = append(got, "text:"+ev.Text)
		}
	}
	if s := strings.Join(got, "|"); s != "text:seen" {
		t.Fatalf("private image tool leaked to caller: %s", s)
	}
}

func TestHeadlessSubscriptionDoesNotReserveInteractiveImageToolName(t *testing.T) {
	run := &subscriptionRun{}
	seg := run.attach()
	lines := []string{
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"m1","model":"claude-opus-5-5","usage":{"input_tokens":1}}}}`,
		`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"mcp__magpie__` + interactiveImageTool + `"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}}`,
		`{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":1}}}`,
		`{"type":"stream_event","event":{"type":"message_stop"}}`,
	}
	go run.readOutput(strings.NewReader(strings.Join(lines, "\n") + "\n"))
	var got []string
	for ev := range seg {
		switch ev.Kind {
		case KToolStart:
			got = append(got, "tool:"+ev.Name)
		case KToolArgs:
			got = append(got, "args:"+ev.Text)
		case KStop:
			got = append(got, "stop:"+ev.Stop)
		}
	}
	if s := strings.Join(got, "|"); s != "tool:"+interactiveImageTool+"|args:{}|stop:tool" {
		t.Fatalf("headless caller tool was treated as private: %s", s)
	}
}

func TestClaudeInteractivePromptRegistersInlineImageAsInternalMCPAttachment(t *testing.T) {
	const png = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
	req := &Request{Messages: []Message{{Role: "user", Parts: []Part{
		{Kind: Text, Text: "what is in this image?"},
		{Kind: Image, MediaType: "image/png", Data: png},
	}}}}

	prompt, images, err := renderClaudeBridgePromptWithImages(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 1 {
		t.Fatalf("images = %d, want 1", len(images))
	}
	var id string
	for k, im := range images {
		id = k
		if im.Data != png || im.MediaType != "image/png" {
			t.Fatalf("registered image = %+v", im)
		}
	}
	if id == "" || !strings.Contains(prompt, id) || !strings.Contains(prompt, interactiveImageTool) {
		t.Fatalf("prompt does not tell Claude how to view attachment: %q", prompt)
	}
	tools := claudeInteractiveTools(req, len(images) > 0)
	found := false
	for _, tool := range tools {
		if tool.Name == interactiveImageTool {
			found = true
		}
	}
	if !found {
		t.Fatalf("interactive image tool missing from %+v", tools)
	}
}

func TestClaudeInteractiveImageToolReturnsImageInsideMCP(t *testing.T) {
	const png = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
	b := newSubscriptionBridge()
	run := &subscriptionRun{
		bridge: b, token: "tok", interactive: true,
		pending: map[string]chan mcpToolResult{},
		images:  map[string]Part{"img_test": {Kind: Image, MediaType: "image/png", Data: png}},
	}
	b.runs["tok"] = run
	mux := http.NewServeMux()
	mux.HandleFunc("POST /cb/{token}", b.mcpCall)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	res, err := http.Post(srv.URL+"/cb/tok", "application/json", strings.NewReader(`{"tool_call_id":"toolu_internal","name":"`+interactiveImageTool+`","arguments":{"id":"img_test"}}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var got mcpToolResult
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 200 || got.IsError {
		t.Fatalf("status=%d result=%+v", res.StatusCode, got)
	}
	var image map[string]any
	for _, part := range got.Content {
		if part["type"] == "image" {
			image = part
		}
	}
	if image == nil || image["data"] != png || image["mimeType"] != "image/png" {
		t.Fatalf("internal image result = %+v", got.Content)
	}
	b.mu.Lock()
	_, leaked := b.calls["toolu_internal"]
	b.mu.Unlock()
	if leaked {
		t.Fatal("internal image tool call leaked into caller-visible pending calls")
	}
}

func TestClaudeInteractiveParserHidesInternalImageToolCall(t *testing.T) {
	run := &subscriptionRun{interactive: true}
	seg := run.attach()
	lines := []string{
		`{"type":"assistant","uuid":"row-tool","session_id":"sess-1","message":{"id":"m1","model":"claude-opus-5-5","content":[{"type":"tool_use","id":"toolu_internal","name":"mcp__magpie__` + interactiveImageTool + `","input":{"id":"img_test"}}],"stop_reason":"tool_use","usage":{"input_tokens":2,"output_tokens":5}}}`,
		`{"type":"assistant","uuid":"row-text","session_id":"sess-1","message":{"id":"m2","model":"claude-opus-5-5","content":[{"type":"text","text":"I can see it"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":4}}}`,
		`{"type":"result","subtype":"success","is_error":false,"session_id":"sess-1","stop_reason":"end_turn","result":"I can see it"}`,
	}
	go run.readOutput(strings.NewReader(strings.Join(lines, "\n") + "\n"))
	var tools int
	var text string
	var stop string
	for ev := range seg {
		switch ev.Kind {
		case KToolStart:
			tools++
		case KText:
			text += ev.Text
		case KStop:
			stop = ev.Stop
		}
	}
	if tools != 0 || text != "I can see it" || stop != "stop" {
		t.Fatalf("tools=%d text=%q stop=%q", tools, text, stop)
	}
}

// An image a tool returned reaches the Claude Code a subscription runs as
// MCP image content, which it hands its model as an image block: in an
// Anthropic tool_result, and beside a Chat tool message, where a client
// sends it in a user message after (smart-lty on Discord: Read on a PNG, a
// browser screenshot, reached the model as text only).
func TestSubscriptionToolResultImages(t *testing.T) {
	// a 1x1 PNG
	const png = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
	cases := []struct {
		name string
		from provider.Protocol
		body string
		mime string
	}{
		{"anthropic tool_result", provider.Anthropic, `{"model":"m","max_tokens":10,"messages":[
			{"role":"user","content":"look at it"},
			{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"Read","input":{"file_path":"a.png"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":[{"type":"text","text":"read a.png"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + png + `"}}]}]}]}`, "image/png"},
		{"chat image after the tool message", provider.Chat, `{"model":"m","messages":[
			{"role":"user","content":"look at it"},
			{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"read","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"read a.png"},
			{"role":"user","content":[{"type":"text","text":"Attached image(s) from tool result:"},{"type":"image_url","image_url":{"url":"data:image/png;base64,` + png + `"}}]}]}`, "image/png"},
		{"type read from the bytes", provider.Anthropic, `{"model":"m","max_tokens":10,"messages":[
			{"role":"user","content":"look at it"},
			{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"Read","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":[{"type":"text","text":"read a.png"},{"type":"image","source":{"type":"base64","data":"` + png + `"}}]}]}]}`, "image/png"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := newSubscriptionBridge()
			run := &subscriptionRun{bridge: b, token: "tok", pending: map[string]chan mcpToolResult{}}
			b.runs["tok"] = run
			mux := http.NewServeMux()
			mux.HandleFunc("POST /cb/{token}", b.mcpCall)
			srv := httptest.NewServer(mux)
			defer srv.Close()

			// Claude Code's tools/call, as the MCP helper hands it over
			answer := make(chan mcpToolResult, 1)
			go func() {
				res, err := http.Post(srv.URL+"/cb/tok", "application/json", strings.NewReader(`{"tool_call_id":"call_1","name":"Read","arguments":{}}`))
				if err != nil {
					answer <- mcpToolResult{}
					return
				}
				defer res.Body.Close()
				var r mcpToolResult
				_ = json.NewDecoder(res.Body).Decode(&r)
				answer <- r
			}()
			for deadline := time.Now().Add(2 * time.Second); ; {
				b.mu.Lock()
				n := len(b.calls)
				b.mu.Unlock()
				if n == 1 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("call_1 was not registered")
				}
				time.Sleep(5 * time.Millisecond)
			}

			req, err := parse(c.from, []byte(c.body))
			if err != nil {
				t.Fatal(err)
			}
			found, results := b.findRun(req)
			if found != run {
				t.Fatal("no run for call_1")
			}
			if _, err := run.continueWith(results, nil); err != nil {
				t.Fatal(err)
			}
			var got mcpToolResult
			select {
			case got = <-answer:
			case <-time.After(2 * time.Second):
				run.finish()
				t.Fatal("call_1 never had its result")
			}
			var image map[string]any
			for _, part := range got.Content {
				if part["type"] == "image" {
					image = part
				}
			}
			if image == nil || image["data"] != png || image["mimeType"] != c.mime {
				out, _ := json.Marshal(got)
				t.Fatalf("the tool's image did not reach the agent: %s", out)
			}
			if got.Content[0]["type"] != "text" || !strings.Contains(got.Content[0]["text"].(string), "read a.png") {
				t.Fatalf("the tool's text: %v", got.Content[0])
			}

			// a run started anew is told the whole conversation, the image too
			prompt, _ := renderClaudePrompt(req)
			seen := false
			for _, block := range prompt {
				if src, _ := block["source"].(map[string]any); block["type"] == "image" && src["data"] == png {
					seen = true
				}
			}
			if !seen {
				out, _ := json.Marshal(prompt)
				t.Fatalf("a new run's prompt has no image: %s", out)
			}
		})
	}
}
