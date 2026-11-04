// Package server implements the HTTP interface.
package server

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gurghet/rmfakecloud-share/internal/auth"
	"github.com/gurghet/rmfakecloud-share/internal/store"
)

// Enqueuer schedules conversions.
type Enqueuer interface{ Enqueue(id string) }

// Deps are the server's collaborators and settings.
type Deps struct {
	Store     *store.Store
	Verifier  *auth.Verifier
	Enqueuer  Enqueuer // nil disables conversion
	PublicURL string   // no trailing slash
	MaxBody   int64
	MaxPages  int
	Log       *slog.Logger
	Now       func() time.Time

	MaxPageBytes         int64         // per page part; 0 = unlimited
	MaxConcurrentUploads int           // 0 = unlimited
	UploadWait           time.Duration // how long an upload waits for a free slot (0 = 30s)
	MaxPagesPerDay       int           // per user, rolling 24 h; 0 = unlimited
	ResponseTimeout      time.Duration // write budget once an upload has been read (0 = 30s)
}

// Timeouts are the HTTP server timeouts.
type Timeouts struct{ ReadHeader, Read, Write, Idle time.Duration }

// DefaultTimeouts allow slow uploads over bad links.
func DefaultTimeouts() Timeouts {
	return Timeouts{ReadHeader: 10 * time.Second, Read: 2 * time.Minute, Write: 3 * time.Minute, Idle: 2 * time.Minute}
}

// NewHTTPServer builds the http.Server with t applied.
func NewHTTPServer(addr string, h http.Handler, t Timeouts) *http.Server {
	return &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: t.ReadHeader, ReadTimeout: t.Read,
		WriteTimeout: t.Write, IdleTimeout: t.Idle, MaxHeaderBytes: 32 << 10}
}

type srv struct {
	Deps
	slots chan struct{} // upload concurrency limiter, nil = unlimited
	quota *quota
}

// New returns the HTTP handler.
func New(d Deps) http.Handler {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	s := &srv{Deps: d, quota: newQuota(d.MaxPagesPerDay)}
	if d.MaxConcurrentUploads > 0 {
		s.slots = make(chan struct{}, d.MaxConcurrentUploads)
	}
	if d.Store != nil && d.MaxPagesPerDay > 0 {
		if metas, err := d.Store.List(); err != nil {
			d.Log.Error("loading existing shares for the quota failed", "err", err)
		} else {
			s.quota.load(metas)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /share/v1/link", s.handleLink)
	mux.HandleFunc("GET /share/{id}/{$}", s.handlePage)
	mux.HandleFunc("GET /share/{id}/note.md", s.handleNote)
	mux.HandleFunc("GET /share/{id}/pages/{file}", s.handlePageImage)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	// Refuse non-canonical paths outright instead of letting the mux redirect them.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "..") || strings.Contains(r.URL.Path, "//") {
			http.NotFound(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if len(h) > len(p) && strings.EqualFold(h[:len(p)], p) {
		return strings.TrimSpace(h[len(p):])
	}
	return ""
}

// acquire takes an upload slot, waiting at most UploadWait.
func (s *srv) acquire(r *http.Request) bool {
	if s.slots == nil {
		return true
	}
	wait := s.UploadWait
	if wait <= 0 {
		wait = 30 * time.Second
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case s.slots <- struct{}{}:
		return true
	case <-timer.C:
		return false
	case <-r.Context().Done():
		return false
	}
}

func (s *srv) release() {
	if s.slots != nil {
		<-s.slots
	}
}

// extendWriteDeadline gives the response its own time budget. The server's
// WriteTimeout starts when the headers arrive, so a slow upload would
// otherwise use it up and the client would get no answer at all.
func (s *srv) extendWriteDeadline(w http.ResponseWriter) {
	d := s.ResponseTimeout
	if d <= 0 {
		d = 30 * time.Second
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(d))
}

func (s *srv) handleLink(w http.ResponseWriter, r *http.Request) {
	// Never log the token itself.
	claims, err := s.Verifier.Verify(bearer(r), s.Now())
	if err != nil {
		s.Log.Warn("share rejected: auth", "reason", err.Error(), "remote", r.RemoteAddr)
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if !s.acquire(r) {
		s.Log.Warn("share rejected: too many concurrent uploads")
		w.Header().Set("Retry-After", "10")
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "busy, retry shortly"})
		return
	}
	defer s.release()
	if r.ContentLength > s.MaxBody {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.MaxBody)
	up, err := parseUploadLimits(r.Header.Get("Content-Type"), r.Body, uploadLimits{MaxPages: s.MaxPages, MaxPageBytes: s.MaxPageBytes})
	s.extendWriteDeadline(w) // the body has been read, whatever the outcome
	if err != nil {
		var ue *uploadError
		status, msg := http.StatusBadRequest, "bad request"
		if errors.As(err, &ue) {
			status, msg = ue.status, ue.msg
		}
		s.Log.Warn("share rejected: upload", "status", status, "reason", err.Error())
		s.Log.Debug("rejected upload details", "user", claims.Subject)
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	now := s.Now()
	undo, retryAfter, ok := s.quota.reserve(claims.Subject, len(up.Pages), now)
	if !ok {
		s.Log.Warn("share rejected: daily page quota exceeded", "limit", s.MaxPagesPerDay, "pages", len(up.Pages))
		s.Log.Debug("quota details", "user", claims.Subject)
		w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())+1))
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "daily page quota exceeded"})
		return
	}
	meta, err := s.Store.Create(store.Meta{
		DocID: up.DocID, PageIDs: up.PageIDs, MailList: up.MailList, User: claims.Subject, Created: now.UTC(),
	}, up.Pages)
	if err != nil {
		undo()
		s.Log.Error("storing share failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	// MailList is recorded in meta.json but not acted on in v1. Only its size is logged.
	s.Log.Info("share created", "share", meta.ID, "pages", len(up.Pages), "mail_list_count", len(up.MailList))
	s.Log.Debug("share details", "share", meta.ID, "user", claims.Subject, "doc", up.DocID)
	if s.Enqueuer != nil {
		s.Enqueuer.Enqueue(meta.ID)
	}
	// Respond right away: the tablet must not wait for the conversion.
	writeJSON(w, http.StatusOK, map[string]string{"link": s.PublicURL + "/" + meta.ID + "/"})
}

// lookup validates the id and loads the share, answering 404 itself on failure.
func (s *srv) lookup(w http.ResponseWriter, r *http.Request) (store.Meta, bool) {
	id := r.PathValue("id")
	if !store.ValidID(id) {
		http.NotFound(w, r)
		return store.Meta{}, false
	}
	m, err := s.Store.Get(id)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.Log.Error("loading share failed", "share", id, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
		} else {
			http.NotFound(w, r)
		}
		return store.Meta{}, false
	}
	return m, true
}

func setCommonHeaders(h http.Header) {
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Robots-Tag", "noindex, nofollow")
}

func (s *srv) handleNote(w http.ResponseWriter, r *http.Request) {
	m, ok := s.lookup(w, r)
	if !ok {
		return
	}
	note, err := s.Store.Note(m.ID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	setCommonHeaders(w.Header())
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(note))
}

func (s *srv) handlePageImage(w http.ResponseWriter, r *http.Request) {
	m, ok := s.lookup(w, r)
	if !ok {
		return
	}
	file := r.PathValue("file")
	numStr, found := strings.CutSuffix(file, ".png")
	if !found || numStr == "" || strings.TrimLeft(numStr, "0123456789") != "" {
		http.NotFound(w, r)
		return
	}
	n, err := strconv.Atoi(numStr)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	b, err := s.Store.Page(m.ID, n)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	setCommonHeaders(w.Header())
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	_, _ = w.Write(b)
}

func newNonce() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return base64.RawStdEncoding.EncodeToString(b[:])
}
