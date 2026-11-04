package worker

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gurghet/rmfakecloud-share/internal/convert"
	"github.com/gurghet/rmfakecloud-share/internal/store"
)

type scriptedConv struct {
	mu    sync.Mutex
	calls int
	fn    func(call int) (string, error)
}

func (s *scriptedConv) Model() string { return "scripted" }
func (s *scriptedConv) Convert(ctx context.Context, _ [][]byte) (string, error) {
	s.mu.Lock()
	s.calls++
	n := s.calls
	s.mu.Unlock()
	return s.fn(n)
}
func (s *scriptedConv) count() int { s.mu.Lock(); defer s.mu.Unlock(); return s.calls }

func fast(w *Worker) *Worker {
	w.Backoff = func(int) time.Duration { return 10 * time.Millisecond }
	w.RetryDelay = 5 * time.Millisecond
	w.RescanEvery = 20 * time.Millisecond
	return w
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestTransientErrorStaysPendingAndIsRetried(t *testing.T) {
	s, _ := store.New(t.TempDir())
	var healthy atomic.Bool
	sc := &scriptedConv{fn: func(int) (string, error) {
		if healthy.Load() {
			return "# ok", nil
		}
		return "", errors.New("dial tcp: connection refused")
	}}
	w := fast(New(s, sc, quiet()))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = w.Start(ctx)
	m, _ := s.Create(store.Meta{DocID: "d"}, [][]byte{[]byte("p")})
	w.Enqueue(m.ID)
	waitFor(t, "three attempts", func() bool { return sc.count() >= 3 })
	if got, _ := s.Get(m.ID); got.Status != store.StatusPending {
		t.Fatalf("transient errors must leave the share pending, got %s", got.Status)
	}
	healthy.Store(true)
	waitStatus(t, s, m.ID, store.StatusDone)
}

func TestPermanentErrorFailsImmediatelyWithoutRetry(t *testing.T) {
	s, _ := store.New(t.TempDir())
	sc := &scriptedConv{fn: func(int) (string, error) { return "", convert.Permanent(errors.New("400 bad")) }}
	w := fast(New(s, sc, quiet()))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = w.Start(ctx)
	m, _ := s.Create(store.Meta{DocID: "d"}, [][]byte{[]byte("p")})
	w.Enqueue(m.ID)
	waitStatus(t, s, m.ID, store.StatusFailed)
	time.Sleep(100 * time.Millisecond)
	if sc.count() != 1 {
		t.Fatalf("permanent error was attempted %d times, want 1", sc.count())
	}
}

func TestTransientShareSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	s, _ := store.New(dir)
	down := &scriptedConv{fn: func(int) (string, error) { return "", errors.New("timeout") }}
	w := fast(New(s, down, quiet()))
	ctx, cancel := context.WithCancel(context.Background())
	_ = w.Start(ctx)
	m, _ := s.Create(store.Meta{DocID: "d"}, [][]byte{[]byte("p")})
	w.Enqueue(m.ID)
	waitFor(t, "first attempt", func() bool { return down.count() >= 1 })
	cancel()
	w.Shutdown(time.Second)

	s2, _ := store.New(dir)
	up := &scriptedConv{fn: func(int) (string, error) { return "recovered", nil }}
	w2 := fast(New(s2, up, quiet()))
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	if err := w2.Start(ctx2); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, s2, m.ID, store.StatusDone)
}

func TestDefaultBackoffIsCappedExponential(t *testing.T) {
	prev := time.Duration(0)
	for a := 1; a <= 40; a++ {
		d := defaultBackoff(a)
		if d < prev {
			t.Fatalf("backoff decreased at attempt %d: %v < %v", a, d, prev)
		}
		if d > 15*time.Minute {
			t.Fatalf("backoff %v at attempt %d exceeds the cap", d, a)
		}
		prev = d
	}
	if defaultBackoff(1) < 10*time.Second {
		t.Fatalf("first retry after %v is too eager for a rate-limited API", defaultBackoff(1))
	}
	if defaultBackoff(2) <= defaultBackoff(1) || defaultBackoff(40) != 15*time.Minute {
		t.Fatalf("backoff must grow then plateau at 15m: %v %v %v", defaultBackoff(1), defaultBackoff(2), defaultBackoff(40))
	}
}

func TestRequeuedFailedShareIsPickedUpWhileRunning(t *testing.T) {
	s, _ := store.New(t.TempDir())
	var mode atomic.Int32 // 0 = permanent failure, 1 = works
	sc := &scriptedConv{fn: func(int) (string, error) {
		if mode.Load() == 0 {
			return "", convert.Permanent(errors.New("nope"))
		}
		return "fixed", nil
	}}
	w := fast(New(s, sc, quiet()))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = w.Start(ctx)
	m, _ := s.Create(store.Meta{DocID: "d"}, [][]byte{[]byte("p")})
	w.Enqueue(m.ID)
	waitStatus(t, s, m.ID, store.StatusFailed)
	mode.Store(1)
	if err := s.Requeue(m.ID); err != nil { // what `rmshare -retry <id>` does
		t.Fatal(err)
	}
	waitStatus(t, s, m.ID, store.StatusDone)
}

type flakyStore struct {
	*store.Store
	failDone  atomic.Int32 // number of MarkDone calls still to fail; <0 = always
	doneCalls atomic.Int32
}

func (f *flakyStore) MarkDone(id, note, model string) error {
	f.doneCalls.Add(1)
	n := f.failDone.Load()
	if n < 0 {
		return errors.New("disk full")
	}
	if n > 0 {
		f.failDone.Add(-1)
		return fmt.Errorf("disk hiccup")
	}
	return f.Store.MarkDone(id, note, model)
}

func TestMarkDoneFailureIsRetriedWithoutReconverting(t *testing.T) {
	base, _ := store.New(t.TempDir())
	fs := &flakyStore{Store: base}
	fs.failDone.Store(2)
	sc := &scriptedConv{fn: func(int) (string, error) { return "note", nil }}
	w := fast(New(fs, sc, quiet()))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = w.Start(ctx)
	m, _ := base.Create(store.Meta{DocID: "d"}, [][]byte{[]byte("p")})
	w.Enqueue(m.ID)
	waitStatus(t, base, m.ID, store.StatusDone)
	if sc.count() != 1 {
		t.Fatalf("converter ran %d times; a save failure must not trigger paid reconversion", sc.count())
	}
}

func TestPersistentMarkDoneFailureMarksFailed(t *testing.T) {
	base, _ := store.New(t.TempDir())
	fs := &flakyStore{Store: base}
	fs.failDone.Store(-1)
	sc := &scriptedConv{fn: func(int) (string, error) { return "note", nil }}
	w := fast(New(fs, sc, quiet()))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = w.Start(ctx)
	m, _ := base.Create(store.Meta{DocID: "d"}, [][]byte{[]byte("p")})
	w.Enqueue(m.ID)
	waitStatus(t, base, m.ID, store.StatusFailed)
	if sc.count() != 1 {
		t.Fatalf("converter ran %d times", sc.count())
	}
}

func TestShutdownLetsInFlightConversionFinish(t *testing.T) {
	s, _ := store.New(t.TempDir())
	started := make(chan struct{})
	release := make(chan struct{})
	sc := &scriptedConv{fn: func(int) (string, error) {
		close(started)
		<-release
		return "finished", nil
	}}
	w := fast(New(s, sc, quiet()))
	ctx, cancel := context.WithCancel(context.Background())
	_ = w.Start(ctx)
	m, _ := s.Create(store.Meta{DocID: "d"}, [][]byte{[]byte("p")})
	w.Enqueue(m.ID)
	<-started
	cancel() // SIGTERM arrives mid-conversion
	go func() { time.Sleep(100 * time.Millisecond); close(release) }()
	w.Shutdown(3 * time.Second)
	if got, _ := s.Get(m.ID); got.Status != store.StatusDone {
		t.Fatalf("in-flight conversion must finish within the grace period, status %s", got.Status)
	}
}

func TestShutdownCancelsAfterGraceAndLeavesSharePending(t *testing.T) {
	s, _ := store.New(t.TempDir())
	started := make(chan struct{})
	sc := &scriptedConv{}
	sc.fn = func(int) (string, error) { return "", nil }
	blocking := &ctxConv{started: started}
	w := fast(New(s, blocking, quiet()))
	ctx, cancel := context.WithCancel(context.Background())
	_ = w.Start(ctx)
	m, _ := s.Create(store.Meta{DocID: "d"}, [][]byte{[]byte("p")})
	w.Enqueue(m.ID)
	<-started
	cancel()
	t0 := time.Now()
	w.Shutdown(150 * time.Millisecond)
	if el := time.Since(t0); el < 100*time.Millisecond || el > 2*time.Second {
		t.Fatalf("Shutdown took %v, want about the grace period", el)
	}
	if !blocking.cancelled.Load() {
		t.Fatal("conversion context must be cancelled once the grace period ends")
	}
	if got, _ := s.Get(m.ID); got.Status != store.StatusPending {
		t.Fatalf("interrupted share must stay pending for the next start, got %s", got.Status)
	}
}

type ctxConv struct {
	started   chan struct{}
	cancelled atomic.Bool
}

func (c *ctxConv) Model() string { return "m" }
func (c *ctxConv) Convert(ctx context.Context, _ [][]byte) (string, error) {
	close(c.started)
	<-ctx.Done()
	c.cancelled.Store(true)
	return "", ctx.Err()
}
