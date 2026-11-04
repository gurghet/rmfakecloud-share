package convert

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type reqInfo struct {
	body   map[string]any
	images []string // decoded image payloads in order
	texts  []string
}

type multiMock struct {
	mu       sync.Mutex
	reqs     []reqInfo
	failOn   int // 1-based request number to fail with failStatus (0 = never)
	failCode int
}

func (m *multiMock) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		ri := reqInfo{body: body}
		for _, blk := range body["messages"].([]any)[0].(map[string]any)["content"].([]any) {
			bm := blk.(map[string]any)
			switch bm["type"] {
			case "image":
				raw, _ := base64.StdEncoding.DecodeString(bm["source"].(map[string]any)["data"].(string))
				ri.images = append(ri.images, string(raw))
			case "text":
				ri.texts = append(ri.texts, bm["text"].(string))
			}
		}
		m.mu.Lock()
		m.reqs = append(m.reqs, ri)
		n := len(m.reqs)
		m.mu.Unlock()
		if m.failOn == n {
			w.Header().Set("content-type", "application/json")
			w.WriteHeader(m.failCode)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"x","message":"boom"}}`)
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, okResponse(fmt.Sprintf("NOTE-%d", n), "end_turn"))
	})
}

func pagesN(n, size int) [][]byte {
	out := make([][]byte, n)
	for i := range out {
		out[i] = []byte(fmt.Sprintf("P%03d", i+1) + strings.Repeat("x", size))
	}
	return out
}

func TestClaudeStreamsWithLargeMaxTokens(t *testing.T) {
	m := &multiMock{}
	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	c := NewClaude(ClaudeOptions{APIKey: "k", Model: "m", BaseURL: srv.URL, MaxRetries: 0})
	if _, err := c.Convert(context.Background(), pagesN(1, 0)); err != nil {
		t.Fatal(err)
	}
	body := m.reqs[0].body
	if body["stream"] != true {
		t.Fatalf("stream = %v, want true", body["stream"])
	}
	if mt, _ := body["max_tokens"].(float64); mt <= 16000 {
		t.Fatalf("max_tokens = %v, want > 16000", body["max_tokens"])
	}
}

func TestClaudeBatchesByImageCountInOrder(t *testing.T) {
	m := &multiMock{}
	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	c := NewClaude(ClaudeOptions{APIKey: "k", Model: "m", BaseURL: srv.URL, MaxRetries: 0, MaxBatchImages: 20})
	note, err := c.Convert(context.Background(), pagesN(45, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.reqs) != 3 {
		t.Fatalf("%d requests, want 3", len(m.reqs))
	}
	wantSizes := []int{20, 20, 5}
	next := 1
	for i, r := range m.reqs {
		if len(r.images) != wantSizes[i] {
			t.Fatalf("batch %d has %d images, want %d", i+1, len(r.images), wantSizes[i])
		}
		for _, img := range r.images {
			if want := fmt.Sprintf("P%03d", next); img != want {
				t.Fatalf("batch %d: got page %q, want %q", i+1, img, want)
			}
			next++
		}
	}
	if note != "NOTE-1\n\nNOTE-2\n\nNOTE-3" {
		t.Fatalf("note = %q", note)
	}
	if !strings.Contains(strings.Join(m.reqs[1].texts, "|"), "Page 21 of 45") {
		t.Fatalf("batch 2 must label pages with their global numbers, texts=%q", m.reqs[1].texts)
	}
}

func TestClaudeBatchesByBytes(t *testing.T) {
	m := &multiMock{}
	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	// every page is 4+6 = 10 bytes; 25 bytes per request fits two pages.
	c := NewClaude(ClaudeOptions{APIKey: "k", Model: "m", BaseURL: srv.URL, MaxRetries: 0, MaxBatchBytes: 25})
	if _, err := c.Convert(context.Background(), pagesN(5, 6)); err != nil {
		t.Fatal(err)
	}
	var sizes []int
	for _, r := range m.reqs {
		sizes = append(sizes, len(r.images))
	}
	if fmt.Sprint(sizes) != "[2 2 1]" {
		t.Fatalf("batch sizes %v, want [2 2 1]", sizes)
	}
}

func TestClaudeBatchFailureNamesTheBatch(t *testing.T) {
	m := &multiMock{failOn: 2, failCode: 400}
	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	c := NewClaude(ClaudeOptions{APIKey: "k", Model: "m", BaseURL: srv.URL, MaxRetries: 0, MaxBatchImages: 20})
	_, err := c.Convert(context.Background(), pagesN(45, 0))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "batch 2 of 3") || !strings.Contains(err.Error(), "pages 21-40") {
		t.Fatalf("error must say which batch failed: %v", err)
	}
	if len(m.reqs) != 2 {
		t.Fatalf("must stop after the failing batch, saw %d requests", len(m.reqs))
	}
}

func TestErrorClassification(t *testing.T) {
	cases := []struct {
		code      int
		permanent bool
	}{
		{400, true}, {401, true}, {403, true}, {404, true}, {413, true},
		{408, false}, {429, false}, {500, false}, {502, false}, {503, false}, {529, false},
	}
	for _, c := range cases {
		m := &multiMock{failOn: 1, failCode: c.code}
		srv := httptest.NewServer(m.handler())
		cl := NewClaude(ClaudeOptions{APIKey: "k", Model: "m", BaseURL: srv.URL, MaxRetries: 0})
		_, err := cl.Convert(context.Background(), pagesN(1, 0))
		srv.Close()
		if err == nil {
			t.Fatalf("%d: expected error", c.code)
		}
		if IsPermanent(err) != c.permanent {
			t.Errorf("HTTP %d: IsPermanent = %v, want %v", c.code, IsPermanent(err), c.permanent)
		}
	}
}

func TestNetworkAndTimeoutErrorsAreTransient(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // connection refused
	cl := NewClaude(ClaudeOptions{APIKey: "k", Model: "m", BaseURL: url, MaxRetries: 0})
	_, err := cl.Convert(context.Background(), pagesN(1, 0))
	if err == nil || IsPermanent(err) {
		t.Fatalf("connection refused must be a transient error, got %v (permanent=%v)", err, IsPermanent(err))
	}

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer slow.Close()
	cl = NewClaude(ClaudeOptions{APIKey: "k", Model: "m", BaseURL: slow.URL, MaxRetries: 0})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = cl.Convert(ctx, pagesN(1, 0))
	if err == nil || IsPermanent(err) {
		t.Fatalf("timeout must be transient, got %v (permanent=%v)", err, IsPermanent(err))
	}
}

func TestRefusalAndEmptyAreRecordedAsPermanent(t *testing.T) {
	var cap captured
	for name, resp := range map[string]string{"refusal": okResponse("", "refusal"), "empty": okResponse("  ", "end_turn")} {
		srv := mockAPI(t, 200, resp, &cap)
		cl := NewClaude(ClaudeOptions{APIKey: "k", Model: "m", BaseURL: srv.URL, MaxRetries: 0})
		_, err := cl.Convert(context.Background(), pagesN(1, 0))
		srv.Close()
		if err == nil || !IsPermanent(err) {
			t.Errorf("%s: err=%v permanent=%v, want permanent error", name, err, err != nil && IsPermanent(err))
		}
	}
}

func TestPermanentWrapperRoundTrips(t *testing.T) {
	base := errors.New("x")
	p := Permanent(base)
	if !IsPermanent(p) || !errors.Is(p, base) || IsPermanent(base) {
		t.Fatal("Permanent/IsPermanent contract broken")
	}
}
