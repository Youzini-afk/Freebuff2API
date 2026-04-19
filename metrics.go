package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	metricsBucketCount = 24 * 60 // 24h at 1-minute resolution
	metricsFlushPeriod = 30 * time.Second
)

// Metrics aggregates request outcomes across all tokens with a 1-minute
// ring buffer (last 24h) and cumulative per-token counters that are
// persisted to disk.
type Metrics struct {
	path string

	mu       sync.Mutex
	buckets  []minuteBucket
	tokens   map[string]*tokenAggregate
	global   globalAggregate
	dirty    bool
	stopCh   chan struct{}
	stopOnce sync.Once
}

type minuteBucket struct {
	// minuteUnix is the minute-start unix timestamp the counts apply to.
	// Zero means the slot is empty.
	minuteUnix int64
	total      int64
	success    int64
	errors     int64
	perToken   map[string]*bucketTokenStats
}

type bucketTokenStats struct {
	Total   int64 `json:"total"`
	Success int64 `json:"success"`
	Errors  int64 `json:"errors"`
}

// tokenAggregate tracks cumulative counters for a single token.
type tokenAggregate struct {
	TokenID       string    `json:"token_id"`
	Total         int64     `json:"total"`
	Success       int64     `json:"success"`
	Errors        int64     `json:"errors"`
	LastRequestAt time.Time `json:"last_request_at,omitempty"`
	LastSuccessAt time.Time `json:"last_success_at,omitempty"`
	LastErrorAt   time.Time `json:"last_error_at,omitempty"`
	LastErrorMsg  string    `json:"last_error_msg,omitempty"`
	LatencySumMs  int64     `json:"latency_sum_ms"`
	LatencyCount  int64     `json:"latency_count"`
}

type globalAggregate struct {
	Total        int64 `json:"total"`
	Success      int64 `json:"success"`
	Errors       int64 `json:"errors"`
	LatencySumMs int64 `json:"latency_sum_ms"`
	LatencyCount int64 `json:"latency_count"`
}

type persistedMetrics struct {
	Version int                       `json:"version"`
	Global  globalAggregate           `json:"global"`
	Tokens  map[string]*tokenAggregate `json:"tokens"`
}

// MinutePoint is one point on a time-series curve returned to the UI.
type MinutePoint struct {
	MinuteUnix int64 `json:"minute_unix"`
	Total      int64 `json:"total"`
	Success    int64 `json:"success"`
	Errors     int64 `json:"errors"`
}

// TokenStats is the public view of cumulative counters for a token.
type TokenStats struct {
	TokenID       string    `json:"token_id"`
	Total         int64     `json:"total"`
	Success       int64     `json:"success"`
	Errors        int64     `json:"errors"`
	LastRequestAt time.Time `json:"last_request_at,omitempty"`
	LastSuccessAt time.Time `json:"last_success_at,omitempty"`
	LastErrorAt   time.Time `json:"last_error_at,omitempty"`
	LastErrorMsg  string    `json:"last_error_msg,omitempty"`
	AvgLatencyMs  int64     `json:"avg_latency_ms"`
}

// OverviewStats summarises global counters for the dashboard.
type OverviewStats struct {
	Total        int64 `json:"total"`
	Success      int64 `json:"success"`
	Errors       int64 `json:"errors"`
	AvgLatencyMs int64 `json:"avg_latency_ms"`
	Last1mTotal  int64 `json:"last_1m_total"`
	Last5mTotal  int64 `json:"last_5m_total"`
	Last1hTotal  int64 `json:"last_1h_total"`
	Last24hTotal int64 `json:"last_24h_total"`
}

// NewMetrics constructs a Metrics instance, loading any persisted cumulative
// counters from path. The path's parent directory is created if missing.
// Passing an empty path disables disk persistence.
func NewMetrics(path string) (*Metrics, error) {
	m := &Metrics{
		path:    strings.TrimSpace(path),
		buckets: make([]minuteBucket, metricsBucketCount),
		tokens:  make(map[string]*tokenAggregate),
		stopCh:  make(chan struct{}),
	}

	if m.path != "" {
		if dir := filepath.Dir(m.path); dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("create metrics dir: %w", err)
			}
		}
		if err := m.loadFromDisk(); err != nil {
			return nil, err
		}
	}

	return m, nil
}

func (m *Metrics) loadFromDisk() error {
	data, err := os.ReadFile(m.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read metrics: %w", err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil
	}
	var parsed persistedMetrics
	if err := json.Unmarshal(data, &parsed); err != nil {
		return fmt.Errorf("parse metrics: %w", err)
	}
	m.global = parsed.Global
	if parsed.Tokens != nil {
		m.tokens = parsed.Tokens
	}
	return nil
}

// StartBackgroundFlush periodically persists cumulative counters to disk.
// The goroutine exits when Close is called.
func (m *Metrics) StartBackgroundFlush() {
	if m.path == "" {
		return
	}
	go func() {
		ticker := time.NewTicker(metricsFlushPeriod)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				_ = m.Flush()
			case <-m.stopCh:
				return
			}
		}
	}()
}

// Close stops the background flusher and persists once more.
func (m *Metrics) Close() error {
	m.stopOnce.Do(func() { close(m.stopCh) })
	return m.Flush()
}

// Flush persists cumulative counters to disk if the file path is configured.
func (m *Metrics) Flush() error {
	if m.path == "" {
		return nil
	}

	m.mu.Lock()
	if !m.dirty {
		m.mu.Unlock()
		return nil
	}
	snapshot := persistedMetrics{
		Version: 1,
		Global:  m.global,
		Tokens:  make(map[string]*tokenAggregate, len(m.tokens)),
	}
	for id, agg := range m.tokens {
		copied := *agg
		snapshot.Tokens[id] = &copied
	}
	m.dirty = false
	m.mu.Unlock()

	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return fmt.Errorf("encode metrics: %w", err)
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write metrics: %w", err)
	}
	if err := os.Rename(tmp, m.path); err != nil {
		return fmt.Errorf("commit metrics: %w", err)
	}
	return nil
}

// Record registers the outcome of a single upstream request.
func (m *Metrics) Record(tokenID string, success bool, statusCode int, errMsg string, latency time.Duration) {
	now := time.Now()
	minuteUnix := now.Unix() - (now.Unix() % 60)
	latencyMs := latency.Milliseconds()

	m.mu.Lock()
	defer m.mu.Unlock()

	bucket := m.bucketForLocked(minuteUnix)
	bucket.total++
	m.global.Total++
	if success {
		bucket.success++
		m.global.Success++
	} else {
		bucket.errors++
		m.global.Errors++
	}
	if latencyMs > 0 {
		m.global.LatencySumMs += latencyMs
		m.global.LatencyCount++
	}

	if tokenID != "" {
		stats := bucket.perToken[tokenID]
		if stats == nil {
			stats = &bucketTokenStats{}
			bucket.perToken[tokenID] = stats
		}
		stats.Total++
		if success {
			stats.Success++
		} else {
			stats.Errors++
		}

		agg := m.tokens[tokenID]
		if agg == nil {
			agg = &tokenAggregate{TokenID: tokenID}
			m.tokens[tokenID] = agg
		}
		agg.Total++
		agg.LastRequestAt = now.UTC()
		if success {
			agg.Success++
			agg.LastSuccessAt = now.UTC()
		} else {
			agg.Errors++
			agg.LastErrorAt = now.UTC()
			if errMsg != "" {
				agg.LastErrorMsg = truncateError(errMsg)
			}
		}
		if latencyMs > 0 {
			agg.LatencySumMs += latencyMs
			agg.LatencyCount++
		}
	}

	m.dirty = true
}

// bucketForLocked returns the ring-buffer slot for the given minute,
// resetting the slot if it belongs to an older minute.
func (m *Metrics) bucketForLocked(minuteUnix int64) *minuteBucket {
	index := int((minuteUnix / 60) % int64(metricsBucketCount))
	if index < 0 {
		index += metricsBucketCount
	}
	bucket := &m.buckets[index]
	if bucket.minuteUnix != minuteUnix {
		bucket.minuteUnix = minuteUnix
		bucket.total = 0
		bucket.success = 0
		bucket.errors = 0
		bucket.perToken = make(map[string]*bucketTokenStats)
	}
	if bucket.perToken == nil {
		bucket.perToken = make(map[string]*bucketTokenStats)
	}
	return bucket
}

// Series returns the last n minutes of traffic, oldest first. Missing
// minutes are returned as zero-valued points so the UI chart is continuous.
func (m *Metrics) Series(minutes int) []MinutePoint {
	if minutes <= 0 {
		return nil
	}
	if minutes > metricsBucketCount {
		minutes = metricsBucketCount
	}
	return m.seriesFiltered(minutes, "")
}

// SeriesForToken is like Series but filtered to a single token ID.
func (m *Metrics) SeriesForToken(tokenID string, minutes int) []MinutePoint {
	if minutes <= 0 {
		return nil
	}
	if minutes > metricsBucketCount {
		minutes = metricsBucketCount
	}
	return m.seriesFiltered(minutes, tokenID)
}

func (m *Metrics) seriesFiltered(minutes int, tokenID string) []MinutePoint {
	now := time.Now().Unix()
	currentMinute := now - (now % 60)

	m.mu.Lock()
	defer m.mu.Unlock()

	points := make([]MinutePoint, 0, minutes)
	for i := minutes - 1; i >= 0; i-- {
		minuteUnix := currentMinute - int64(i*60)
		index := int((minuteUnix / 60) % int64(metricsBucketCount))
		if index < 0 {
			index += metricsBucketCount
		}
		bucket := m.buckets[index]
		point := MinutePoint{MinuteUnix: minuteUnix}
		if bucket.minuteUnix == minuteUnix {
			if tokenID == "" {
				point.Total = bucket.total
				point.Success = bucket.success
				point.Errors = bucket.errors
			} else if stats, ok := bucket.perToken[tokenID]; ok {
				point.Total = stats.Total
				point.Success = stats.Success
				point.Errors = stats.Errors
			}
		}
		points = append(points, point)
	}
	return points
}

// Overview returns dashboard counters including recent activity windows.
func (m *Metrics) Overview() OverviewStats {
	series := m.Series(24 * 60)

	m.mu.Lock()
	defer m.mu.Unlock()

	stats := OverviewStats{
		Total:   m.global.Total,
		Success: m.global.Success,
		Errors:  m.global.Errors,
	}
	if m.global.LatencyCount > 0 {
		stats.AvgLatencyMs = m.global.LatencySumMs / m.global.LatencyCount
	}
	for i, p := range series {
		// series is oldest→newest, so the last index is the current minute.
		offsetFromNow := len(series) - 1 - i
		if offsetFromNow < 1 {
			stats.Last1mTotal += p.Total
		}
		if offsetFromNow < 5 {
			stats.Last5mTotal += p.Total
		}
		if offsetFromNow < 60 {
			stats.Last1hTotal += p.Total
		}
		stats.Last24hTotal += p.Total
	}
	return stats
}

// TokenStats returns cumulative counters for a token, or zero value if
// unknown.
func (m *Metrics) TokenStats(tokenID string) TokenStats {
	m.mu.Lock()
	defer m.mu.Unlock()

	agg := m.tokens[tokenID]
	if agg == nil {
		return TokenStats{TokenID: tokenID}
	}
	out := TokenStats{
		TokenID:       agg.TokenID,
		Total:         agg.Total,
		Success:       agg.Success,
		Errors:        agg.Errors,
		LastRequestAt: agg.LastRequestAt,
		LastSuccessAt: agg.LastSuccessAt,
		LastErrorAt:   agg.LastErrorAt,
		LastErrorMsg:  agg.LastErrorMsg,
	}
	if agg.LatencyCount > 0 {
		out.AvgLatencyMs = agg.LatencySumMs / agg.LatencyCount
	}
	return out
}

// ForgetToken removes cumulative counters for the given token id. The
// in-memory ring buffer retains entries until overwritten by newer minutes.
func (m *Metrics) ForgetToken(tokenID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.tokens, tokenID)
	m.dirty = true
}

func truncateError(value string) string {
	value = strings.TrimSpace(value)
	const maxLen = 500
	if len(value) > maxLen {
		return value[:maxLen] + "…"
	}
	return value
}
