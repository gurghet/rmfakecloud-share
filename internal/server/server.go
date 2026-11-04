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
}

type srv struct{ Deps }

// New returns the HTTP handler.
func New(d Deps) http.Handler {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	s := &srv{d}
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

func (s *srv) handleLink(w http.ResponseWriter, r *http.Request) {
	// Never log the token itself.
	claims, err := s.Verifier.Verify(bearer(r), s.Now())
	if err != nil {
		s.Log.Warn("share rejected: auth", "reason", err.Error(), "remote", r.RemoteAddr)
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if r.ContentLength > s.MaxBody {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.MaxBody)
	up, err := parseUpload(r.Header.Get("Content-Type"), r.Body, s.MaxPages)
	if err != nil {
		var ue *uploadError
		status, msg := http.StatusBadRequest, "bad request"
		if errors.As(err, &ue) {
			status, msg = ue.status, ue.msg
		}
		s.Log.Warn("share rejected: upload", "status", status, "reason", err.Error(), "user", claims.Subject)
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	meta, err := s.Store.Create(store.Meta{
		DocID: up.DocID, PageIDs: up.PageIDs, MailList: up.MailList, User: claims.Subject, Created: s.Now().UTC(),
	}, up.Pages)
	if err != nil {
		s.Log.Error("storing share failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	// MailList is recorded but not acted on in v1.
	s.Log.Info("share created", "share", meta.ID, "user", claims.Subject, "doc", up.DocID, "pages", len(up.Pages), "mail_list", up.MailList)
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
