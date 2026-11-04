package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gurghet/rmfakecloud-share/internal/auth"
	"github.com/gurghet/rmfakecloud-share/internal/store"
	"github.com/gurghet/rmfakecloud-share/internal/worker"
)

const (
	secret    = "synthetic-secret-for-tests"
	publicURL = "https://notes.example.com/share"
	// Mirrors the quoted boundary with dots and underscores seen from xochitl.
	wireBoundary = "boundary_.oOo._f2NyZPUjFTnDwQ9PIDSHS7MfwPG8ZGpD"
)

func tinyPNG(t *testing.T, shade uint8) []byte {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, 2, 2))
	img.SetGray(0, 0, color.Gray{shade})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func mintToken(t *testing.T, scopes string, exp time.Time, derived bool) string {
	t.Helper()
	key := []byte(secret)
	if derived {
		k, err := pbkdf2.Key(sha256.New, secret, []byte("todo some salt"), 10000, 32)
		if err != nil {
			t.Fatal(err)
		}
		key = k
	}
	e := base64.RawURLEncoding
	h, _ := json.Marshal(map[string]any{"alg": "HS256", "kid": "1", "typ": "JWT"})
	c, _ := json.Marshal(map[string]any{"sub": "user-7", "scopes": scopes, "exp": exp.Unix(), "nbf": time.Now().Add(-time.Minute).Unix(), "iss": "rM WebApp"})
	signing := e.EncodeToString(h) + "." + e.EncodeToString(c)
	m := hmac.New(sha256.New, key)
	m.Write([]byte(signing))
	return signing + "." + e.EncodeToString(m.Sum(nil))
}

func goodToken(t *testing.T) string {
	return mintToken(t, "intgr docedit sync:tortoise share-url", time.Now().Add(time.Hour), true)
}

// wireBody reproduces xochitl's body structure: metadata part first with only
// content-disposition and content-type, then one part per page carrying a
// content-length and a filename.
func wireBody(t *testing.T, docID string, pageIDs []string, pages [][]byte) []byte {
	t.Helper()
	md, _ := json.MarshalIndent(map[string]any{"DocID": docID, "MailList": []string{}, "Pages": pageIDs}, "", "    ")
	var b bytes.Buffer
	fmt.Fprintf(&b, "--%s\r\ncontent-disposition: form-data; name=\"metadata\"\r\ncontent-type: application/json\r\n\r\n", wireBoundary)
	b.Write(md)
	b.WriteString("\r\n")
	for i, p := range pages {
		fmt.Fprintf(&b, "--%s\r\ncontent-length: %d\r\ncontent-disposition: form-data; name=\"page\"; filename=\"%s.png\"\r\ncontent-type: image/png\r\n\r\n", wireBoundary, len(p), pageIDs[i])
		b.Write(p)
		b.WriteString("\r\n")
	}
	fmt.Fprintf(&b, "--%s--\r\n", wireBoundary)
	return b.Bytes()
}

func wireContentType() string {
	return fmt.Sprintf("multipart/form-data; boundary=%q", wireBoundary)
}

// ---- (c) wire format parsing ----

func TestParseUploadXochitlWireFormat(t *testing.T) {
	p1, p2, p3 := tinyPNG(t, 10), tinyPNG(t, 120), tinyPNG(t, 250)
	ids := []string{"9a9e0001-0000-4000-8000-000000000001", "11111111-2222-4333-8444-555555555555", "99999999-aaaa-4bbb-8ccc-dddddddddddd"}
	body := wireBody(t, "d0c1d0c1-0000-4000-8000-000000000001", ids, [][]byte{p1, p2, p3})

	// Sanity: the fixture really has the quoted boundary and lowercase headers.
	if !strings.Contains(wireContentType(), `boundary="boundary_.oOo._`) || !bytes.Contains(body, []byte("content-disposition: form-data; name=\"metadata\"")) {
		t.Fatal("fixture does not reproduce the wire format")
	}

	u, err := parseUpload(wireContentType(), bytes.NewReader(body), 50)
	if err != nil {
		t.Fatalf("parseUpload: %v", err)
	}
	if u.DocID != "d0c1d0c1-0000-4000-8000-000000000001" {
		t.Errorf("DocID = %q", u.DocID)
	}
	if len(u.Pages) != 3 || !bytes.Equal(u.Pages[0], p1) || !bytes.Equal(u.Pages[1], p2) || !bytes.Equal(u.Pages[2], p3) {
		t.Fatalf("pages not parsed in order: %d pages", len(u.Pages))
	}
	if len(u.PageIDs) != 3 || u.PageIDs[2] != ids[2] {
		t.Errorf("PageIDs = %v", u.PageIDs)
	}
}

func TestParseUploadRejectsBadInput(t *testing.T) {
	png1 := tinyPNG(t, 1)
	ok := func() []byte { return wireBody(t, "d", []string{"a"}, [][]byte{png1}) }
	cases := map[string]struct {
		ct   string
		body []byte
	}{
		"not multipart":    {"application/json", []byte("{}")},
		"non-png page":     {wireContentType(), wireBody(t, "d", []string{"a"}, [][]byte{[]byte("GIF89a....")})},
		"no pages":         {wireContentType(), wireBody(t, "d", nil, nil)},
		"missing metadata": {wireContentType(), bytes.Replace(ok(), []byte(`name="metadata"`), []byte(`name="other"`), 1)},
		"bad metadata":     {wireContentType(), bytes.Replace(ok(), []byte(`"DocID"`), []byte(`"Doc"`), 1)},
		"truncated":        {wireContentType(), ok()[:len(ok())/2]},
	}
	for name, c := range cases {
		if _, err := parseUpload(c.ct, bytes.NewReader(c.body), 50); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestParseUploadTooManyPages(t *testing.T) {
	var pages [][]byte
	var ids []string
	for i := 0; i < 4; i++ {
		pages = append(pages, tinyPNG(t, uint8(i)))
		ids = append(ids, fmt.Sprintf("p%d", i))
	}
	_, err := parseUpload(wireContentType(), bytes.NewReader(wireBody(t, "d", ids, pages)), 3)
	var ue *uploadError
	if !errors.As(err, &ue) || ue.status != http.StatusBadRequest {
		t.Fatalf("err = %v, want 400 uploadError", err)
	}
}

// ---- harness ----

type recorder struct{ ids []string }

func (r *recorder) Enqueue(id string) { r.ids = append(r.ids, id) }

type harness struct {
	h   http.Handler
	st  *store.Store
	rec *recorder
}

func newHarness(t *testing.T, mut func(*Deps)) *harness {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	v, err := auth.NewVerifier(secret, auth.KeyModeRmfakecloud)
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	d := Deps{Store: st, Verifier: v, Enqueuer: rec, PublicURL: publicURL, MaxBody: 1 << 20, MaxPages: 5,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: time.Now}
	if mut != nil {
		mut(&d)
	}
	return &harness{h: New(d), st: st, rec: rec}
}

func (hh *harness) do(method, path string, hdr map[string]string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	hh.h.ServeHTTP(w, req)
	return w
}

func (hh *harness) post(t *testing.T, token string, body []byte) *httptest.ResponseRecorder {
	hdr := map[string]string{"Content-Type": wireContentType(), "User-Agent": "xochitl/3.29.1.0 (codex 6.0.112)"}
	if token != "" {
		hdr["Authorization"] = "Bearer " + token
	}
	return hh.do("POST", "/share/v1/link", hdr, body)
}

func twoPageBody(t *testing.T) []byte {
	return wireBody(t, "d0c1d0c1-0000-4000-8000-000000000001", []string{"p-one", "p-two"}, [][]byte{tinyPNG(t, 5), tinyPNG(t, 200)})
}

func linkFrom(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var r struct {
		Link string `json:"link"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatalf("bad JSON %q: %v", w.Body.String(), err)
	}
	return r.Link
}

// ---- (d) happy path ----

func TestPostHappyPathAndPageIgnoresQueryString(t *testing.T) {
	h := newHarness(t, nil)
	w := h.post(t, goodToken(t), twoPageBody(t))
	if w.Code != 200 {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q", ct)
	}
	link := linkFrom(t, w)
	if !strings.HasPrefix(link, publicURL+"/") || !strings.HasSuffix(link, "/") {
		t.Fatalf("link = %q", link)
	}
	id := strings.TrimSuffix(strings.TrimPrefix(link, publicURL+"/"), "/")
	if !store.ValidID(id) {
		t.Fatalf("link id %q invalid", id)
	}
	if len(h.rec.ids) != 1 || h.rec.ids[0] != id {
		t.Fatalf("enqueued %v, want [%s]", h.rec.ids, id)
	}
	m, err := h.st.Get(id)
	if err != nil || m.User != "user-7" || m.DocID != "d0c1d0c1-0000-4000-8000-000000000001" || m.PageCount != 2 {
		t.Fatalf("stored meta %+v, %v", m, err)
	}
	for _, path := range []string{"/share/" + id + "/", "/share/" + id + "/?rm_utm_source=shared_webpage&rm_utm_campaign=create_open_link&rm_utm_medium=qr_code"} {
		g := h.do("GET", path, nil, nil)
		if g.Code != 200 {
			t.Fatalf("GET %s = %d", path, g.Code)
		}
	}
}

// ---- (a)(b) auth over HTTP ----

func TestPostAuth(t *testing.T) {
	h := newHarness(t, nil)
	body := twoPageBody(t)
	cases := map[string]string{
		"no token":      "",
		"raw-key token": mintToken(t, "share-url", time.Now().Add(time.Hour), false),
		"no scope":      mintToken(t, "intgr docedit", time.Now().Add(time.Hour), true),
		"expired":       mintToken(t, "share-url", time.Now().Add(-time.Hour), true),
		"garbage":       "abc.def.ghi",
	}
	for name, tok := range cases {
		if w := h.post(t, tok, body); w.Code != 401 {
			t.Errorf("%s: status %d, want 401", name, w.Code)
		}
	}
	if len(h.rec.ids) != 0 {
		t.Fatal("unauthorised requests must not enqueue")
	}
	if w := h.post(t, goodToken(t), body); w.Code != 200 {
		t.Fatalf("derived-key token must be accepted, got %d", w.Code)
	}
}

// ---- (e) GET id validation ----

func TestGetInvalidIDsAre404(t *testing.T) {
	h := newHarness(t, nil)
	for _, p := range []string{
		"/share/nope/", "/share/AAAAAAAAAAAAAAAAAAAAAA/", "/share/..%2f..%2fetc/passwd", "/share/%2e%2e/",
		"/share/v1/", "/share/AAAAAAAAAAAAAAAAAAAAAA/note.md", "/share/..%2fx/note.md",
		"/share/AAAAAAAAAAAAAAAAAAAAAA/pages/1.png", "/share/..%5c..%5cwindows/pages/1.png",
	} {
		if w := h.do("GET", p, nil, nil); w.Code != 404 {
			t.Errorf("GET %s = %d, want 404", p, w.Code)
		}
	}
}

func TestPageAndNoteEndpoints(t *testing.T) {
	h := newHarness(t, nil)
	id := strings.TrimSuffix(strings.TrimPrefix(linkFrom(t, h.post(t, goodToken(t), twoPageBody(t))), publicURL+"/"), "/")

	if w := h.do("GET", "/share/"+id+"/note.md", nil, nil); w.Code != 404 {
		t.Errorf("note.md before conversion = %d, want 404", w.Code)
	}
	p := h.do("GET", "/share/"+id+"/pages/2.png", nil, nil)
	if p.Code != 200 || p.Header().Get("Content-Type") != "image/png" || !bytes.Equal(p.Body.Bytes(), tinyPNG(t, 200)) {
		t.Fatalf("page 2: %d %q", p.Code, p.Header().Get("Content-Type"))
	}
	for _, bad := range []string{"0.png", "3.png", "-1.png", "1", "1.png.png", "01x.png", "../1.png", "abc.png"} {
		if w := h.do("GET", "/share/"+id+"/pages/"+bad, nil, nil); w.Code != 404 {
			t.Errorf("pages/%s = %d, want 404", bad, w.Code)
		}
	}
	_ = h.st.MarkDone(id, "# Title\n\nbody", "m")
	n := h.do("GET", "/share/"+id+"/note.md", nil, nil)
	if n.Code != 200 || !strings.HasPrefix(n.Header().Get("Content-Type"), "text/markdown") || n.Body.String() != "# Title\n\nbody" {
		t.Fatalf("note.md: %d %q %q", n.Code, n.Header().Get("Content-Type"), n.Body.String())
	}
}

func TestHealthz(t *testing.T) {
	if w := newHarness(t, nil).do("GET", "/healthz", nil, nil); w.Code != 200 {
		t.Fatalf("healthz = %d", w.Code)
	}
}

// ---- (f) validation statuses ----

func TestPostValidationStatuses(t *testing.T) {
	h := newHarness(t, nil)
	tok := goodToken(t)
	if w := h.post(t, tok, wireBody(t, "d", []string{"a"}, [][]byte{[]byte("not a png at all")})); w.Code != 400 {
		t.Errorf("non-PNG page = %d, want 400", w.Code)
	}
	var pages [][]byte
	var ids []string
	for i := 0; i < 6; i++ { // MaxPages is 5 in the harness
		pages = append(pages, tinyPNG(t, uint8(i)))
		ids = append(ids, fmt.Sprint(i))
	}
	if w := h.post(t, tok, wireBody(t, "d", ids, pages)); w.Code != 400 {
		t.Errorf("too many pages = %d, want 400", w.Code)
	}
	if w := h.post(t, tok, []byte("garbage")); w.Code != 400 {
		t.Errorf("garbage body = %d, want 400", w.Code)
	}
	if len(h.rec.ids) != 0 {
		t.Error("rejected uploads must not enqueue")
	}
}

func TestPostOversizedBodyIs413(t *testing.T) {
	h := newHarness(t, func(d *Deps) { d.MaxBody = 4096 })
	big := bytes.Repeat([]byte{0xAB}, 20000)
	body := wireBody(t, "d", []string{"a"}, [][]byte{append(tinyPNG(t, 1), big...)})
	if w := h.post(t, goodToken(t), body); w.Code != 413 {
		t.Fatalf("status %d, want 413", w.Code)
	}
	// Also when the client lies about / hides the length (chunked, unknown length).
	req := httptest.NewRequest("POST", "/share/v1/link", io.NopCloser(bytes.NewReader(body)))
	req.ContentLength = -1
	req.Header.Set("Content-Type", wireContentType())
	req.Header.Set("Authorization", "Bearer "+goodToken(t))
	w := httptest.NewRecorder()
	h.h.ServeHTTP(w, req)
	if w.Code != 413 {
		t.Fatalf("unknown-length oversized status %d, want 413", w.Code)
	}
}

// ---- (g) worker integration, rendering ----

type fakeConv struct {
	out string
	err error
}

func (f fakeConv) Model() string { return "fake" }
func (f fakeConv) Convert(context.Context, [][]byte) (string, error) {
	return f.out, f.err
}

func withWorker(t *testing.T, conv fakeConv) *harness {
	t.Helper()
	var w *worker.Worker
	h := newHarness(t, func(d *Deps) {
		w = worker.New(d.Store, conv, d.Log)
		d.Enqueuer = w
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	return h
}

func waitForPage(t *testing.T, h *harness, path, want string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		last = h.do("GET", path, nil, nil).Body.String()
		if strings.Contains(last, want) {
			return last
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("page %s never contained %q; last body:\n%s", path, want, last)
	return ""
}

func TestPageGoesFromConvertingToRenderedNote(t *testing.T) {
	h := newHarness(t, nil)
	id := strings.TrimSuffix(strings.TrimPrefix(linkFrom(t, h.post(t, goodToken(t), twoPageBody(t))), publicURL+"/"), "/")
	pending := h.do("GET", "/share/"+id+"/", nil, nil)
	body := pending.Body.String()
	if !strings.Contains(strings.ToLower(body), "converting") || !strings.Contains(body, `http-equiv="refresh"`) {
		t.Fatalf("pending page must say converting and auto-refresh:\n%s", body)
	}
	if !strings.Contains(body, "pages/1.png") || !strings.Contains(body, "pages/2.png") {
		t.Fatal("pending page must still show the original page images")
	}
	_ = h.st.MarkDone(id, "# Groceries\n\n- [ ] oat milk\n", "m")
	done := h.do("GET", "/share/"+id+"/", nil, nil).Body.String()
	if !strings.Contains(done, "<h1") || !strings.Contains(done, "Groceries") || !strings.Contains(done, "oat milk") {
		t.Fatalf("done page must render the note as HTML:\n%s", done)
	}
	if strings.Contains(done, `http-equiv="refresh"`) {
		t.Fatal("done page must stop auto-refreshing")
	}
	if !strings.Contains(done, "note.md") {
		t.Fatal("done page must link the markdown download")
	}
}

func TestEndToEndWithFakeConverter(t *testing.T) {
	h := withWorker(t, fakeConv{out: "# Meeting notes\n\n1. first\n2. second"})
	id := strings.TrimSuffix(strings.TrimPrefix(linkFrom(t, h.post(t, goodToken(t), twoPageBody(t))), publicURL+"/"), "/")
	waitForPage(t, h, "/share/"+id+"/", "Meeting notes")
	n := h.do("GET", "/share/"+id+"/note.md", nil, nil)
	if n.Code != 200 || !strings.Contains(n.Body.String(), "2. second") {
		t.Fatalf("note.md %d %q", n.Code, n.Body.String())
	}
}

func TestFailedConversionIsShown(t *testing.T) {
	h := withWorker(t, fakeConv{err: errors.New("upstream said no")})
	id := strings.TrimSuffix(strings.TrimPrefix(linkFrom(t, h.post(t, goodToken(t), twoPageBody(t))), publicURL+"/"), "/")
	body := waitForPage(t, h, "/share/"+id+"/", "upstream said no")
	if strings.Contains(body, `http-equiv="refresh"`) {
		t.Fatal("failed page must not auto-refresh")
	}
	if !strings.Contains(body, "pages/1.png") {
		t.Fatal("failed page must still show the original pages")
	}
}

func TestConversionDisabledMessage(t *testing.T) {
	h := newHarness(t, func(d *Deps) { d.Enqueuer = nil })
	id := strings.TrimSuffix(strings.TrimPrefix(linkFrom(t, h.post(t, goodToken(t), twoPageBody(t))), publicURL+"/"), "/")
	body := h.do("GET", "/share/"+id+"/", nil, nil).Body.String()
	if !strings.Contains(strings.ToLower(body), "conversion disabled") || strings.Contains(body, `http-equiv="refresh"`) {
		t.Fatalf("disabled page wrong:\n%s", body)
	}
	if !strings.Contains(body, "pages/1.png") {
		t.Fatal("images must still be shown")
	}
}

// ---- (i) no raw HTML ----

func TestMarkdownRawHTMLIsNotPassedThrough(t *testing.T) {
	h := newHarness(t, nil)
	id := strings.TrimSuffix(strings.TrimPrefix(linkFrom(t, h.post(t, goodToken(t), twoPageBody(t))), publicURL+"/"), "/")
	evil := "# Hi\n\n<script>alert(1)</script>\n\n<img src=x onerror=alert(2)>\n\n[click](javascript:alert(3))\n\ninline <b onclick=alert(4)>bold</b>\n"
	_ = h.st.MarkDone(id, evil, "m")
	w := h.do("GET", "/share/"+id+"/", nil, nil)
	body := w.Body.String()
	for _, bad := range []string{"<script>alert", "onerror=", "javascript:alert", "<b onclick"} {
		if strings.Contains(body, bad) {
			t.Errorf("page contains dangerous %q", bad)
		}
	}
	// Only our own nonce'd script may exist; none may contain the payload.
	if strings.Contains(body, "alert(1)") && !strings.Contains(body, "&lt;script&gt;") {
		t.Error("payload must be escaped or dropped")
	}
	// The raw markdown download keeps the text verbatim (it is served as text, not HTML).
	n := h.do("GET", "/share/"+id+"/note.md", nil, nil)
	if n.Body.String() != evil || strings.Contains(n.Header().Get("Content-Type"), "html") {
		t.Fatal("note.md must be served verbatim as markdown")
	}
}

func TestSecurityHeaders(t *testing.T) {
	h := newHarness(t, nil)
	id := strings.TrimSuffix(strings.TrimPrefix(linkFrom(t, h.post(t, goodToken(t), twoPageBody(t))), publicURL+"/"), "/")
	w := h.do("GET", "/share/"+id+"/", nil, nil)
	csp := w.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "frame-ancestors 'none'", "img-src 'self'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q lacks %q", csp, want)
		}
	}
	if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") || strings.Contains(csp, "http") {
		t.Errorf("CSP too loose: %q", csp)
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Error("missing nosniff / no-referrer")
	}
	if strings.Contains(w.Body.String(), "https://") && !strings.Contains(w.Body.String(), publicURL) {
		// no external assets allowed (links to our own URL are fine)
		t.Error("page references an external URL")
	}
}
