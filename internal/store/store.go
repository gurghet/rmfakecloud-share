// Package store persists shares on the filesystem.
package store

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// Status of a share's conversion.
type Status string

const (
	StatusPending Status = "pending"
	StatusDone    Status = "done"
	StatusFailed  Status = "failed"
)

// ErrNotFound is returned for unknown or malformed ids.
var ErrNotFound = errors.New("share not found")

// Meta is stored as meta.json.
type Meta struct {
	ID        string    `json:"id"`
	DocID     string    `json:"doc_id"`
	PageIDs   []string  `json:"page_ids"`
	MailList  []string  `json:"mail_list"`
	User      string    `json:"user"`
	Created   time.Time `json:"created"`
	Status    Status    `json:"status"`
	Error     string    `json:"error,omitempty"`
	Model     string    `json:"model,omitempty"`
	PageCount int       `json:"page_count"`
}

// Store is a filesystem-backed share store.
//
// Layout: <root>/shares/<id>/{meta.json,note.md,pages/NN.png}
type Store struct{ root string }

var idRe = regexp.MustCompile(`^[A-Za-z0-9_-]{22}$`)

// New opens (creating if needed) a store under dataDir.
func New(dataDir string) (*Store, error) {
	root := filepath.Join(dataDir, "shares")
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, err
	}
	return &Store{root: root}, nil
}

// NewID returns a fresh 128-bit base64url id without padding.
func NewID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// ValidID reports whether s is a well-formed share id.
func ValidID(s string) bool { return idRe.MatchString(s) }

func (s *Store) dir(id string) (string, error) {
	if !ValidID(id) {
		return "", ErrNotFound
	}
	return filepath.Join(s.root, id), nil
}

// writeAtomic writes data to path via a temp file in the same directory.
func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, 0o640); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

func pageName(n int) string { return fmt.Sprintf("%02d.png", n) }

// Create writes a new pending share. Meta.ID, Status and PageCount are set by
// the store; Created is set unless provided.
func (s *Store) Create(m Meta, pages [][]byte) (Meta, error) {
	id, err := NewID()
	if err != nil {
		return Meta{}, err
	}
	m.ID = id
	m.Status = StatusPending
	m.PageCount = len(pages)
	if m.Created.IsZero() {
		m.Created = time.Now().UTC()
	}
	if m.PageIDs == nil {
		m.PageIDs = []string{}
	}
	if m.MailList == nil {
		m.MailList = []string{}
	}
	dir := filepath.Join(s.root, id)
	if err := os.MkdirAll(filepath.Join(dir, "pages"), 0o750); err != nil {
		return Meta{}, err
	}
	for i, p := range pages {
		if err := writeAtomic(filepath.Join(dir, "pages", pageName(i+1)), p); err != nil {
			os.RemoveAll(dir)
			return Meta{}, err
		}
	}
	// meta.json last: a share without meta.json is incomplete and ignored.
	if err := s.writeMeta(m); err != nil {
		os.RemoveAll(dir)
		return Meta{}, err
	}
	return m, nil
}

func (s *Store) writeMeta(m Meta) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(s.root, m.ID, "meta.json"), b)
}

// Get loads meta for id.
func (s *Store) Get(id string) (Meta, error) {
	dir, err := s.dir(id)
	if err != nil {
		return Meta{}, err
	}
	b, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Meta{}, ErrNotFound
		}
		return Meta{}, err
	}
	var m Meta
	if err := json.Unmarshal(b, &m); err != nil {
		return Meta{}, fmt.Errorf("corrupt meta for %s: %w", id, err)
	}
	return m, nil
}

// Page returns PNG bytes of the 1-based page n.
func (s *Store) Page(id string, n int) ([]byte, error) {
	m, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	if n < 1 || n > m.PageCount {
		return nil, ErrNotFound
	}
	b, err := os.ReadFile(filepath.Join(s.root, id, "pages", pageName(n)))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return b, nil
}

// Pages reads all page images in order.
func (s *Store) Pages(id string) ([][]byte, error) {
	m, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	out := make([][]byte, 0, m.PageCount)
	for i := 1; i <= m.PageCount; i++ {
		b, err := s.Page(id, i)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

// Note returns the converted markdown.
func (s *Store) Note(id string) (string, error) {
	dir, err := s.dir(id)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(filepath.Join(dir, "note.md"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", ErrNotFound
		}
		return "", err
	}
	return string(b), nil
}

// MarkDone stores the note and marks the share done.
func (s *Store) MarkDone(id, note, model string) error {
	m, err := s.Get(id)
	if err != nil {
		return err
	}
	// Note first, then meta: a crash in between leaves the share pending.
	if err := writeAtomic(filepath.Join(s.root, id, "note.md"), []byte(note)); err != nil {
		return err
	}
	m.Status, m.Model, m.Error = StatusDone, model, ""
	return s.writeMeta(m)
}

// MarkFailed records a conversion failure.
func (s *Store) MarkFailed(id, msg string) error {
	m, err := s.Get(id)
	if err != nil {
		return err
	}
	m.Status, m.Error = StatusFailed, msg
	return s.writeMeta(m)
}

// Pending lists ids of shares whose status is pending, oldest first.
func (s *Store) Pending() ([]string, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	var metas []Meta
	for _, e := range entries {
		if !e.IsDir() || !ValidID(e.Name()) {
			continue
		}
		m, err := s.Get(e.Name())
		if err != nil || m.Status != StatusPending {
			continue
		}
		metas = append(metas, m)
	}
	sort.Slice(metas, func(i, j int) bool { return metas[i].Created.Before(metas[j].Created) })
	ids := make([]string, len(metas))
	for i, m := range metas {
		ids[i] = m.ID
	}
	return ids, nil
}
