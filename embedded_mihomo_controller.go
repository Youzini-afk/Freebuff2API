package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

func (m *EmbeddedMihomoManager) controllerRequest(ctx context.Context, method, path string, body []byte) ([]byte, int, error) {
	if !m.IsRunning() {
		return nil, 0, fmt.Errorf("mihomo is not running")
	}
	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, m.ControllerURL()+path, reader)
	if err != nil {
		return nil, 0, fmt.Errorf("create controller request: %w", err)
	}
	if secret := strings.TrimSpace(m.cfg.EmbeddedMihomoSecret); secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("send controller request: %w", err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read controller response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return payload, resp.StatusCode, fmt.Errorf("controller request failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(payload)))
	}
	return payload, resp.StatusCode, nil
}

func (m *EmbeddedMihomoManager) Groups() (EmbeddedMihomoGroups, error) {
	if !m.IsRunning() {
		return EmbeddedMihomoGroups{
			Running:       false,
			SelectedGroup: m.cfg.EmbeddedMihomoGroupName,
			Groups:        nil,
		}, nil
	}
	payload, _, err := m.controllerRequest(context.Background(), http.MethodGet, "/proxies", nil)
	if err != nil {
		return EmbeddedMihomoGroups{}, err
	}
	var decoded struct {
		Proxies map[string]struct {
			Type  string   `json:"type"`
			Now   string   `json:"now"`
			All   []string `json:"all"`
			Alive bool     `json:"alive"`
		} `json:"proxies"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return EmbeddedMihomoGroups{}, fmt.Errorf("decode controller proxies: %w", err)
	}
	groups := make([]EmbeddedMihomoGroup, 0, len(decoded.Proxies))
	for name, item := range decoded.Proxies {
		lowerType := strings.ToLower(strings.TrimSpace(item.Type))
		if len(item.All) == 0 && !strings.Contains(lowerType, "selector") && !strings.Contains(lowerType, "urltest") && !strings.Contains(lowerType, "fallback") && !strings.Contains(lowerType, "loadbalance") && !strings.Contains(lowerType, "relay") {
			continue
		}
		groups = append(groups, EmbeddedMihomoGroup{
			Name:  name,
			Type:  item.Type,
			Now:   item.Now,
			All:   append([]string(nil), item.All...),
			Alive: item.Alive,
		})
	}
	sort.Slice(groups, func(i, j int) bool {
		return groups[i].Name < groups[j].Name
	})
	m.mu.RLock()
	selectedGroup := strings.TrimSpace(m.state.CurrentGroup)
	m.mu.RUnlock()
	if selectedGroup == "" {
		selectedGroup = m.cfg.EmbeddedMihomoGroupName
	}
	if len(groups) > 0 && selectedGroup == "" {
		selectedGroup = groups[0].Name
	}
	for _, group := range groups {
		if group.Name == selectedGroup {
			return EmbeddedMihomoGroups{
				Running:       true,
				SelectedGroup: selectedGroup,
				Groups:        groups,
			}, nil
		}
	}
	if len(groups) > 0 {
		selectedGroup = groups[0].Name
	}
	return EmbeddedMihomoGroups{
		Running:       true,
		SelectedGroup: selectedGroup,
		Groups:        groups,
	}, nil
}

func (m *EmbeddedMihomoManager) SelectProxy(groupName, proxyName string) (EmbeddedMihomoGroups, error) {
	if !m.IsRunning() {
		return EmbeddedMihomoGroups{}, fmt.Errorf("mihomo is not running")
	}
	groupName = strings.TrimSpace(groupName)
	proxyName = strings.TrimSpace(proxyName)
	if groupName == "" {
		return EmbeddedMihomoGroups{}, fmt.Errorf("group name is required")
	}
	if proxyName == "" {
		return EmbeddedMihomoGroups{}, fmt.Errorf("proxy name is required")
	}
	body, err := json.Marshal(map[string]string{"name": proxyName})
	if err != nil {
		return EmbeddedMihomoGroups{}, fmt.Errorf("marshal controller request: %w", err)
	}
	if _, _, err := m.controllerRequest(context.Background(), http.MethodPut, "/proxies/"+url.PathEscape(groupName), body); err != nil {
		return EmbeddedMihomoGroups{}, err
	}
	m.mu.Lock()
	m.state.CurrentGroup = groupName
	m.state.CurrentProxy = proxyName
	m.state.LastError = ""
	m.saveStateLocked()
	m.mu.Unlock()
	return m.Groups()
}

func (m *EmbeddedMihomoManager) ProbeExit() (EmbeddedMihomoProbe, error) {
	if !m.IsRunning() {
		return EmbeddedMihomoProbe{}, fmt.Errorf("mihomo is not running")
	}
	proxyURL, err := url.Parse(m.MixedProxyURL())
	if err != nil {
		return EmbeddedMihomoProbe{}, fmt.Errorf("parse mixed proxy url: %w", err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyURL(proxyURL)
	client := &http.Client{Timeout: 8 * time.Second, Transport: transport}
	resp, err := client.Get("https://cloudflare.com/cdn-cgi/trace")
	probe := EmbeddedMihomoProbe{At: time.Now().UTC().Format(time.RFC3339)}
	if err != nil {
		probe.Error = err.Error()
		m.setProbeState(probe)
		return probe, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		probe.Error = err.Error()
		m.setProbeState(probe)
		return probe, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		probe.Error = fmt.Sprintf("probe status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		m.setProbeState(probe)
		return probe, fmt.Errorf(probe.Error)
	}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "ip=") {
			probe.IP = strings.TrimSpace(strings.TrimPrefix(line, "ip="))
		}
		if strings.HasPrefix(line, "loc=") {
			probe.Loc = strings.TrimSpace(strings.TrimPrefix(line, "loc="))
		}
	}
	probe.Blocked = probe.Loc == "CN" || probe.Loc == "HK"
	m.setProbeState(probe)
	return probe, nil
}

func (m *EmbeddedMihomoManager) setProbeState(probe EmbeddedMihomoProbe) {
	m.mu.Lock()
	m.state.LastProbeAt = probe.At
	m.state.LastProbeIP = probe.IP
	m.state.LastProbeLoc = probe.Loc
	m.state.LastProbeError = probe.Error
	m.state.LastProbeBlocked = probe.Blocked
	m.saveStateLocked()
	m.mu.Unlock()
}

func (m *EmbeddedMihomoManager) Logs(limit int) []string {
	if limit <= 0 {
		limit = 100
	}
	data, err := os.ReadFile(m.logPath)
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	trimmed := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		trimmed = append(trimmed, line)
	}
	if len(trimmed) <= limit {
		return trimmed
	}
	return trimmed[len(trimmed)-limit:]
}
