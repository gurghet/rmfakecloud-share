package store

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestNewIDIs128BitBase64URLAndUnique(t *testing.T) {
	a, _ := NewID()
	b, _ := NewID()
	if a == b {
		t.Fatal("ids must be unique")
	}
	raw, err := base64.RawURLEncoding.DecodeString(a)
	if err != nil || len(raw) != 16 {
		t.Fatalf("id %q must decode to 16 bytes, got %d (%v)", a, len(raw), err)
	}
	if !ValidID(a) {
		t.Fatal("generated id must validate")
	}
}

func TestValidIDRejectsTraversalAndGarbage(t *testing.T) {
	bad := []string{
		"", ".", "..", "../etc/passwd", "..%2f..", "a/b", `a\b`, "AAAAAAAAAAAAAAAAAAAAAA/", "AAAAAAAAAAAAAAAAAAAAA", // 21 chars
		"AAAAAAAAAAAAAAAAAAAAAAA",   // 23 chars
		"AAAAAAAAAAAAAAAAAAAAA=",    // padding
		"AAAAAAAAAAAAAAAAAAAA\x00A", // NUL
		"AAAAAAAAAAAAAAAAAAAA..",    // dots
		"AAAAAAAAAAAAAAAAAAAA A",
	}
	for _, s := range bad {
		if ValidID(s) {
			t.Errorf("ValidID(%q) = true, want false", s)
		}
	}
	if !ValidID("AAAAAAAAAAAAAAAAAAAAAA") || !ValidID("a-_zA-_zA-_zA-_zA-_zAA"[:22]) {
		t.Error("22-char base64url ids must be valid")
	}
}

func TestCreateGetRoundTrip(t *testing.T) {
	s := newStore(t)
	pages := [][]byte{[]byte("\x89PNG-one"), []byte("\x89PNG-two")}
	m, err := s.Create(Meta{DocID: "doc", PageIDs: []string{"p1", "p2"}, MailList: []string{}, User: "u1"}, pages)
	if err != nil {
		t.Fatal(err)
	}
	if !ValidID(m.ID) {
		t.Fatalf("created id %q invalid", m.ID)
	}
	got, err := s.Get(m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusPending || got.DocID != "doc" || got.User != "u1" || got.PageCount != 2 || got.Created.IsZero() {
		t.Fatalf("unexpected meta %+v", got)
	}
	p2, err := s.Page(m.ID, 2)
	if err != nil || !bytes.Equal(p2, pages[1]) {
		t.Fatalf("page 2 = %q, %v", p2, err)
	}
	if _, err := s.Page(m.ID, 3); !errors.Is(err, ErrNotFound) {
		t.Fatalf("page 3 err = %v, want ErrNotFound", err)
	}
	if _, err := s.Page(m.ID, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("page 0 err = %v, want ErrNotFound", err)
	}
	all, err := s.Pages(m.ID)
	if err != nil || len(all) != 2 || !bytes.Equal(all[0], pages[0]) {
		t.Fatalf("Pages = %v, %v", all, err)
	}
}

func TestGetUnknownAndMalformed(t *testing.T) {
	s := newStore(t)
	for _, id := range []string{"AAAAAAAAAAAAAAAAAAAAAA", "../x"} {
		if _, err := s.Get(id); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get(%q) err = %v, want ErrNotFound", id, err)
		}
	}
}

func TestMarkDoneAndFailed(t *testing.T) {
	s := newStore(t)
	m, _ := s.Create(Meta{DocID: "d"}, [][]byte{[]byte("x")})
	if err := s.MarkDone(m.ID, "# Hi\n", "model-x"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(m.ID)
	if got.Status != StatusDone || got.Model != "model-x" {
		t.Fatalf("meta after done: %+v", got)
	}
	note, err := s.Note(m.ID)
	if err != nil || note != "# Hi\n" {
		t.Fatalf("note = %q, %v", note, err)
	}

	m2, _ := s.Create(Meta{DocID: "d2"}, [][]byte{[]byte("x")})
	if err := s.MarkFailed(m2.ID, "boom"); err != nil {
		t.Fatal(err)
	}
	got2, _ := s.Get(m2.ID)
	if got2.Status != StatusFailed || got2.Error != "boom" {
		t.Fatalf("meta after fail: %+v", got2)
	}
	if _, err := s.Note(m2.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed share must have no note, err=%v", err)
	}
}

func TestPendingListsOnlyPendingOldestFirst(t *testing.T) {
	s := newStore(t)
	a, _ := s.Create(Meta{DocID: "a", Created: time.Now().Add(-2 * time.Hour)}, [][]byte{[]byte("x")})
	b, _ := s.Create(Meta{DocID: "b"}, [][]byte{[]byte("x")})
	c, _ := s.Create(Meta{DocID: "c"}, [][]byte{[]byte("x")})
	_ = s.MarkDone(c.ID, "n", "m")
	ids, err := s.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != a.ID || ids[1] != b.ID {
		t.Fatalf("Pending = %v, want [%s %s]", ids, a.ID, b.ID)
	}
}

func TestNoTempFilesLeftBehind(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	m, _ := s.Create(Meta{DocID: "d"}, [][]byte{[]byte("x")})
	_ = s.MarkDone(m.ID, "n", "m")
	var leftovers []string
	_ = filepath.Walk(dir, func(p string, i os.FileInfo, _ error) error {
		if i != nil && !i.IsDir() && filepath.Ext(p) == ".tmp" {
			leftovers = append(leftovers, p)
		}
		return nil
	})
	if len(leftovers) != 0 {
		t.Fatalf("temp files left: %v", leftovers)
	}
}
