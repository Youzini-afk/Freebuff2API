package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	adminSessionCookie = "freebuff_admin"
	adminSessionTTL    = 24 * time.Hour
)

// AdminHandler exposes the administration WebUI and JSON API.
type AdminHandler struct {
	cfg      Config
	logger   *log.Logger
	store    *TokenStore
	runs     *RunManager
	registry *ModelRegistry
	metrics  *Metrics
	proxy    *EmbeddedMihomoManager
	staticFS fs.FS

	hmacKey []byte
}

// NewAdminHandler returns an initialised admin handler. staticFS may be nil
// when the WebUI is not bundled (the API endpoints remain usable).
func NewAdminHandler(cfg Config, logger *log.Logger, store *TokenStore, runs *RunManager, registry *ModelRegistry, metrics *Metrics, proxy *EmbeddedMihomoManager, staticFS fs.FS) (*AdminHandler, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate admin session key: %w", err)
	}
	return &AdminHandler{
		cfg:      cfg,
		logger:   logger,
		store:    store,
		runs:     runs,
		registry: registry,
		metrics:  metrics,
		proxy:    proxy,
		staticFS: staticFS,
		hmacKey:  key,
	}, nil
}

// Enabled reports whether the admin WebUI is available.
func (a *AdminHandler) Enabled() bool {
	return a != nil && strings.TrimSpace(a.cfg.AdminPassword) != ""
}

// Register attaches admin routes to the given mux.
func (a *AdminHandler) Register(mux *http.ServeMux) {
	if !a.Enabled() {
		return
	}

	mux.HandleFunc("/admin", a.handleRoot)
	mux.HandleFunc("/admin/", a.handleRoot)
	mux.HandleFunc("/admin/api/login", a.handleLogin)
	mux.HandleFunc("/admin/api/logout", a.handleLogout)
	mux.HandleFunc("/admin/api/me", a.handleMe)
	mux.HandleFunc("/admin/api/overview", a.authed(a.handleOverview))
	mux.HandleFunc("/admin/api/tokens", a.authed(a.handleTokens))
	mux.HandleFunc("/admin/api/tokens/import", a.authed(a.handleTokenImport))
	mux.HandleFunc("/admin/api/tokens/", a.authed(a.handleTokenByID))
	mux.HandleFunc("/admin/api/metrics", a.authed(a.handleMetrics))
	mux.HandleFunc("/admin/api/metrics/token/", a.authed(a.handleMetricsForToken))
	mux.HandleFunc("/admin/api/config", a.authed(a.handleConfigSummary))
	mux.HandleFunc("/admin/api/models/status", a.authed(a.handleModelRegistryStatus))
	mux.HandleFunc("/admin/api/models/refresh", a.authed(a.handleModelRegistryRefresh))
	mux.HandleFunc("/admin/api/proxy/status", a.authed(a.handleProxyStatus))
	mux.HandleFunc("/admin/api/proxy/start", a.authed(a.handleProxyStart))
	mux.HandleFunc("/admin/api/proxy/stop", a.authed(a.handleProxyStop))
	mux.HandleFunc("/admin/api/proxy/restart", a.authed(a.handleProxyRestart))
	mux.HandleFunc("/admin/api/proxy/subscription", a.authed(a.handleProxySubscription))
	mux.HandleFunc("/admin/api/proxy/groups", a.authed(a.handleProxyGroups))
	mux.HandleFunc("/admin/api/proxy/select", a.authed(a.handleProxySelect))
	mux.HandleFunc("/admin/api/proxy/probe", a.authed(a.handleProxyProbe))
	mux.HandleFunc("/admin/api/proxy/upstream-probe", a.authed(a.handleProxyUpstreamProbe))
	mux.HandleFunc("/admin/api/proxy/logs", a.authed(a.handleProxyLogs))
}

// -----------------------------------------------------------------------------
// Session + auth helpers.
// -----------------------------------------------------------------------------

func (a *AdminHandler) authed(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.isAuthenticated(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error": "unauthenticated",
			})
			return
		}
		handler(w, r)
	}
}

func (a *AdminHandler) isAuthenticated(r *http.Request) bool {
	cookie, err := r.Cookie(adminSessionCookie)
	if err != nil {
		return false
	}
	return a.verifySession(cookie.Value)
}

func (a *AdminHandler) verifySession(value string) bool {
	parts := strings.SplitN(value, ".", 2)
	if len(parts) != 2 {
		return false
	}
	issuedUnix, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return false
	}
	expected := a.signSession(issuedUnix)
	if !hmac.Equal([]byte(expected), []byte(value)) {
		return false
	}
	issued := time.Unix(issuedUnix, 0)
	if time.Since(issued) > adminSessionTTL {
		return false
	}
	if issued.After(time.Now().Add(5 * time.Minute)) {
		return false
	}
	return true
}

func (a *AdminHandler) signSession(issuedUnix int64) string {
	mac := hmac.New(sha256.New, a.hmacKey)
	_, _ = mac.Write([]byte(strconv.FormatInt(issuedUnix, 10)))
	return fmt.Sprintf("%d.%s", issuedUnix, hex.EncodeToString(mac.Sum(nil)))
}

func (a *AdminHandler) setSessionCookie(w http.ResponseWriter, r *http.Request, value string) {
	http.SetCookie(w, &http.Cookie{
		Name:     adminSessionCookie,
		Value:    value,
		Path:     "/admin",
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(adminSessionTTL),
	})
}

func (a *AdminHandler) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     adminSessionCookie,
		Value:    "",
		Path:     "/admin",
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// -----------------------------------------------------------------------------
// Static HTML serving.
// -----------------------------------------------------------------------------

func (a *AdminHandler) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/admin" {
		http.Redirect(w, r, "/admin/", http.StatusFound)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/admin/")
	if path == "" || path == "index.html" {
		a.serveIndex(w, r)
		return
	}
	if a.staticFS == nil {
		http.NotFound(w, r)
		return
	}
	http.StripPrefix("/admin/", http.FileServer(http.FS(a.staticFS))).ServeHTTP(w, r)
}

func (a *AdminHandler) serveIndex(w http.ResponseWriter, r *http.Request) {
	if a.staticFS == nil {
		http.Error(w, "admin UI not bundled", http.StatusNotFound)
		return
	}
	data, err := fs.ReadFile(a.staticFS, "index.html")
	if err != nil {
		http.Error(w, "admin UI not bundled", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

// -----------------------------------------------------------------------------
// Auth API.
// -----------------------------------------------------------------------------

type loginRequest struct {
	Password string `json:"password"`
}

func (a *AdminHandler) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}

	var body loginRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	expected := strings.TrimSpace(a.cfg.AdminPassword)
	got := strings.TrimSpace(body.Password)
	if expected == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "admin disabled"})
		return
	}
	if subtle.ConstantTimeCompare([]byte(expected), []byte(got)) != 1 {
		// Slow down brute-force attempts.
		time.Sleep(750 * time.Millisecond)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid password"})
		return
	}

	token := a.signSession(time.Now().Unix())
	a.setSessionCookie(w, r, token)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *AdminHandler) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	a.clearSessionCookie(w, r)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *AdminHandler) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": a.isAuthenticated(r),
	})
}

// -----------------------------------------------------------------------------
// Overview / config.
// -----------------------------------------------------------------------------

func (a *AdminHandler) handleOverview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	totals := a.metrics.Overview()
	snapshots := a.runs.Snapshots()

	enabledCount := 0
	readyCount := 0
	for _, s := range snapshots {
		if s.Enabled {
			enabledCount++
		}
		if s.Enabled && s.SessionStatus == string(sessionStatusActive) {
			readyCount++
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"totals":      totals,
		"token_count": len(snapshots),
		"enabled_tokens": enabledCount,
		"ready_tokens":   readyCount,
		"upstream":    a.cfg.UpstreamBaseURL,
		"data_dir":    a.cfg.DataDir,
	})
}

func (a *AdminHandler) handleConfigSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"listen_addr":       a.cfg.ListenAddr,
		"upstream_base_url": a.cfg.UpstreamBaseURL,
		"rotation_interval": a.cfg.RotationInterval.String(),
		"request_timeout":   a.cfg.RequestTimeout.String(),
		"data_dir":          a.cfg.DataDir,
		"api_keys_enabled":  len(a.cfg.APIKeys) > 0,
		"proxy_backend_mode": a.cfg.ProxyBackendMode,
		"http_proxy_set":    strings.TrimSpace(a.cfg.HTTPProxy) != "",
		"embedded_mihomo_mixed_port": a.cfg.EmbeddedMihomoMixedPort,
		"embedded_mihomo_controller_port": a.cfg.EmbeddedMihomoControllerPort,
		"embedded_mihomo_group_name": a.cfg.EmbeddedMihomoGroupName,
		"embedded_mihomo_binary_path_set": strings.TrimSpace(a.cfg.EmbeddedMihomoBinaryPath) != "",
	})
}

func (a *AdminHandler) handleModelRegistryStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"registry": a.registry.Status()})
}

func (a *AdminHandler) handleModelRegistryRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	status, err := a.registry.RefreshNow(ctx)
	response := map[string]any{"registry": status}
	if err != nil {
		response["error"] = err.Error()
		writeJSON(w, http.StatusBadGateway, response)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (a *AdminHandler) handleProxyStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	writeJSON(w, http.StatusOK, a.proxy.Status())
}

func (a *AdminHandler) handleProxyStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	status, err := a.proxy.Start()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "proxy": status})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"proxy": status})
}

func (a *AdminHandler) handleProxyStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	status, err := a.proxy.Stop()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error(), "proxy": status})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"proxy": status})
}

func (a *AdminHandler) handleProxyRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	status, err := a.proxy.Restart()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "proxy": status})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"proxy": status})
}

func (a *AdminHandler) handleProxySubscription(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	var body struct {
		SubscriptionURL string `json:"subscription_url"`
		RestartIfRunning *bool `json:"restart_if_running"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	restartIfRunning := true
	if body.RestartIfRunning != nil {
		restartIfRunning = *body.RestartIfRunning
	}
	status, err := a.proxy.UpdateSubscription(body.SubscriptionURL, restartIfRunning)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "proxy": status})
		return
	}
	response := map[string]any{"proxy": status}
	if status.Running {
		groups, groupErr := a.proxy.RefreshProvider()
		if groupErr != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": groupErr.Error(), "proxy": a.proxy.Status()})
			return
		}
		response["proxy"] = a.proxy.Status()
		response["groups"] = groups
	}
	writeJSON(w, http.StatusOK, response)
}

func (a *AdminHandler) handleProxyGroups(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	groups, err := a.proxy.Groups()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"running":        false,
			"selected_group": a.cfg.EmbeddedMihomoGroupName,
			"groups":         []any{},
			"error":          err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, groups)
}

func (a *AdminHandler) handleProxySelect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	var body struct {
		GroupName string `json:"group_name"`
		ProxyName string `json:"proxy_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	groups, err := a.proxy.SelectProxy(body.GroupName, body.ProxyName)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": groups, "proxy": a.proxy.Status()})
}

func (a *AdminHandler) handleProxyProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	probe, err := a.proxy.ProbeExit()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "probe": probe, "proxy": a.proxy.Status()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"probe": probe, "proxy": a.proxy.Status()})
}

func (a *AdminHandler) handleProxyUpstreamProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	probe, err := a.runs.ProbeRoute(ctx)
	response := map[string]any{
		"probe": probe,
		"proxy": a.proxy.Status(),
	}
	if err != nil {
		response["error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, response)
}

func (a *AdminHandler) handleProxyLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	limit := 80
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": a.proxy.Logs(limit)})
}

// -----------------------------------------------------------------------------
// Token CRUD.
// -----------------------------------------------------------------------------

type tokenView struct {
	ID            string    `json:"id"`
	Label         string    `json:"label"`
	TokenMasked   string    `json:"token_masked"`
	Enabled       bool      `json:"enabled"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	Pool          *poolView `json:"pool,omitempty"`
	Stats         TokenStats `json:"stats"`
}

type poolView struct {
	Name              string        `json:"name"`
	Enabled           bool          `json:"enabled"`
	SessionStatus     string        `json:"session_status"`
	SessionInstanceID string        `json:"session_instance_id,omitempty"`
	SessionExpiresAt  time.Time     `json:"session_expires_at,omitempty"`
	CooldownUntil     time.Time     `json:"cooldown_until,omitempty"`
	LastError         string        `json:"last_error,omitempty"`
	Runs              []runSnapshot `json:"runs"`
	DrainingRuns      int           `json:"draining_runs"`
}

func (a *AdminHandler) buildTokenView(record ManagedToken) tokenView {
	view := tokenView{
		ID:          record.ID,
		Label:       record.Label,
		TokenMasked: MaskToken(record.Token),
		Enabled:     record.Enabled,
		CreatedAt:   record.CreatedAt,
		UpdatedAt:   record.UpdatedAt,
		Stats:       a.metrics.TokenStats(record.ID),
	}
	if snap, ok := a.runs.SnapshotByID(record.ID); ok {
		view.Pool = &poolView{
			Name:              snap.Name,
			Enabled:           snap.Enabled,
			SessionStatus:     snap.SessionStatus,
			SessionInstanceID: snap.SessionInstanceID,
			SessionExpiresAt:  snap.SessionExpiresAt,
			CooldownUntil:     snap.CooldownUntil,
			LastError:         snap.LastError,
			Runs:              snap.Runs,
			DrainingRuns:      snap.DrainingRuns,
		}
	}
	return view
}

func (a *AdminHandler) handleTokens(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		records := a.store.List()
		views := make([]tokenView, 0, len(records))
		for _, record := range records {
			views = append(views, a.buildTokenView(record))
		}
		writeJSON(w, http.StatusOK, map[string]any{"tokens": views})
	case http.MethodPost:
		var body struct {
			Token string `json:"token"`
			Label string `json:"label"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
			return
		}
		if strings.TrimSpace(body.Token) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "token is required"})
			return
		}
		record, err := a.store.Create(body.Label, body.Token)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		a.reconcileRuns()
		writeJSON(w, http.StatusOK, map[string]any{"token": a.buildTokenView(record)})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
	}
}

func (a *AdminHandler) handleTokenImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}

	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "failed to read import file"})
		return
	}

	records, err := ParseImportedTokens(data)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	imported, skipped, err := a.store.Import(records)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	views := make([]tokenView, 0, len(imported))
	if len(imported) > 0 {
		a.reconcileRuns()
		for _, record := range imported {
			views = append(views, a.buildTokenView(record))
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"imported":       views,
		"imported_count": len(imported),
		"skipped_count":  skipped,
	})
}

func (a *AdminHandler) handleTokenByID(w http.ResponseWriter, r *http.Request) {
	id, tail := splitTrailingID("/admin/api/tokens/", r.URL.Path)
	if id == "" {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "token id required"})
		return
	}

	if tail == "refresh" {
		a.handleTokenRefresh(w, r, id)
		return
	}
	if tail != "" {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown endpoint"})
		return
	}

	switch r.Method {
	case http.MethodPatch:
		var body struct {
			Label   *string `json:"label,omitempty"`
			Enabled *bool   `json:"enabled,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
			return
		}
		record, err := a.store.Update(id, body.Label, body.Enabled)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		a.reconcileRuns()
		writeJSON(w, http.StatusOK, map[string]any{"token": a.buildTokenView(record)})
	case http.MethodDelete:
		if err := a.store.Delete(id); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
			return
		}
		a.reconcileRuns()
		a.metrics.ForgetToken(id)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case http.MethodGet:
		record, ok := a.store.Get(id)
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "token not found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"token": a.buildTokenView(record)})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
	}
}

func (a *AdminHandler) handleTokenRefresh(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	record, ok := a.store.Get(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "token not found"})
		return
	}
	a.reconcileRuns()
	if err := a.runs.PrewarmToken(id); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"ok":      true,
		"message": "manual prewarm requested",
		"token":   a.buildTokenView(record),
	})
}

// -----------------------------------------------------------------------------
// Metrics API.
// -----------------------------------------------------------------------------

func (a *AdminHandler) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	minutes := parseRangeMinutes(r.URL.Query().Get("range"))
	writeJSON(w, http.StatusOK, map[string]any{
		"minutes": minutes,
		"series":  a.metrics.Series(minutes),
	})
}

func (a *AdminHandler) handleMetricsForToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/admin/api/metrics/token/")
	if id == "" {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "token id required"})
		return
	}
	minutes := parseRangeMinutes(r.URL.Query().Get("range"))
	writeJSON(w, http.StatusOK, map[string]any{
		"minutes": minutes,
		"series":  a.metrics.SeriesForToken(id, minutes),
		"stats":   a.metrics.TokenStats(id),
	})
}

func parseRangeMinutes(value string) int {
	value = strings.TrimSpace(value)
	if value == "" {
		return 60
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return 60
	}
	if parsed > 24*60 {
		return 24 * 60
	}
	return parsed
}

// splitTrailingID splits "prefix + id + (/rest)" into (id, rest). If there
// is no trailing slash or rest, rest is "".
func splitTrailingID(prefix, path string) (string, string) {
	rest := strings.TrimPrefix(path, prefix)
	if rest == "" {
		return "", ""
	}
	if idx := strings.Index(rest, "/"); idx >= 0 {
		return rest[:idx], rest[idx+1:]
	}
	return rest, ""
}

// -----------------------------------------------------------------------------
// RunManager reconciliation helper.
// -----------------------------------------------------------------------------

func (a *AdminHandler) reconcileRuns() {
	ctx, cancel := context.WithTimeout(context.Background(), a.cfg.RequestTimeout)
	defer cancel()
	a.runs.Reconcile(ctx, a.store.List())
}

