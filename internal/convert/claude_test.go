package convert

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type captured struct {
	apiKey string
	path   string
	body   map[string]any
}

func mockAPI(t *testing.T, status int, respBody string, cap *captured) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		cap.apiKey = r.Header.Get("x-api-key")
		cap.path = r.URL.Path
		_ = json.Unmarshal(b, &cap.body)
		if status == 200 {
			w.Header().Set("content-type", "text/event-stream")
		} else {
			w.Header().Set("content-type", "application/json")
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, respBody)
	}))
}

// okResponse builds a streaming (SSE) Messages API response, which is what the
// converter now requests.
func okResponse(text, stop string) string {
	ev := func(name string, v any) string {
		b, _ := json.Marshal(v)
		return "event: " + name + "\ndata: " + string(b) + "\n\n"
	}
	var sb strings.Builder
	sb.WriteString(ev("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": "msg_test", "type": "message", "role": "assistant", "model": "claude-opus-5-5", "content": []any{},
		"stop_reason": nil, "stop_sequence": nil, "usage": map[string]any{"input_tokens": 10, "output_tokens": 1}}}))
	if text != "" {
		sb.WriteString(ev("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}}))
		sb.WriteString(ev("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": text}}))
		sb.WriteString(ev("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}))
	}
	sb.WriteString(ev("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]any{"output_tokens": 5}}))
	sb.WriteString(ev("message_stop", map[string]any{"type": "message_stop"}))
	return sb.String()
}

func TestClaudeRequestShapeAndResponse(t *testing.T) {
	var cap captured
	srv := mockAPI(t, 200, okResponse("# Shopping\n\n- [ ] milk", "end_turn"), &cap)
	defer srv.Close()
	c := NewClaude(ClaudeOptions{APIKey: "test-key-123", Model: "test-model-x", BaseURL: srv.URL, MaxRetries: 0})

	pages := [][]byte{[]byte("\x89PNG-first"), []byte("\x89PNG-second"), []byte("\x89PNG-third")}
	note, err := c.Convert(context.Background(), pages)
	if err != nil {
		t.Fatal(err)
	}
	if note != "# Shopping\n\n- [ ] milk" {
		t.Fatalf("note = %q", note)
	}
	if cap.apiKey != "test-key-123" {
		t.Fatalf("x-api-key = %q", cap.apiKey)
	}
	if cap.path != "/v1/messages" {
		t.Fatalf("path = %q", cap.path)
	}
	if cap.body["model"] != "test-model-x" {
		t.Fatalf("model = %v", cap.body["model"])
	}
	if s, _ := cap.body["system"].(string); s == "" {
		if arr, ok := cap.body["system"].([]any); !ok || len(arr) == 0 {
			t.Fatalf("system prompt missing: %v", cap.body["system"])
		}
	}
	msgs := cap.body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("want one user message, got %d", len(msgs))
	}
	content := msgs[0].(map[string]any)["content"].([]any)
	var images []string
	for _, blk := range content {
		m := blk.(map[string]any)
		if m["type"] != "image" {
			continue
		}
		src := m["source"].(map[string]any)
		if src["type"] != "base64" || src["media_type"] != "image/png" {
			t.Fatalf("bad image source %v", src)
		}
		raw, _ := base64.StdEncoding.DecodeString(src["data"].(string))
		images = append(images, string(raw))
	}
	if len(images) != 3 || images[0] != string(pages[0]) || images[1] != string(pages[1]) || images[2] != string(pages[2]) {
		t.Fatalf("images not in page order: %q", images)
	}
}

func TestClaudeNon2xxIsError(t *testing.T) {
	var cap captured
	srv := mockAPI(t, 400, `{"type":"error","error":{"type":"invalid_request_error","message":"bad thing"}}`, &cap)
	defer srv.Close()
	c := NewClaude(ClaudeOptions{APIKey: "k", Model: "m", BaseURL: srv.URL, MaxRetries: 0})
	if _, err := c.Convert(context.Background(), [][]byte{[]byte("x")}); err == nil {
		t.Fatal("expected error for HTTP 400")
	} else if !strings.Contains(err.Error(), "bad thing") {
		t.Fatalf("error should carry API message: %v", err)
	}
}

func TestClaudeEmptyTextIsError(t *testing.T) {
	var cap captured
	srv := mockAPI(t, 200, okResponse("   ", "end_turn"), &cap)
	defer srv.Close()
	c := NewClaude(ClaudeOptions{APIKey: "k", Model: "m", BaseURL: srv.URL, MaxRetries: 0})
	if _, err := c.Convert(context.Background(), [][]byte{[]byte("x")}); err == nil {
		t.Fatal("empty transcription must not be reported as success")
	}
}

func TestClaudeRefusalIsError(t *testing.T) {
	var cap captured
	srv := mockAPI(t, 200, okResponse("", "refusal"), &cap)
	defer srv.Close()
	c := NewClaude(ClaudeOptions{APIKey: "k", Model: "m", BaseURL: srv.URL, MaxRetries: 0})
	if _, err := c.Convert(context.Background(), [][]byte{[]byte("x")}); err == nil {
		t.Fatal("refusal must be an error")
	}
}

func TestClaudeTruncationIsMarked(t *testing.T) {
	var cap captured
	srv := mockAPI(t, 200, okResponse("# Partial", "max_tokens"), &cap)
	defer srv.Close()
	c := NewClaude(ClaudeOptions{APIKey: "k", Model: "m", BaseURL: srv.URL, MaxRetries: 0})
	note, err := c.Convert(context.Background(), [][]byte{[]byte("x")})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(note, "# Partial") || !strings.Contains(strings.ToLower(note), "truncated") {
		t.Fatalf("truncated output must keep text and say it was truncated, got %q", note)
	}
}

func TestClaudeNoPagesIsError(t *testing.T) {
	c := NewClaude(ClaudeOptions{APIKey: "k", Model: "m", BaseURL: "http://127.0.0.1:1", MaxRetries: 0})
	if _, err := c.Convert(context.Background(), nil); err == nil {
		t.Fatal("no pages must be an error")
	}
}
