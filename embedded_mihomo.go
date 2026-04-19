package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	proxyBackendHTTPProxy      = "http_proxy"
	proxyBackendEmbeddedMihomo = "embedded_mihomo"
)

type EmbeddedMihomoState struct {
	SubscriptionURL string `json:"subscription_url"`
	CurrentGroup    string `json:"current_group"`
	CurrentProxy    string `json:"current_proxy"`
	LastError       string `json:"last_error"`
	LastStartedAt   string `json:"last_started_at"`
	LastStoppedAt   string `json:"last_stopped_at"`
	LastUpdatedAt   string `json:"last_updated_at"`
	LastProbeAt     string `json:"last_probe_at"`
	LastProbeIP     string `json:"last_probe_ip"`
	LastProbeLoc    string `json:"last_probe_loc"`
	LastProbeError  string `json:"last_probe_error"`
	LastProbeBlocked bool  `json:"last_probe_blocked"`
}

type EmbeddedMihomoStatus struct {
	Enabled                bool   `json:"enabled"`
	Mode                   string `json:"mode"`
	Running                bool   `json:"running"`
	SubscriptionConfigured bool   `json:"subscription_configured"`
	SubscriptionURL        string `json:"subscription_url"`
	BinaryPath             string `json:"binary_path"`
	DataDir                string `json:"data_dir"`
	ConfigPath             string `json:"config_path"`
	MixedProxyURL          string `json:"mixed_proxy_url"`
	ControllerURL          string `json:"controller_url"`
	GroupName              string `json:"group_name"`
	CurrentGroup           string `json:"current_group"`
	CurrentProxy           string `json:"current_proxy"`
	LastError              string `json:"last_error"`
	LastStartedAt          string `json:"last_started_at"`
	LastStoppedAt          string `json:"last_stopped_at"`
	LastUpdatedAt          string `json:"last_updated_at"`
	LastProbeAt            string `json:"last_probe_at"`
	LastProbeIP            string `json:"last_probe_ip"`
	LastProbeLoc           string `json:"last_probe_loc"`
	LastProbeError         string `json:"last_probe_error"`
	LastProbeBlocked       bool   `json:"last_probe_blocked"`
	GroupsCount            int    `json:"groups_count"`
}

type EmbeddedMihomoProbe struct {
	At      string `json:"at"`
	IP      string `json:"ip"`
	Loc     string `json:"loc"`
	Blocked bool   `json:"blocked"`
	Error   string `json:"error"`
}

type EmbeddedMihomoGroup struct {
	Name  string   `json:"name"`
	Type  string   `json:"type"`
	Now   string   `json:"now"`
	All   []string `json:"all"`
	Alive bool     `json:"alive"`
}

type EmbeddedMihomoGroups struct {
	Running       bool                  `json:"running"`
	SelectedGroup string                `json:"selected_group"`
	Groups        []EmbeddedMihomoGroup `json:"groups"`
}

type EmbeddedMihomoManager struct {
	cfg          Config
	logger       *log.Logger
	dataDir      string
	providersDir string
	configPath   string
	statePath    string
	logPath      string

	mu       sync.RWMutex
	cmd      *exec.Cmd
	exitCh   chan struct{}
	logFile  *os.File
	stopping bool
	state    EmbeddedMihomoState
	providerRefreshPending bool
}

func NewEmbeddedMihomoManager(cfg Config, logger *log.Logger) (*EmbeddedMihomoManager, error) {
	dataDir := filepath.Join(cfg.DataDir, "mihomo")
	manager := &EmbeddedMihomoManager{
		cfg:          cfg,
		logger:       logger,
		dataDir:      dataDir,
		providersDir: filepath.Join(dataDir, "providers"),
		configPath:   filepath.Join(dataDir, "config.yaml"),
		statePath:    filepath.Join(dataDir, "state.json"),
		logPath:      filepath.Join(dataDir, "mihomo.log"),
	}
	if err := os.MkdirAll(manager.providersDir, 0o755); err != nil {
		return nil, fmt.Errorf("create mihomo data dir: %w", err)
	}
	manager.loadState()
	return manager, nil
}

func (m *EmbeddedMihomoManager) loadState() {
	data, err := os.ReadFile(m.statePath)
	if err != nil {
		return
	}
	var state EmbeddedMihomoState
	if err := json.Unmarshal(data, &state); err != nil {
		return
	}
	m.state = state
}

func (m *EmbeddedMihomoManager) saveStateLocked() {
	data, err := json.MarshalIndent(m.state, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(m.statePath, data, 0o600)
}

func (m *EmbeddedMihomoManager) providerPath() string {
	return filepath.Join(m.providersDir, "primary.yaml")
}

func (m *EmbeddedMihomoManager) resetSelectionStateLocked() {
	m.state.CurrentGroup = ""
	m.state.CurrentProxy = ""
	m.state.LastProbeAt = ""
	m.state.LastProbeIP = ""
	m.state.LastProbeLoc = ""
	m.state.LastProbeError = ""
	m.state.LastProbeBlocked = false
}

func (m *EmbeddedMihomoManager) clearProviderCacheLocked() error {
	if err := os.RemoveAll(m.providersDir); err != nil {
		return fmt.Errorf("clear mihomo provider cache: %w", err)
	}
	if err := os.MkdirAll(m.providersDir, 0o755); err != nil {
		return fmt.Errorf("recreate mihomo providers dir: %w", err)
	}
	return nil
}

func (m *EmbeddedMihomoManager) currentSubscriptionLocked() string {
	if value := strings.TrimSpace(m.state.SubscriptionURL); value != "" {
		return value
	}
	return strings.TrimSpace(m.cfg.EmbeddedMihomoSubscriptionURL)
}

func (m *EmbeddedMihomoManager) SubscriptionConfigured() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.currentSubscriptionLocked() != ""
}

func (m *EmbeddedMihomoManager) findBinary() string {
	if candidate := strings.TrimSpace(m.cfg.EmbeddedMihomoBinaryPath); candidate != "" {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate
		}
	}
	if discovered, err := exec.LookPath("mihomo"); err == nil && strings.TrimSpace(discovered) != "" {
		return discovered
	}
	candidates := compactStrings([]string{
		filepath.Join(m.dataDir, "bin", "mihomo"),
		"/mihomo",
		"/usr/local/bin/mihomo",
		"/usr/bin/mihomo",
		"/app/bin/mihomo",
	})
	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() {
			continue
		}
		if info.Mode()&0o111 != 0 {
			return candidate
		}
	}
	return ""
}

func (m *EmbeddedMihomoManager) MixedProxyURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d", m.cfg.EmbeddedMihomoMixedPort)
}

func (m *EmbeddedMihomoManager) ControllerURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d", m.cfg.EmbeddedMihomoControllerPort)
}

func (m *EmbeddedMihomoManager) ProxyURL() string {
	if !m.IsRunning() {
		return ""
	}
	return m.MixedProxyURL()
}

func (m *EmbeddedMihomoManager) IsRunning() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.exitCh != nil && m.cmd != nil && m.cmd.Process != nil
}

func (m *EmbeddedMihomoManager) LastError() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return strings.TrimSpace(m.state.LastError)
}

func (m *EmbeddedMihomoManager) Status() EmbeddedMihomoStatus {
	m.mu.RLock()
	running := m.exitCh != nil && m.cmd != nil && m.cmd.Process != nil
	status := EmbeddedMihomoStatus{
		Enabled:                m.cfg.ProxyBackendMode == proxyBackendEmbeddedMihomo,
		Mode:                   m.cfg.ProxyBackendMode,
		Running:                running,
		SubscriptionConfigured: m.currentSubscriptionLocked() != "",
		SubscriptionURL:        m.currentSubscriptionLocked(),
		BinaryPath:             m.findBinary(),
		DataDir:                m.dataDir,
		ConfigPath:             m.configPath,
		MixedProxyURL:          m.MixedProxyURL(),
		ControllerURL:          m.ControllerURL(),
		GroupName:              m.cfg.EmbeddedMihomoGroupName,
		CurrentGroup:           m.state.CurrentGroup,
		CurrentProxy:           m.state.CurrentProxy,
		LastError:              m.state.LastError,
		LastStartedAt:          m.state.LastStartedAt,
		LastStoppedAt:          m.state.LastStoppedAt,
		LastUpdatedAt:          m.state.LastUpdatedAt,
		LastProbeAt:            m.state.LastProbeAt,
		LastProbeIP:            m.state.LastProbeIP,
		LastProbeLoc:           m.state.LastProbeLoc,
		LastProbeError:         m.state.LastProbeError,
		LastProbeBlocked:       m.state.LastProbeBlocked,
	}
	m.mu.RUnlock()
	if running {
		if groups, err := m.Groups(); err == nil {
			status.GroupsCount = len(groups.Groups)
			if groups.SelectedGroup != "" {
				status.CurrentGroup = groups.SelectedGroup
			}
			for _, group := range groups.Groups {
				if group.Name == status.CurrentGroup {
					status.CurrentProxy = group.Now
					break
				}
			}
		}
	}
	return status
}

func (m *EmbeddedMihomoManager) writeConfigLocked(subscriptionURL string) error {
	providerPath := filepath.ToSlash(m.providerPath())
	content := strings.Join([]string{
		"mixed-port: " + strconv.Itoa(m.cfg.EmbeddedMihomoMixedPort),
		"allow-lan: false",
		"bind-address: \"*\"",
		"mode: rule",
		"log-level: info",
		"ipv6: true",
		"external-controller: \"127.0.0.1:" + strconv.Itoa(m.cfg.EmbeddedMihomoControllerPort) + "\"",
		"secret: " + strconv.Quote(m.cfg.EmbeddedMihomoSecret),
		"proxy-providers:",
		"  primary:",
		"    type: http",
		"    url: " + strconv.Quote(subscriptionURL),
		"    path: " + strconv.Quote(providerPath),
		"    interval: 3600",
		"    health-check:",
		"      enable: true",
		"      url: " + strconv.Quote(m.cfg.EmbeddedMihomoTestURL),
		"      interval: 600",
		"proxy-groups:",
		"  - name: \"自动选择\"",
		"    type: url-test",
		"    use: [\"primary\"]",
		"    url: " + strconv.Quote(m.cfg.EmbeddedMihomoTestURL),
		"    interval: 300",
		"    tolerance: 150",
		"  - name: " + strconv.Quote(m.cfg.EmbeddedMihomoGroupName),
		"    type: select",
		"    use: [\"primary\"]",
		"    proxies: [\"自动选择\", \"DIRECT\"]",
		"rules:",
		"  - \"MATCH," + strings.ReplaceAll(m.cfg.EmbeddedMihomoGroupName, "\"", "") + "\"",
	}, "\n") + "\n"
	return os.WriteFile(m.configPath, []byte(content), 0o600)
}

func (m *EmbeddedMihomoManager) waitLoop(cmd *exec.Cmd, logFile *os.File, exitCh chan struct{}) {
	err := cmd.Wait()
	close(exitCh)
	m.mu.Lock()
	if m.cmd == cmd {
		m.cmd = nil
		m.exitCh = nil
		m.logFile = nil
	}
	if err != nil && !m.stopping {
		m.state.LastError = err.Error()
	}
	m.saveStateLocked()
	m.mu.Unlock()
	if logFile != nil {
		_ = logFile.Close()
	}
}

func (m *EmbeddedMihomoManager) waitUntilReady(timeout time.Duration) error {
	client := &http.Client{Timeout: 3 * time.Second}
	deadline := time.Now().Add(timeout)
	lastErr := ""
	for time.Now().Before(deadline) {
		req, err := http.NewRequest(http.MethodGet, m.ControllerURL()+"/version", nil)
		if err == nil {
			if secret := strings.TrimSpace(m.cfg.EmbeddedMihomoSecret); secret != "" {
				req.Header.Set("Authorization", "Bearer "+secret)
			}
			resp, reqErr := client.Do(req)
			if reqErr == nil {
				_ = resp.Body.Close()
				if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					return nil
				}
				lastErr = fmt.Sprintf("controller status %d", resp.StatusCode)
			} else {
				lastErr = reqErr.Error()
			}
		} else {
			lastErr = err.Error()
		}
		time.Sleep(500 * time.Millisecond)
	}
	if lastErr == "" {
		lastErr = "mihomo startup timeout"
	}
	return fmt.Errorf(lastErr)
}

func (m *EmbeddedMihomoManager) waitUntilProviderReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	lastErr := ""
	for time.Now().Before(deadline) {
		info, err := os.Stat(m.providerPath())
		if err == nil && !info.IsDir() && info.Size() > 0 {
			groups, groupErr := m.Groups()
			if groupErr == nil {
				for _, group := range groups.Groups {
					if group.Name == m.cfg.EmbeddedMihomoGroupName {
						if len(group.All) > 2 {
							return nil
						}
						lastErr = "provider nodes are not ready yet"
						break
					}
				}
			} else {
				lastErr = groupErr.Error()
			}
		} else if err != nil {
			lastErr = err.Error()
		} else {
			lastErr = "provider cache is empty"
		}
		time.Sleep(500 * time.Millisecond)
	}
	if lastErr == "" {
		lastErr = "mihomo provider refresh timeout"
	}
	return fmt.Errorf(lastErr)
}

func (m *EmbeddedMihomoManager) Start() (EmbeddedMihomoStatus, error) {
	m.mu.Lock()
	if m.exitCh != nil && m.cmd != nil && m.cmd.Process != nil {
		m.mu.Unlock()
		return m.Status(), nil
	}
	subscriptionURL := m.currentSubscriptionLocked()
	if subscriptionURL == "" {
		m.state.LastError = "embedded mihomo subscription url is not configured"
		m.saveStateLocked()
		m.mu.Unlock()
		return m.Status(), fmt.Errorf("embedded mihomo subscription url is not configured")
	}
	binaryPath := m.findBinary()
	if binaryPath == "" {
		m.state.LastError = "mihomo binary not found"
		m.saveStateLocked()
		m.mu.Unlock()
		return m.Status(), fmt.Errorf("mihomo binary not found")
	}
	refreshProvider := m.providerRefreshPending
	if refreshProvider {
		if err := m.clearProviderCacheLocked(); err != nil {
			m.state.LastError = err.Error()
			m.saveStateLocked()
			m.mu.Unlock()
			return m.Status(), err
		}
	}
	if err := m.writeConfigLocked(subscriptionURL); err != nil {
		m.state.LastError = err.Error()
		m.saveStateLocked()
		m.mu.Unlock()
		return m.Status(), err
	}
	logFile, err := os.OpenFile(m.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		m.state.LastError = err.Error()
		m.saveStateLocked()
		m.mu.Unlock()
		return m.Status(), fmt.Errorf("open mihomo log: %w", err)
	}
	cmd := exec.Command(binaryPath, "-d", m.dataDir, "-f", m.configPath)
	cmd.Dir = m.dataDir
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		m.state.LastError = err.Error()
		m.saveStateLocked()
		m.mu.Unlock()
		return m.Status(), fmt.Errorf("start mihomo: %w", err)
	}
	startedAt := time.Now().UTC()
	exitCh := make(chan struct{})
	m.cmd = cmd
	m.exitCh = exitCh
	m.logFile = logFile
	m.stopping = false
	m.state.LastError = ""
	m.state.LastStartedAt = startedAt.Format(time.RFC3339)
	m.saveStateLocked()
	go m.waitLoop(cmd, logFile, exitCh)
	groupName := m.state.CurrentGroup
	proxyName := m.state.CurrentProxy
	m.mu.Unlock()
	if err := m.waitUntilReady(15 * time.Second); err != nil {
		_, _ = m.Stop()
		m.mu.Lock()
		m.state.LastError = err.Error()
		m.saveStateLocked()
		m.mu.Unlock()
		return m.Status(), err
	}
	if refreshProvider {
		if err := m.waitUntilProviderReady(20 * time.Second); err != nil {
			_, _ = m.Stop()
			m.mu.Lock()
			m.state.LastError = err.Error()
			m.saveStateLocked()
			m.mu.Unlock()
			return m.Status(), err
		}
		m.mu.Lock()
		m.providerRefreshPending = false
		m.saveStateLocked()
		m.mu.Unlock()
	}
	if groupName != "" && proxyName != "" {
		_, _ = m.SelectProxy(groupName, proxyName)
	}
	return m.Status(), nil
}

func (m *EmbeddedMihomoManager) Stop() (EmbeddedMihomoStatus, error) {
	m.mu.Lock()
	cmd := m.cmd
	exitCh := m.exitCh
	if cmd == nil || cmd.Process == nil || exitCh == nil {
		m.mu.Unlock()
		return m.Status(), nil
	}
	m.stopping = true
	m.mu.Unlock()
	_ = cmd.Process.Signal(os.Interrupt)
	select {
	case <-exitCh:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		select {
		case <-exitCh:
		case <-time.After(3 * time.Second):
		}
	}
	m.mu.Lock()
	m.stopping = false
	m.state.LastStoppedAt = time.Now().UTC().Format(time.RFC3339)
	m.saveStateLocked()
	m.mu.Unlock()
	return m.Status(), nil
}

func (m *EmbeddedMihomoManager) Restart() (EmbeddedMihomoStatus, error) {
	if _, err := m.Stop(); err != nil {
		return m.Status(), err
	}
	return m.Start()
}

func (m *EmbeddedMihomoManager) Close() {
	_, _ = m.Stop()
}

func (m *EmbeddedMihomoManager) UpdateSubscription(subscriptionURL string, restartIfRunning bool) (EmbeddedMihomoStatus, error) {
	subscriptionURL = strings.TrimSpace(subscriptionURL)
	if !strings.HasPrefix(subscriptionURL, "http://") && !strings.HasPrefix(subscriptionURL, "https://") {
		return m.Status(), fmt.Errorf("subscription url must start with http:// or https://")
	}
	m.mu.Lock()
	changed := subscriptionURL != m.currentSubscriptionLocked()
	m.state.SubscriptionURL = subscriptionURL
	m.state.LastUpdatedAt = time.Now().UTC().Format(time.RFC3339)
	m.state.LastError = ""
	if changed {
		m.providerRefreshPending = true
		m.resetSelectionStateLocked()
		if m.exitCh == nil || m.cmd == nil || m.cmd.Process == nil {
			if err := m.clearProviderCacheLocked(); err != nil {
				m.state.LastError = err.Error()
				m.saveStateLocked()
				m.mu.Unlock()
				return m.Status(), err
			}
		}
	}
	m.saveStateLocked()
	running := m.exitCh != nil && m.cmd != nil && m.cmd.Process != nil
	m.mu.Unlock()
	if running && restartIfRunning {
		return m.Restart()
	}
	return m.Status(), nil
}
