// Package worker runs share conversions in the background.
package worker

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/gurghet/rmfakecloud-share/internal/convert"
	"github.com/gurghet/rmfakecloud-share/internal/store"
)

// conversionTimeout bounds one share's conversion.
const conversionTimeout = 15 * time.Minute

// Store is the persistence the worker needs. *store.Store implements it.
type Store interface {
	Get(id string) (store.Meta, error)
	Pages(id string) ([][]byte, error)
	MarkDone(id, note, model string) error
	MarkFailed(id, msg string) error
	Pending() ([]string, error)
}

// Worker converts queued shares one at a time, which keeps CPU and memory low
// on small hardware and stays within API rate limits.
//
// Transient errors (network, timeouts, 408/429/5xx) leave the share pending and
// retry it with capped exponential backoff. Permanent errors mark it failed. A
// failed share goes back to pending through Store.Requeue (rmshare -retry), and
// the periodic rescan then picks it up; page visitors can never trigger work.
type Worker struct {
	store Store
	conv  convert.Converter
	log   *slog.Logger

	// Backoff returns the wait before retry number attempt (1-based) after a
	// transient error. RetryDelay is the wait between attempts to save a result.
	// RescanEvery is how often pending shares missing from the queue are picked
	// up again. Set them before Start.
	Backoff     func(attempt int) time.Duration
	RetryDelay  time.Duration
	RescanEvery time.Duration

	runCtx    context.Context // cancelled by Shutdown once the grace period is over
	runCancel context.CancelFunc
	done      chan struct{} // closed when the processing loop has exited

	mu       sync.Mutex
	queue    []string
	wake     chan struct{}
	queued   map[string]bool // queued, running, or waiting for a backoff timer
	attempts map[string]int
}

// New creates a worker.
func New(s Store, c convert.Converter, log *slog.Logger) *Worker {
	ctx, cancel := context.WithCancel(context.Background())
	return &Worker{store: s, conv: c, log: log, wake: make(chan struct{}, 1), queued: map[string]bool{}, attempts: map[string]int{},
		Backoff: defaultBackoff, RetryDelay: 2 * time.Second, RescanEvery: time.Minute,
		runCtx: ctx, runCancel: cancel, done: make(chan struct{})}
}

const maxBackoff = 15 * time.Minute

// defaultBackoff doubles from 30 s up to a 15 minute cap.
func defaultBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 6 {
		return maxBackoff
	}
	d := 30 * time.Second << (attempt - 1)
	if d > maxBackoff {
		return maxBackoff
	}
	return d
}

// Enqueue schedules a share for conversion. It never blocks.
func (w *Worker) Enqueue(id string) {
	w.mu.Lock()
	if !w.queued[id] {
		w.queued[id] = true
		w.queue = append(w.queue, id)
	}
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *Worker) next() (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.queue) == 0 {
		return "", false
	}
	id := w.queue[0]
	w.queue = w.queue[1:]
	return id, true
}

func (w *Worker) enqueuePending() error {
	pending, err := w.store.Pending()
	if err != nil {
		return err
	}
	for _, id := range pending {
		w.Enqueue(id)
	}
	return nil
}

// Start enqueues pending shares and processes the queue until ctx is done.
// Cancelling ctx stops taking new work but lets the conversion in flight
// finish; call Shutdown to wait for it. Start returns once the goroutines run.
func (w *Worker) Start(ctx context.Context) error {
	if err := w.enqueuePending(); err != nil {
		return err
	}
	w.mu.Lock()
	n := len(w.queue)
	w.mu.Unlock()
	if n > 0 {
		w.log.Info("re-enqueued pending shares", "count", n)
	}
	go w.loop(ctx)
	go w.rescan(ctx)
	return nil
}

func (w *Worker) rescan(ctx context.Context) {
	t := time.NewTicker(w.RescanEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := w.enqueuePending(); err != nil {
				w.log.Error("rescanning pending shares failed", "err", err)
			}
		}
	}
}

// Shutdown waits up to grace for the conversion in flight, then cancels it.
// Cancel the context given to Start first. An interrupted share stays pending
// and is converted on the next start.
func (w *Worker) Shutdown(grace time.Duration) {
	select {
	case <-w.done:
		return
	case <-time.After(grace):
	}
	w.log.Warn("shutdown grace period over: cancelling the conversion in flight")
	w.runCancel()
	<-w.done
}

func (w *Worker) loop(ctx context.Context) {
	defer close(w.done)
	for {
		if ctx.Err() != nil {
			return
		}
		id, ok := w.next()
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-w.wake:
				continue
			}
		}
		if !w.process(id) {
			w.mu.Lock()
			delete(w.queued, id)
			w.mu.Unlock()
		}
	}
}

// process converts one share. It returns true when a retry timer now owns the
// share's place in the queued set.
func (w *Worker) process(id string) (retryScheduled bool) {
	meta, err := w.store.Get(id)
	if err != nil || meta.Status != store.StatusPending {
		return false
	}
	pages, err := w.store.Pages(id)
	if err != nil {
		w.fail(id, err)
		return false
	}
	cctx, cancel := context.WithTimeout(w.runCtx, conversionTimeout)
	defer cancel()
	start := time.Now()
	note, err := w.conv.Convert(cctx, pages)
	if err != nil {
		if w.runCtx.Err() != nil {
			return false // interrupted by shutdown: stays pending
		}
		if convert.IsPermanent(err) {
			w.fail(id, err)
			return false
		}
		return w.retryLater(id, err)
	}
	w.mu.Lock()
	delete(w.attempts, id)
	w.mu.Unlock()
	w.save(id, note, len(pages), time.Since(start))
	return false
}

func (w *Worker) retryLater(id string, err error) bool {
	w.mu.Lock()
	w.attempts[id]++
	n := w.attempts[id]
	w.mu.Unlock()
	d := w.Backoff(n)
	w.log.Warn("conversion failed temporarily, will retry", "share", id, "attempt", n, "retry_in", d, "err", err)
	time.AfterFunc(d, func() {
		w.mu.Lock()
		delete(w.queued, id)
		w.mu.Unlock()
		w.Enqueue(id)
	})
	return true
}

// save stores the result. A save failure must not trigger another paid
// conversion, so it is retried here and the share is marked failed if it keeps failing.
func (w *Worker) save(id, note string, pages int, took time.Duration) {
	var err error
	for i := 0; i < 5; i++ {
		if err = w.store.MarkDone(id, note, w.conv.Model()); err == nil {
			w.log.Info("conversion done", "share", id, "pages", pages, "took", took.Round(time.Millisecond))
			return
		}
		w.log.Error("saving note failed", "share", id, "attempt", i+1, "err", err)
		time.Sleep(w.RetryDelay)
	}
	w.fail(id, fmt.Errorf("saving the note failed: %w", err))
}

func (w *Worker) fail(id string, err error) {
	w.log.Error("conversion failed", "share", id, "err", err)
	if e := w.store.MarkFailed(id, err.Error()); e != nil {
		w.log.Error("recording failure failed", "share", id, "err", e)
	}
}
