package server

import (
	"bytes"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gurghet/rmfakecloud-share/internal/auth"
	"github.com/gurghet/rmfakecloud-share/internal/store"
)

func tokenFor(t *testing.T, sub string, exp time.Time) string {
	t.Helper()
	k, err := pbkdf2.Key(sha256.New, secret, []byte("todo some salt"), 10000, 32)
	if err != nil {
		t.Fatal(err)
	}
	e := base64.RawURLEncoding
	h, _ := json.Marshal(map[string]any{"alg": "HS256", "kid": "1", "typ": "JWT"})
	c, _ := json.Marshal(map[string]any{"sub": sub, "scopes": "share-url", "exp": exp.Unix(), "nbf": time.Now().Add(-time.Minute).Unix()})
	signing := e.EncodeToString(h) + "." + e.EncodeToString(c)
	m := hmac.New(sha256.New, k)
	m.Write([]byte(signing))
	return signing + "." + e.EncodeToString(m.Sum(nil))
}

// ---- 1: slow upload still gets its response ----

func TestSlowUploadStillGetsItsResponse(t *testing.T) {
	h := newHarness(t, nil)
	ts := httptest.NewUnstartedServer(nil)
	// Write timeout shorter than the upload, like the production misconfiguration.
	ts.Config = NewHTTPServer("", h.h, Timeouts{ReadHeader: time.Second, Read: 5 * time.Second, Write: 400 * time.Millisecond, Idle: time.Second})
	ts.Start()
	defer ts.Close()

	body := twoPageBody(t)
	pr, pw := io.Pipe()
	req, _ := http.NewRequest("POST", ts.URL+"/share/v1/link", pr)
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Type", wireContentType())
	req.Header.Set("Authorization", "Bearer "+goodToken(t))
	go func() {
		_, _ = pw.Write(body[:100])
		time.Sleep(900 * time.Millisecond) // longer than the write timeout
		_, _ = pw.Write(body[100:])
		_ = pw.Close()
	}()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("client got no response although the server stored the share: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), `"link"`) {
		t.Fatalf("body %q", b)
	}
}

func TestDefaultTimeoutsWriteOutlastsRead(t *testing.T) {
	d := DefaultTimeouts()
	if d.Write <= d.Read {
		t.Fatalf("write timeout %v must exceed read timeout %v, otherwise slow uploads get no response", d.Write, d.Read)
	}
}

// ---- 3: limits ----

func TestOversizedPageIs400(t *testing.T) {
	h := newHarness(t, func(d *Deps) { d.MaxPageBytes = 200 })
	big := append(tinyPNG(t, 1), bytes.Repeat([]byte{7}, 500)...)
	w := h.post(t, goodToken(t), wireBody(t, "d", []string{"a"}, [][]byte{big}))
	if w.Code != 400 {
		t.Fatalf("status %d, want 400 for a page over the per-image limit", w.Code)
	}
	if len(h.rec.ids) != 0 {
		t.Fatal("must not enqueue")
	}
	if ok := h.post(t, goodToken(t), wireBody(t, "d", []string{"a"}, [][]byte{tinyPNG(t, 1)})); ok.Code != 200 {
		t.Fatalf("small page rejected: %d", ok.Code)
	}
}

func TestConcurrentUploadsAreLimited(t *testing.T) {
	h := newHarness(t, func(d *Deps) { d.MaxConcurrentUploads = 1; d.UploadWait = 150 * time.Millisecond })
	ts := httptest.NewServer(h.h)
	defer ts.Close()
	body := twoPageBody(t)

	pr, pw := io.Pipe()
	defer pw.Close() // never leave the stalled upload hanging if an assertion fails
	reqA, _ := http.NewRequest("POST", ts.URL+"/share/v1/link", pr)
	reqA.ContentLength = int64(len(body))
	reqA.Header.Set("Content-Type", wireContentType())
	reqA.Header.Set("Authorization", "Bearer "+goodToken(t))
	resA := make(chan int, 1)
	go func() {
		resp, err := http.DefaultClient.Do(reqA)
		if err != nil {
			resA <- -1
			return
		}
		resp.Body.Close()
		resA <- resp.StatusCode
	}()
	_, _ = pw.Write(body[:50])
	time.Sleep(100 * time.Millisecond) // A now holds the only slot

	reqB, _ := http.NewRequest("POST", ts.URL+"/share/v1/link", bytes.NewReader(body))
	reqB.Header.Set("Content-Type", wireContentType())
	reqB.Header.Set("Authorization", "Bearer "+goodToken(t))
	respB, err := http.DefaultClient.Do(reqB)
	if err != nil {
		t.Fatal(err)
	}
	respB.Body.Close()
	if respB.StatusCode != 503 || respB.Header.Get("Retry-After") == "" {
		t.Fatalf("second concurrent upload: status %d retry-after %q, want 503 with Retry-After", respB.StatusCode, respB.Header.Get("Retry-After"))
	}
	_, _ = pw.Write(body[50:])
	_ = pw.Close()
	if code := <-resA; code != 200 {
		t.Fatalf("first upload status %d", code)
	}
	// Slot is free again.
	if w := h.post(t, goodToken(t), body); w.Code != 200 {
		t.Fatalf("upload after release = %d", w.Code)
	}
}

// ---- 8: quota ----

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func TestDailyPageQuotaPerUserRolling(t *testing.T) {
	clk := &clock{t: time.Now()}
	h := newHarness(t, func(d *Deps) { d.MaxPagesPerDay = 3; d.Now = clk.Now })
	exp := clk.Now().Add(72 * time.Hour)
	u7, u8 := tokenFor(t, "user-7", exp), tokenFor(t, "user-8", exp)
	body := twoPageBody(t) // 2 pages

	if w := h.post(t, u7, body); w.Code != 200 {
		t.Fatalf("first share = %d", w.Code)
	}
	w := h.post(t, u7, body)
	if w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatalf("second share = %d (retry-after %q), want 429", w.Code, w.Header().Get("Retry-After"))
	}
	if all, _ := h.st.List(); len(all) != 1 {
		t.Fatalf("over-quota upload must not be stored, have %d shares", len(all))
	}
	if w := h.post(t, u8, body); w.Code != 200 {
		t.Fatalf("other user must have their own quota, got %d", w.Code)
	}
	clk.Add(25 * time.Hour)
	if w := h.post(t, u7, body); w.Code != 200 {
		t.Fatalf("quota must roll over after 24h, got %d", w.Code)
	}
}

func TestQuotaSurvivesRestart(t *testing.T) {
	clk := &clock{t: time.Now()}
	h := newHarness(t, func(d *Deps) { d.MaxPagesPerDay = 3; d.Now = clk.Now })
	tok := tokenFor(t, "user-7", clk.Now().Add(time.Hour))
	if w := h.post(t, tok, twoPageBody(t)); w.Code != 200 {
		t.Fatalf("setup share = %d", w.Code)
	}
	v, _ := auth.NewVerifier(secret, auth.KeyModeRmfakecloud)
	restarted := New(Deps{Store: h.st, Verifier: v, PublicURL: publicURL, MaxBody: 1 << 20, MaxPages: 5,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: clk.Now, MaxPagesPerDay: 3})
	h2 := &harness{h: restarted, st: h.st, rec: &recorder{}}
	if w := h2.post(t, tok, twoPageBody(t)); w.Code != 429 {
		t.Fatalf("after restart the stored shares must still count, got %d", w.Code)
	}
}

// ---- 6: logs ----

func TestInfoLogsCarryNoPersonalData(t *testing.T) {
	var buf bytes.Buffer
	h := newHarness(t, func(d *Deps) {
		d.Log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	})
	md, _ := json.Marshal(map[string]any{"DocID": "doc-secret-id-123", "MailList": []string{"alice@example.com", "bob@example.com"}, "Pages": []string{"p1"}})
	var b bytes.Buffer
	fmt.Fprintf(&b, "--%s\r\ncontent-disposition: form-data; name=\"metadata\"\r\ncontent-type: application/json\r\n\r\n%s\r\n", wireBoundary, md)
	png := tinyPNG(t, 3)
	fmt.Fprintf(&b, "--%s\r\ncontent-length: %d\r\ncontent-disposition: form-data; name=\"page\"; filename=\"p1.png\"\r\ncontent-type: image/png\r\n\r\n", wireBoundary, len(png))
	b.Write(png)
	fmt.Fprintf(&b, "\r\n--%s--\r\n", wireBoundary)
	if w := h.post(t, goodToken(t), b.Bytes()); w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	out := buf.String()
	if !strings.Contains(out, "share created") {
		t.Fatalf("expected a share created line:\n%s", out)
	}
	for _, leak := range []string{"alice@example.com", "bob@example.com", "doc-secret-id-123", "user-7"} {
		if strings.Contains(out, leak) {
			t.Errorf("INFO log leaks %q:\n%s", leak, out)
		}
	}
	if !strings.Contains(out, "mail_list_count=2") {
		t.Errorf("log should carry the MailList count only:\n%s", out)
	}
	_ = store.ValidID
}

// ---- 11: link attributes ----

func TestRenderedLinksAreNofollowNoopener(t *testing.T) {
	html, err := renderMarkdown("[a](https://example.org/a) and https://example.org/b and <https://example.org/c>\n")
	if err != nil {
		t.Fatal(err)
	}
	out := string(html)
	anchors := strings.Count(out, "<a ")
	if anchors < 3 {
		t.Fatalf("expected 3 anchors, got %d in %s", anchors, out)
	}
	if got := strings.Count(out, `rel="nofollow noopener noreferrer"`); got != anchors {
		t.Fatalf("%d of %d anchors carry the rel attribute in %s", got, anchors, out)
	}
}
