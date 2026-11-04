package server

import (
	"sync"
	"time"

	"github.com/gurghet/rmfakecloud-share/internal/store"
)

const quotaWindow = 24 * time.Hour

type quotaEntry struct {
	at    time.Time
	pages int
}

// quota limits the pages each user may have converted in a rolling 24 hours.
// It is rebuilt from the stored shares at startup.
type quota struct {
	limit   int
	mu      sync.Mutex
	entries map[string][]quotaEntry
}

func newQuota(limit int) *quota { return &quota{limit: limit, entries: map[string][]quotaEntry{}} }

func (q *quota) load(metas []store.Meta) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, m := range metas {
		if m.User != "" {
			q.entries[m.User] = append(q.entries[m.User], quotaEntry{m.Created, m.PageCount})
		}
	}
}

// reserve counts pages for user at now. It returns an undo function, or ok=false
// with the time until the oldest counted share leaves the window.
func (q *quota) reserve(user string, pages int, now time.Time) (undo func(), retryAfter time.Duration, ok bool) {
	if q.limit <= 0 {
		return func() {}, 0, true
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	live := q.entries[user][:0]
	used := 0
	for _, e := range q.entries[user] {
		if now.Sub(e.at) < quotaWindow {
			live = append(live, e)
			used += e.pages
		}
	}
	q.entries[user] = live
	if used+pages > q.limit {
		wait := quotaWindow
		if len(live) > 0 {
			wait = live[0].at.Add(quotaWindow).Sub(now)
		}
		return nil, wait, false
	}
	e := quotaEntry{now, pages}
	q.entries[user] = append(q.entries[user], e)
	return func() {
		q.mu.Lock()
		defer q.mu.Unlock()
		list := q.entries[user]
		for i := range list {
			if list[i] == e {
				q.entries[user] = append(list[:i], list[i+1:]...)
				return
			}
		}
	}, 0, true
}
