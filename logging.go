package main

// Usage & admin statistics collector — the "passive audit trail".
//
// Design (per architecture doc): no database. Events accumulate in RAM
// and are flushed to R2 as JSON-lines files, one per day. Data is
// explicitly NON-CRITICAL: if the server crashes between flushes, the
// un-flushed events are lost — an accepted trade-off for a zero-persistence
// design. The flush interval (default 1 hour, STATS_FLUSH_MINUTES override)
// bounds the loss window.
//
// Events recorded:
//   admin_login / admin_login_failed / admin_logout — from auth handlers
//   admin_request  — every USER-ORIENTATED authenticated API call (the
//                    structured, persistent version of the [ADMIN]
//                    console log lines). Operational endpoints are
//                    excluded via statsSkipPaths (see below).
//   track_download — public usage: what was requested, from which IP,
//                    and when (per requirements: no personal tracking
//                    beyond IP; catalog fetches are deliberately NOT
//                    logged — they fire on every page load)
//
// Privacy note: IP addresses are stored. This was an explicit design
// decision for usage statistics. Credentials are never recorded —
// failed logins store the IP only.

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"os"
	"strconv"
	"sync"
	"time"
)

// Event is one statistics record. Fields are omitempty so public events
// (track_download) don't carry empty admin fields and vice versa.
type Event struct {
	Time   time.Time `json:"time"`
	Kind   string    `json:"kind"`
	UserID string    `json:"user,omitempty"`
	Method string    `json:"method,omitempty"`
	Path   string    `json:"path,omitempty"`
	File   string    `json:"file,omitempty"` // track filename (downloads)
	IP     string    `json:"ip,omitempty"`
}

// statsSkipPaths lists OPERATIONAL endpoints excluded from the
// admin_request event stream. Crawl orchestration is machine overhead,
// not an administrator action: start-crawl fires once per rebuild, but
// crawl-status is polled repeatedly by the UI watching the job — logging
// it would flood the stats with identical GETs that carry no insight.
// Deliberately a hardcoded set: these are server-internal operational
// routes, not user-facing features that change often.
var statsSkipPaths = map[string]bool{
	"/api/start-crawl":  true,
	"/api/crawl-status": true,
}

const statsBufferCap = 10000 // hard cap: bounds memory if R2 is unreachable for a long period

// statsCollector buffers events in memory between flushes.
type statsCollector struct {
	mu     sync.Mutex
	buffer []Event
}

var stats = &statsCollector{}

// Record appends an event (timestamped here, UTC).
func (s *statsCollector) Record(e Event) {
	e.Time = time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.buffer) >= statsBufferCap {
		// Buffer full (R2 down across many flushes?) — drop the oldest.
		// Non-critical data; a bounded loss beats unbounded memory.
		s.buffer = s.buffer[1:]
	}
	s.buffer = append(s.buffer, e)
}

// RecordAdminRequest records an authenticated API call unless it targets
// an operational endpoint excluded from stats (see statsSkipPaths).
// authMiddleware calls this for every request that passes the gate.
func (s *statsCollector) RecordAdminRequest(userID, method, path, ip string) {
	if statsSkipPaths[path] {
		return // operational overhead — not a user/admin action
	}
	s.Record(Event{
		Kind:   "admin_request",
		UserID: userID,
		Method: method,
		Path:   path,
		IP:     ip,
	})
}

// take removes and returns the buffered events.
func (s *statsCollector) take() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.buffer
	s.buffer = nil
	return out
}

// restore re-queues events after a failed flush (oldest first).
func (s *statsCollector) restore(events []Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buffer = append(events, s.buffer...)
}

// Flush writes all buffered events to R2, appending to today's file.
// Safe to call at any time; exported so the shutdown path can force it.
// Returns true if the batch was written (or there was nothing to write);
// false if the write failed and events were requeued.
func (s *statsCollector) Flush(ctx context.Context) bool {
	events := s.take()
	if len(events) == 0 {
		return true // nothing buffered = nothing failed
	}

	// Serialize the batch to JSON lines.
	var batch bytes.Buffer
	enc := json.NewEncoder(&batch)
	for _, e := range events {
		if err := enc.Encode(e); err != nil {
			log.Printf("[STATS] Warning: could not encode event: %v", err)
		}
	}

	// NOTE: the key uses a .json extension even though the content is
	// JSON-lines. The bucket crawler whitelists ONLY .mp3 files, so a
	// stats file can never become a catalog track — but keep the
	// extension/prefix recognizable in case that whitelist ever changes.
	key := "stats/usage-" + time.Now().UTC().Format("2006-01-02") + ".json"

	// Append to today's file if a previous flush already created it.
	var payload []byte
	if existing, err := storageClient.GetObjectBytes(ctx, key); err == nil && len(existing) > 0 {
		payload = make([]byte, 0, len(existing)+batch.Len())
		payload = append(payload, existing...)
		if payload[len(payload)-1] != '\n' {
			payload = append(payload, '\n')
		}
	}
	payload = append(payload, batch.Bytes()...)

	if err := storageClient.PutObjectBytes(ctx, key, payload); err != nil {
		// Requeue so the data survives until the next flush attempt.
		// (At shutdown there is no next attempt — the requeue is futile
		// but harmless there, and keeps this one code path for both the
		// ticker and shutdown callers.)
		log.Printf("[STATS] Flush FAILED (%d events requeued): %v", len(events), err)
		s.restore(events)
		return false
	}
	log.Printf("[STATS] Flushed %d events to %s", len(events), key)
	return true
}

// StartStatsLoop runs the periodic flush until ctx is cancelled, then
// performs one final best-effort flush (Render sends SIGTERM on deploys).
// Returns true if the final flush succeeded; false if it failed (events
// requeued — and lost at process exit, accepted for non-critical data).
// The periodic ticker flushes' outcomes are not reported — only the
// shutdown flush matters to the caller.
func StartStatsLoop(ctx context.Context) bool {
	interval := time.Hour
	if m := os.Getenv("STATS_FLUSH_MINUTES"); m != "" {
		if n, err := strconv.Atoi(m); err == nil && n > 0 {
			interval = time.Duration(n) * time.Minute
		}
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			stats.Flush(context.Background())
		case <-ctx.Done():
			// Graceful shutdown: flush what we have, but WITH A TIMEOUT.
			// If R2 or the network hangs, the flush gives up after the
			// deadline so the process can always exit.
			flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return stats.Flush(flushCtx)
		}
	}
}
