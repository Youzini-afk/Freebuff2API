package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type RunManager struct {
	cfg    Config
	logger *log.Logger
	client *UpstreamClient
	next   atomic.Uint64

	mu       sync.RWMutex
	pools    map[string]*tokenPool
	order    []string
	agentIDs []string

	stopCh chan struct{}
	wg     sync.WaitGroup
}

type tokenPool struct {
	id      string
	name    string
	label   string
	token   string
	enabled bool
	cfg     Config
	client  *UpstreamClient
	logger  *log.Logger

	mu            sync.Mutex
	runs          map[string]*managedRun // agentID → current run
	draining      []*managedRun
	session       *cachedSession
	sessionRefreshCh chan struct{}
	sessionRebuildScheduled bool
	lastError     string
	cooldownUntil time.Time
}

type managedRun struct {
	id           string
	agentID      string
	startedAt    time.Time
	inflight     int
	requestCount int
	finishing    bool
}

type runLease struct {
	pool *tokenPool
	run  *managedRun
}

type tokenSnapshot struct {
	ID                string        `json:"id"`
	Name              string        `json:"name"`
	Label             string        `json:"label,omitempty"`
	Enabled           bool          `json:"enabled"`
	Runs              []runSnapshot `json:"runs"`
	DrainingRuns      int           `json:"draining_runs"`
	SessionStatus     string        `json:"session_status,omitempty"`
	SessionInstanceID string        `json:"session_instance_id,omitempty"`
	SessionExpiresAt  time.Time     `json:"session_expires_at,omitempty"`
	CooldownUntil     time.Time     `json:"cooldown_until,omitempty"`
	LastError         string        `json:"last_error,omitempty"`
}

type runSnapshot struct {
	AgentID      string    `json:"agent_id"`
	RunID        string    `json:"run_id"`
	StartedAt    time.Time `json:"started_at"`
	Inflight     int       `json:"inflight"`
	RequestCount int       `json:"request_count"`
}

func NewRunManager(cfg Config, client *UpstreamClient, logger *log.Logger) *RunManager {
	return &RunManager{
		cfg:    cfg,
		logger: logger,
		client: client,
		pools:  make(map[string]*tokenPool),
		stopCh: make(chan struct{}),
	}
}

func (m *RunManager) Start(ctx context.Context, agentIDs []string) {
	m.mu.Lock()
	m.agentIDs = append([]string(nil), agentIDs...)
	m.mu.Unlock()

	// Pre-warm runs for all free agents in background.
	// The server is already listening; if a request arrives before
	// pre-warming finishes, acquire() will lazily create the run.
	go m.prewarm(ctx, agentIDs)

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				maintainCtx, cancel := context.WithTimeout(context.Background(), m.cfg.RequestTimeout)
				for _, pool := range m.snapshotPools() {
					if !pool.isEnabled() {
						continue
					}
					if err := pool.maintain(maintainCtx); err != nil {
						m.logger.Printf("%s: maintenance failed: %v", pool.name, err)
					}
				}
				cancel()
			case <-m.stopCh:
				return
			}
		}
	}()
}

// snapshotPools returns a copy of all pools in insertion order.
func (m *RunManager) snapshotPools() []*tokenPool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*tokenPool, 0, len(m.order))
	for _, id := range m.order {
		if p, ok := m.pools[id]; ok {
			out = append(out, p)
		}
	}
	return out
}

// enabledPools returns a stable slice of enabled pools in insertion order.
func (m *RunManager) enabledPools() []*tokenPool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*tokenPool, 0, len(m.order))
	for _, id := range m.order {
		pool, ok := m.pools[id]
		if !ok || !pool.isEnabled() {
			continue
		}
		out = append(out, pool)
	}
	return out
}

func (m *RunManager) prewarm(ctx context.Context, agentIDs []string) {
	startupCtx, cancel := context.WithTimeout(context.Background(), m.cfg.RequestTimeout)
	defer cancel()

	for _, pool := range m.snapshotPools() {
		if !pool.isEnabled() {
			continue
		}
		go pool.prewarmSession(ctx)
		for _, agentID := range agentIDs {
			if err := pool.rotateAgent(startupCtx, agentID); err != nil {
				m.logger.Printf("%s: prewarm %s failed: %v", pool.name, agentID, err)
			} else {
				m.logger.Printf("%s: prewarmed %s", pool.name, agentID)
			}
		}
	}
}

func (m *RunManager) Close(ctx context.Context) {
	close(m.stopCh)
	m.wg.Wait()
	for _, pool := range m.snapshotPools() {
		if err := pool.shutdown(ctx); err != nil {
			m.logger.Printf("%s: shutdown failed: %v", pool.name, err)
		}
	}
}

func (m *RunManager) Acquire(ctx context.Context, agentID string) (*runLease, error) {
	enabled := m.enabledPools()
	if len(enabled) == 0 {
		return nil, errors.New("no enabled auth tokens configured")
	}

	startIndex := int(m.next.Add(1)-1) % len(enabled)
	candidates := make([]*tokenPool, 0, len(enabled))
	for pass := 0; pass < 2; pass++ {
		for offset := 0; offset < len(enabled); offset++ {
			pool := enabled[(startIndex+offset)%len(enabled)]
			ready := pool.hasReadySession()
			if pass == 0 && !ready {
				continue
			}
			if pass == 1 && ready {
				continue
			}
			candidates = append(candidates, pool)
		}
	}

	var errs []string
	for _, pool := range candidates {
		lease, err := pool.acquire(ctx, agentID)
		if err == nil {
			return lease, nil
		}
		errs = append(errs, fmt.Sprintf("%s: %v", pool.name, err))
	}

	return nil, fmt.Errorf("unable to acquire run from any token (%s)", strings.Join(errs, "; "))
}

func (m *RunManager) Release(lease *runLease) {
	if lease == nil || lease.pool == nil || lease.run == nil {
		return
	}
	lease.pool.release(lease.run)
}

func (m *RunManager) Invalidate(lease *runLease, reason string) {
	if lease == nil || lease.pool == nil || lease.run == nil {
		return
	}
	lease.pool.invalidate(lease.run, reason)
}

func (m *RunManager) Cooldown(lease *runLease, duration time.Duration, reason string) {
	if lease == nil || lease.pool == nil {
		return
	}
	lease.pool.markCooldown(duration, reason)
}

func (m *RunManager) Snapshots() []tokenSnapshot {
	pools := m.snapshotPools()
	snapshots := make([]tokenSnapshot, 0, len(pools))
	for _, pool := range pools {
		snapshots = append(snapshots, pool.snapshot())
	}
	return snapshots
}

// SnapshotByID returns the live snapshot for a single pool, if present.
func (m *RunManager) SnapshotByID(id string) (tokenSnapshot, bool) {
	m.mu.RLock()
	pool, ok := m.pools[id]
	m.mu.RUnlock()
	if !ok {
		return tokenSnapshot{}, false
	}
	return pool.snapshot(), true
}

// AgentIDs returns the most recent agent list used for prewarming.
func (m *RunManager) AgentIDs() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, len(m.agentIDs))
	copy(out, m.agentIDs)
	return out
}

func (m *RunManager) ProbeRoute(ctx context.Context) (UpstreamRouteProbe, error) {
	return m.client.ProbeRoute(ctx)
}

func (m *RunManager) PrewarmToken(id string) error {
	m.mu.RLock()
	pool, ok := m.pools[id]
	agentIDs := append([]string(nil), m.agentIDs...)
	m.mu.RUnlock()
	if !ok {
		return errors.New("token not found")
	}
	if !pool.isEnabled() {
		return errors.New("token is disabled")
	}
	pool.requestManualPrewarm(agentIDs)
	return nil
}

// Reconcile adds missing pools, removes stale ones, and updates label /
// enabled flags to match the provided desired state.
func (m *RunManager) Reconcile(ctx context.Context, desired []ManagedToken) {
	desiredByID := make(map[string]ManagedToken, len(desired))
	for _, t := range desired {
		desiredByID[t.ID] = t
	}

	// Additions / updates.
	for _, token := range desired {
		m.upsertPool(ctx, token)
	}

	// Removals.
	m.mu.RLock()
	var toRemove []string
	for id := range m.pools {
		if _, keep := desiredByID[id]; !keep {
			toRemove = append(toRemove, id)
		}
	}
	m.mu.RUnlock()

	for _, id := range toRemove {
		m.removePool(ctx, id)
	}
}

// upsertPool adds the pool if missing, otherwise updates its label, enabled
// state and (if the secret changed) replaces it entirely.
func (m *RunManager) upsertPool(ctx context.Context, token ManagedToken) {
	m.mu.Lock()
	existing, ok := m.pools[token.ID]
	if ok {
		existing.mu.Lock()
		if token.Token != "" && existing.token != token.Token {
			m.mu.Unlock()
			existing.mu.Unlock()
			m.removePool(ctx, token.ID)
			m.mu.Lock()
			existing = nil
			ok = false
		} else {
			existing.label = token.Label
			existing.name = displayName(token)
			wasEnabled := existing.enabled
			existing.enabled = token.Enabled
			existing.mu.Unlock()
			m.mu.Unlock()
			if !wasEnabled && token.Enabled {
				go existing.prewarmSession(context.Background())
				for _, agentID := range m.AgentIDs() {
					go func(pool *tokenPool, id string) {
						startupCtx, cancel := context.WithTimeout(context.Background(), m.cfg.RequestTimeout)
						defer cancel()
						if err := pool.rotateAgent(startupCtx, id); err != nil {
							m.logger.Printf("%s: prewarm %s failed: %v", pool.name, id, err)
						}
					}(existing, agentID)
				}
			}
			return
		}
	}
	if !ok {
		pool := &tokenPool{
			id:      token.ID,
			label:   token.Label,
			name:    displayName(token),
			token:   token.Token,
			enabled: token.Enabled,
			cfg:     m.cfg,
			client:  m.client,
			runs:    make(map[string]*managedRun),
			logger:  m.logger,
		}
		m.pools[token.ID] = pool
		m.order = append(m.order, token.ID)
		agentIDs := append([]string(nil), m.agentIDs...)
		m.mu.Unlock()

		if token.Enabled {
			go pool.prewarmSession(context.Background())
			for _, agentID := range agentIDs {
				go func(id string) {
					startupCtx, cancel := context.WithTimeout(context.Background(), m.cfg.RequestTimeout)
					defer cancel()
					if err := pool.rotateAgent(startupCtx, id); err != nil {
						m.logger.Printf("%s: prewarm %s failed: %v", pool.name, id, err)
					}
				}(agentID)
			}
		}
		return
	}
}

// removePool detaches a pool from the manager and shuts it down.
func (m *RunManager) removePool(ctx context.Context, id string) {
	m.mu.Lock()
	pool, ok := m.pools[id]
	if !ok {
		m.mu.Unlock()
		return
	}
	delete(m.pools, id)
	for i, existing := range m.order {
		if existing == id {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
	m.mu.Unlock()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), m.cfg.RequestTimeout)
	defer cancel()
	if err := pool.shutdown(shutdownCtx); err != nil {
		m.logger.Printf("%s: shutdown failed: %v", pool.name, err)
	}
}

func displayName(token ManagedToken) string {
	if label := strings.TrimSpace(token.Label); label != "" {
		return label
	}
	if id := strings.TrimSpace(token.ID); id != "" {
		return id
	}
	return "token"
}

func (p *tokenPool) acquire(ctx context.Context, agentID string) (*runLease, error) {
	p.mu.Lock()
	if now := time.Now(); now.Before(p.cooldownUntil) {
		cooldownUntil := p.cooldownUntil
		p.mu.Unlock()
		return nil, fmt.Errorf("token cooling down until %s", cooldownUntil.Format(time.RFC3339))
	}
	run := p.runs[agentID]
	needsRotate := run == nil || time.Since(run.startedAt) >= p.cfg.RotationInterval
	p.mu.Unlock()

	if needsRotate {
		if err := p.rotateAgent(ctx, agentID); err != nil {
			return nil, err
		}
	}

	if _, err := p.ensureSession(ctx); err != nil {
		return nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	run = p.runs[agentID]
	if run == nil {
		return nil, errors.New("run missing after rotation")
	}
	run.inflight++
	run.requestCount++
	return &runLease{pool: p, run: run}, nil
}

func (p *tokenPool) maintain(ctx context.Context) error {
	p.mu.Lock()
	var toRotate []string
	for agentID, run := range p.runs {
		if time.Since(run.startedAt) >= p.cfg.RotationInterval {
			toRotate = append(toRotate, agentID)
		}
	}
	draining := append([]*managedRun(nil), p.draining...)
	p.mu.Unlock()

	for _, agentID := range toRotate {
		if err := p.rotateAgent(ctx, agentID); err != nil {
			p.logger.Printf("%s: rotate agent %s failed: %v", p.name, agentID, err)
		}
	}

	for _, run := range draining {
		if err := p.finishIfReady(run); err != nil {
			p.logger.Printf("%s: finish draining run %s failed: %v", p.name, run.id, err)
		}
	}
	return nil
}

func (p *tokenPool) shutdown(ctx context.Context) error {
	p.mu.Lock()
	var allRuns []*managedRun
	for _, run := range p.runs {
		allRuns = append(allRuns, run)
	}
	allRuns = append(allRuns, p.draining...)
	p.runs = make(map[string]*managedRun)
	p.draining = nil
	p.mu.Unlock()

	var errs []string
	for _, run := range allRuns {
		if err := p.client.FinishRun(ctx, p.token, run.id, run.requestCount); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if err := p.endSession(ctx); err != nil {
		errs = append(errs, err.Error())
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func (p *tokenPool) rotateAgent(ctx context.Context, agentID string) error {
	p.mu.Lock()
	if now := time.Now(); now.Before(p.cooldownUntil) {
		cooldownUntil := p.cooldownUntil
		p.mu.Unlock()
		return fmt.Errorf("token cooling down until %s", cooldownUntil.Format(time.RFC3339))
	}
	p.mu.Unlock()

	runID, err := p.client.StartRun(ctx, p.token, agentID)
	if err != nil {
		p.mu.Lock()
		p.lastError = err.Error()
		p.mu.Unlock()
		return err
	}

	p.mu.Lock()
	oldRun := p.runs[agentID]
	p.runs[agentID] = &managedRun{
		id:        runID,
		agentID:   agentID,
		startedAt: time.Now(),
	}
	p.lastError = ""
	if oldRun != nil {
		p.draining = append(p.draining, oldRun)
	}
	p.mu.Unlock()

	if oldRun != nil {
		go func(run *managedRun) {
			if err := p.finishIfReady(run); err != nil {
				p.logger.Printf("%s: finish rotated run %s (agent %s) failed: %v", p.name, run.id, run.agentID, err)
			}
		}(oldRun)
	}
	return nil
}

func (p *tokenPool) requestManualPrewarm(agentIDs []string) {
	p.mu.Lock()
	p.cooldownUntil = time.Time{}
	p.lastError = ""
	p.mu.Unlock()

	p.ensureSessionAsync("manual")
	for _, agentID := range agentIDs {
		go func(id string) {
			startupCtx, cancel := context.WithTimeout(context.Background(), p.cfg.RequestTimeout)
			defer cancel()
			if err := p.rotateAgent(startupCtx, id); err != nil {
				p.logger.Printf("%s: manual prewarm %s failed: %v", p.name, id, err)
			} else {
				p.logger.Printf("%s: manual prewarmed %s", p.name, id)
			}
		}(agentID)
	}
}

func (p *tokenPool) release(run *managedRun) {
	if run == nil {
		return
	}

	p.mu.Lock()
	if run.inflight > 0 {
		run.inflight--
	}
	p.mu.Unlock()

	if err := p.finishIfReady(run); err != nil {
		p.logger.Printf("%s: finish released run %s failed: %v", p.name, run.id, err)
	}
}

func (p *tokenPool) finishIfReady(run *managedRun) error {
	p.mu.Lock()
	if run == nil || run.inflight > 0 || run.finishing {
		p.mu.Unlock()
		return nil
	}
	// Only finish if this run is no longer the current run for its agent
	if current, ok := p.runs[run.agentID]; ok && current == run {
		p.mu.Unlock()
		return nil
	}
	run.finishing = true
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), p.cfg.RequestTimeout)
	defer cancel()

	if err := p.client.FinishRun(ctx, p.token, run.id, run.requestCount); err != nil {
		p.mu.Lock()
		run.finishing = false
		p.lastError = err.Error()
		p.mu.Unlock()
		return err
	}

	p.mu.Lock()
	filtered := p.draining[:0]
	for _, drainingRun := range p.draining {
		if drainingRun != run {
			filtered = append(filtered, drainingRun)
		}
	}
	p.draining = filtered
	p.mu.Unlock()
	return nil
}

func (p *tokenPool) invalidate(run *managedRun, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Remove from current runs if it matches
	if current, ok := p.runs[run.agentID]; ok && current == run {
		delete(p.runs, run.agentID)
	}

	filtered := p.draining[:0]
	for _, drainingRun := range p.draining {
		if drainingRun != run {
			filtered = append(filtered, drainingRun)
		}
	}
	p.draining = filtered
	if reason != "" {
		p.lastError = reason
	}
}

func (p *tokenPool) markCooldown(duration time.Duration, reason string) {
	if duration <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cooldownUntil = time.Now().Add(duration)
	if reason != "" {
		p.lastError = reason
	}
}

func (p *tokenPool) isEnabled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.enabled
}

func (p *tokenPool) snapshot() tokenSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()

	snapshot := tokenSnapshot{
		ID:            p.id,
		Name:          p.name,
		Label:         p.label,
		Enabled:       p.enabled,
		DrainingRuns:  len(p.draining),
		CooldownUntil: p.cooldownUntil,
		LastError:     p.lastError,
	}
	if p.session != nil {
		snapshot.SessionStatus = string(p.session.status)
		snapshot.SessionInstanceID = p.session.instanceID
		snapshot.SessionExpiresAt = p.session.expiresAt
	}
	for agentID, run := range p.runs {
		snapshot.Runs = append(snapshot.Runs, runSnapshot{
			AgentID:      agentID,
			RunID:        run.id,
			StartedAt:    run.startedAt,
			Inflight:     run.inflight,
			RequestCount: run.requestCount,
		})
	}
	return snapshot
}
