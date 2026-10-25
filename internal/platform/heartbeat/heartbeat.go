// Package heartbeat records when each background job last ran, for the health page.
package heartbeat

import (
	"sort"
	"sync"
	"time"
)

var (
	mu   sync.Mutex
	last = map[string]time.Time{}
)

// Beat marks a job as having just run.
func Beat(name string) {
	mu.Lock()
	last[name] = time.Now()
	mu.Unlock()
}

type Entry struct {
	Name string    `json:"name"`
	At   time.Time `json:"last_run"`
}

// All returns every job that has run in this process, sorted by name.
func All() []Entry {
	mu.Lock()
	defer mu.Unlock()
	out := make([]Entry, 0, len(last))
	for k, v := range last {
		out = append(out, Entry{k, v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
