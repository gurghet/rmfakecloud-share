package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/gurghet/rmfakecloud-share/internal/convert"
	"github.com/gurghet/rmfakecloud-share/internal/store"
)

type fakeConv struct {
	mu    sync.Mutex
	calls [][][]byte
	out   string
	err   error
}

func (f *fakeConv) Model() string { return "fake-model" }
func (f *fakeConv) Convert(ctx context.Context, pages [][]byte) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, pages)
	f.mu.Unlock()
	return f.out, f.err
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func waitStatus(t *testing.T, s *store.Store, id string, want store.Status) store.Meta {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		m, err := s.Get(id)
		if err == nil && m.Status == want {
			return m
		}
		time.Sleep(10 * time.Millisecond)
	}
	m, _ := s.Get(id)
	t.Fatalf("share %s never reached %s, last meta %+v", id, want, m)
	return m
}

func TestPendingToDoneWritesNote(t *testing.T) {
	s, _ := store.New(t.TempDir())
	fc := &fakeConv{out: "# Converted"}
	w := New(s, fc, quiet())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	m, _ := s.Create(store.Meta{DocID: "d"}, [][]byte{[]byte("p1"), []byte("p2")})
	if got, _ := s.Get(m.ID); got.Status != store.StatusPending {
		t.Fatalf("initial status %s", got.Status)
	}
	w.Enqueue(m.ID)
	done := waitStatus(t, s, m.ID, store.StatusDone)
	if done.Model != "fake-model" {
		t.Fatalf("model = %q", done.Model)
	}
	note, err := s.Note(m.ID)
	if err != nil || note != "# Converted" {
		t.Fatalf("note = %q, %v", note, err)
	}
	if len(fc.calls) != 1 || len(fc.calls[0]) != 2 || string(fc.calls[0][1]) != "p2" {
		t.Fatalf("converter got wrong pages: %q", fc.calls)
	}
}

func TestConverterErrorMarksFailed(t *testing.T) {
	s, _ := store.New(t.TempDir())
	w := New(s, &fakeConv{err: convert.Permanent(errors.New("upstream exploded"))}, quiet())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = w.Start(ctx)
	m, _ := s.Create(store.Meta{DocID: "d"}, [][]byte{[]byte("p1")})
	w.Enqueue(m.ID)
	f := waitStatus(t, s, m.ID, store.StatusFailed)
	if f.Error == "" {
		t.Fatal("failed share must carry an error message")
	}
	if _, err := s.Note(m.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("no note expected, err=%v", err)
	}
}

func TestStartReenqueuesPendingShares(t *testing.T) {
	dir := t.TempDir()
	s, _ := store.New(dir)
	// Simulates a crash: share persisted as pending, nothing queued in memory.
	a, _ := s.Create(store.Meta{DocID: "a"}, [][]byte{[]byte("pa")})
	b, _ := s.Create(store.Meta{DocID: "b"}, [][]byte{[]byte("pb")})
	finished, _ := s.Create(store.Meta{DocID: "c"}, [][]byte{[]byte("pc")})
	_ = s.MarkDone(finished.ID, "already", "m")

	s2, _ := store.New(dir) // fresh process
	fc := &fakeConv{out: "recovered"}
	w := New(s2, fc, quiet())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, s2, a.ID, store.StatusDone)
	waitStatus(t, s2, b.ID, store.StatusDone)
	if n, _ := s2.Note(finished.ID); n != "already" {
		t.Fatalf("done share must not be reconverted, note=%q", n)
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.calls) != 2 {
		t.Fatalf("converter called %d times, want 2", len(fc.calls))
	}
}

func TestEnqueueDoesNotBlockWhileConverting(t *testing.T) {
	s, _ := store.New(t.TempDir())
	release := make(chan struct{})
	slow := &blockingConv{release: release}
	w := New(s, slow, quiet())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = w.Start(ctx)
	defer close(release)
	// In-flight work now finishes after cancel, so wait for it before TempDir cleanup.
	t.Cleanup(func() { w.Shutdown(2 * time.Second) })
	finished := make(chan struct{})
	go func() {
		// One real share keeps the worker busy; the rest are synthetic ids so the
		// loop does not pay an fsync per share (the store now flushes to disk).
		m, _ := s.Create(store.Meta{DocID: "d"}, [][]byte{[]byte("p")})
		w.Enqueue(m.ID)
		for i := 0; i < 2000; i++ {
			w.Enqueue(fmt.Sprintf("synthetic-%d", i))
		}
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("Enqueue blocked while the worker was busy")
	}
}

type blockingConv struct{ release chan struct{} }

func (b *blockingConv) Model() string { return "m" }
func (b *blockingConv) Convert(ctx context.Context, _ [][]byte) (string, error) {
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return "x", nil
}
