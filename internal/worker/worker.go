// Package worker runs share conversions in the background.
package worker

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/gurghet/rmfakecloud-share/internal/convert"
	"github.com/gurghet/rmfakecloud-share/internal/store"
)

// conversionTimeout bounds one share's conversion.
const conversionTimeout = 15 * time.Minute

// Worker converts queued shares one at a time, which keeps CPU and memory low
// on small hardware and stays within API rate limits.
type Worker struct {
	store *store.Store
	conv  convert.Converter
	log   *slog.Logger

	mu     sync.Mutex
	queue  []string
	wake   chan struct{}
	queued map[string]bool
}

// New creates a worker.
func New(s *store.Store, c convert.Converter, log *slog.Logger) *Worker {
	return &Worker{store: s, conv: c, log: log, wake: make(chan struct{}, 1), queued: map[string]bool{}}
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

// Start re-enqueues pending shares and processes the queue until ctx is done.
// It returns once the goroutine is running.
func (w *Worker) Start(ctx context.Context) error {
	pending, err := w.store.Pending()
	if err != nil {
		return err
	}
	for _, id := range pending {
		w.Enqueue(id)
	}
	if len(pending) > 0 {
		w.log.Info("re-enqueued pending shares", "count", len(pending))
	}
	go w.loop(ctx)
	return nil
}

func (w *Worker) loop(ctx context.Context) {
	for {
		id, ok := w.next()
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-w.wake:
				continue
			}
		}
		if ctx.Err() != nil {
			return
		}
		w.process(ctx, id)
		w.mu.Lock()
		delete(w.queued, id)
		w.mu.Unlock()
	}
}

func (w *Worker) process(ctx context.Context, id string) {
	meta, err := w.store.Get(id)
	if err != nil || meta.Status != store.StatusPending {
		return
	}
	pages, err := w.store.Pages(id)
	if err != nil {
		w.fail(id, err)
		return
	}
	cctx, cancel := context.WithTimeout(ctx, conversionTimeout)
	defer cancel()
	start := time.Now()
	note, err := w.conv.Convert(cctx, pages)
	if err != nil {
		if ctx.Err() != nil {
			return // shutting down: stays pending, retried on next start
		}
		w.fail(id, err)
		return
	}
	if err := w.store.MarkDone(id, note, w.conv.Model()); err != nil {
		w.log.Error("saving note failed", "share", id, "err", err)
		return
	}
	w.log.Info("conversion done", "share", id, "pages", len(pages), "took", time.Since(start).Round(time.Millisecond))
}

func (w *Worker) fail(id string, err error) {
	w.log.Error("conversion failed", "share", id, "err", err)
	if e := w.store.MarkFailed(id, err.Error()); e != nil {
		w.log.Error("recording failure failed", "share", id, "err", e)
	}
}
