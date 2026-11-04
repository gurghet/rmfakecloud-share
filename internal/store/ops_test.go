package store

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRequeueOnlyFailedShares(t *testing.T) {
	s := newStore(t)
	m, _ := s.Create(Meta{DocID: "d"}, [][]byte{[]byte("x")})
	if err := s.Requeue(m.ID); !errors.Is(err, ErrNotFailed) {
		t.Fatalf("requeue of pending share err = %v, want ErrNotFailed", err)
	}
	_ = s.MarkFailed(m.ID, "bad request")
	if err := s.Requeue(m.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(m.ID)
	if got.Status != StatusPending || got.Error != "" {
		t.Fatalf("after requeue: %+v", got)
	}
	ids, _ := s.Pending()
	if len(ids) != 1 || ids[0] != m.ID {
		t.Fatalf("Pending = %v", ids)
	}
	if err := s.Requeue("../nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bad id err = %v", err)
	}
}

func TestListReturnsAllShares(t *testing.T) {
	s := newStore(t)
	a, _ := s.Create(Meta{DocID: "a", User: "u1"}, [][]byte{[]byte("x"), []byte("y")})
	b, _ := s.Create(Meta{DocID: "b", User: "u2"}, [][]byte{[]byte("x")})
	_ = s.MarkDone(b.ID, "n", "m")
	all, err := s.List()
	if err != nil || len(all) != 2 {
		t.Fatalf("List = %v, %v", all, err)
	}
	seen := map[string]Meta{}
	for _, m := range all {
		seen[m.ID] = m
	}
	if seen[a.ID].PageCount != 2 || seen[b.ID].Status != StatusDone || seen[a.ID].User != "u1" {
		t.Fatalf("unexpected metas %+v", seen)
	}
}

func TestPruneDeletesOldFinishedSharesOnly(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	old, _ := s.Create(Meta{DocID: "old", Created: now.Add(-48 * time.Hour)}, [][]byte{[]byte("x")})
	_ = s.MarkDone(old.ID, "n", "m")
	oldFailed, _ := s.Create(Meta{DocID: "of", Created: now.Add(-48 * time.Hour)}, [][]byte{[]byte("x")})
	_ = s.MarkFailed(oldFailed.ID, "e")
	oldPending, _ := s.Create(Meta{DocID: "op", Created: now.Add(-48 * time.Hour)}, [][]byte{[]byte("x")})
	fresh, _ := s.Create(Meta{DocID: "fresh", Created: now.Add(-time.Hour)}, [][]byte{[]byte("x")})
	_ = s.MarkDone(fresh.ID, "n", "m")

	n, err := s.Prune(now, 24*time.Hour)
	if err != nil || n != 2 {
		t.Fatalf("Prune = %d, %v; want 2", n, err)
	}
	for _, id := range []string{old.ID, oldFailed.ID} {
		if _, err := s.Get(id); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s should be gone, err=%v", id, err)
		}
	}
	for _, id := range []string{oldPending.ID, fresh.ID} {
		if _, err := s.Get(id); err != nil {
			t.Errorf("%s must survive: %v", id, err)
		}
	}
}

func TestCorruptMetaIsReportedAndQuarantinedNotSkippedSilently(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	var logs bytes.Buffer
	s.Log = slog.New(slog.NewTextHandler(&logs, nil))
	good, _ := s.Create(Meta{DocID: "g"}, [][]byte{[]byte("x")})
	bad, _ := s.Create(Meta{DocID: "b"}, [][]byte{[]byte("x")})
	metaPath := filepath.Join(dir, "shares", bad.ID, "meta.json")
	if err := os.WriteFile(metaPath, []byte("{not json"), 0o640); err != nil {
		t.Fatal(err)
	}

	ids, err := s.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != good.ID {
		t.Fatalf("Pending = %v, want only the good share", ids)
	}
	out := logs.String()
	if !strings.Contains(out, "level=ERROR") || !strings.Contains(out, bad.ID) || !strings.Contains(strings.ToLower(out), "corrupt") {
		t.Fatalf("corrupt share must be logged loudly with its id, got:\n%s", out)
	}
	if _, err := os.Stat(metaPath + ".corrupt"); err != nil {
		t.Fatalf("meta.json must be quarantined as meta.json.corrupt: %v", err)
	}
	if _, err := s.Get(bad.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("quarantined share err = %v, want ErrNotFound", err)
	}
}

func TestPruneLoopRemovesExpiredSharesUsingInjectedClock(t *testing.T) {
	s := newStore(t)
	created := time.Now()
	m, _ := s.Create(Meta{DocID: "d", Created: created}, [][]byte{[]byte("x")})
	_ = s.MarkDone(m.ID, "n", "m")

	var mu sync.Mutex
	clock := created.Add(time.Hour) // not yet expired
	now := func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.PruneLoop(ctx, 10*time.Millisecond, 24*time.Hour, now)

	time.Sleep(60 * time.Millisecond)
	if _, err := s.Get(m.ID); err != nil {
		t.Fatalf("share removed too early: %v", err)
	}
	mu.Lock()
	clock = created.Add(25 * time.Hour)
	mu.Unlock()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := s.Get(m.ID); errors.Is(err, ErrNotFound) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("share older than retention was never removed")
}
