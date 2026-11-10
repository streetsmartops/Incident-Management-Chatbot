package anthropic_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/streetsmartops/incident-management-chatbot/core"
	"github.com/streetsmartops/incident-management-chatbot/providers/anthropic"
)

func TestToolLoopRequestShapeAndThinkingReplay(t *testing.T) {
	var reqs []map[string]any
	var headers []http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/messages") {
			t.Errorf("path %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		reqs = append(reqs, m)
		headers = append(headers, r.Header.Clone())
		w.Header().Set("Content-Type", "application/json")
		if len(reqs) == 1 {
			_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[{"type":"thinking","thinking":"","signature":"SIG123"},{"type":"tool_use","id":"toolu_01","name":"query_metric","input":{"query":"sum:errors{service:checkout}"}}],"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":100,"output_tokens":20,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"msg_2","type":"message","role":"assistant","model":"claude-opus-5-5","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":150,"output_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`))
	}))
	defer srv.Close()

	c := anthropic.New(anthropic.Config{APIKey: "test-key", Effort: "high", Fallbacks: true, BaseURL: srv.URL, HTTP: srv.Client()})
	tools := []core.LlmTool{{Name: "query_metric", Description: "Query a metric", InputSchema: map[string]any{
		"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}, "required": []string{"query"},
	}}}
	conv := c.NewConversation("You are a triage assistant.", tools, 4000)
	turn, err := conv.Send(context.Background(), "Investigate INC-1")
	if err != nil {
		t.Fatal(err)
	}
	if turn.StopReason != core.StopToolUse || len(turn.ToolCalls) != 1 || turn.ToolCalls[0].ID != "toolu_01" {
		t.Fatalf("turn: %+v", turn)
	}
	var in map[string]string
	if err := json.Unmarshal(turn.ToolCalls[0].Input, &in); err != nil || in["query"] == "" {
		t.Fatalf("tool input: %s", turn.ToolCalls[0].Input)
	}
	turn, err = conv.SendToolResults(context.Background(), []core.LlmToolResult{{CallID: "toolu_01", Content: "42 errors/min"}})
	if err != nil || turn.StopReason != core.StopEndTurn || turn.Text != "done" {
		t.Fatalf("turn2: %+v %v", turn, err)
	}

	first := reqs[0]
	if first["model"] != "claude-opus-5-5" || first["fallbacks"] != "default" {
		t.Fatalf("model/fallbacks: %v %v", first["model"], first["fallbacks"])
	}
	if first["output_config"].(map[string]any)["effort"] != "high" {
		t.Fatal("effort not sent")
	}
	if _, ok := first["thinking"]; ok {
		t.Fatal("thinking must be omitted on claude-opus-5-5 (adaptive by default)")
	}
	sys := first["system"].([]any)[0].(map[string]any)
	if sys["cache_control"] == nil {
		t.Fatal("system prompt should be cache-controlled")
	}
	tool := first["tools"].([]any)[0].(map[string]any)
	if tool["name"] != "query_metric" || tool["input_schema"].(map[string]any)["additionalProperties"] != false {
		t.Fatalf("tool: %+v", tool)
	}
	if h := headers[0].Get("anthropic-beta"); !strings.Contains(h, "server-side-fallback-2026-07-01") {
		t.Fatalf("beta header: %q", h)
	}
	// The SDK chooses the auth header; accept either form.
	if headers[0].Get("x-api-key") != "test-key" && headers[0].Get("Authorization") != "Bearer test-key" {
		t.Fatal("api key not sent")
	}
	// Second request replays the assistant turn including the thinking block signature.
	msgs := reqs[1]["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("want user, assistant, user; got %d", len(msgs))
	}
	asst, _ := json.Marshal(msgs[1])
	if !strings.Contains(string(asst), "SIG123") {
		t.Fatalf("thinking block not replayed: %s", asst)
	}
	res, _ := json.Marshal(msgs[2])
	if !strings.Contains(string(res), `"tool_use_id":"toolu_01"`) {
		t.Fatalf("tool result: %s", res)
	}
}

func TestFallbacksOff(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"m","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":"refusal","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()
	c := anthropic.New(anthropic.Config{APIKey: "k", BaseURL: srv.URL, HTTP: srv.Client()})
	turn, err := c.NewConversation("s", nil, 100).Send(context.Background(), "x")
	if err != nil || turn.StopReason != core.StopRefusal {
		t.Fatalf("%+v %v", turn, err)
	}
	if _, ok := body["fallbacks"]; ok {
		t.Fatal("fallbacks must be omitted when disabled")
	}
}
