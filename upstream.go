package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type UpstreamClient struct {
	baseURL    string
	httpClient *http.Client
	userAgent  string
	proxyMode  string
	proxy      *EmbeddedMihomoManager
	fallbackProxyURL string
}

type UpstreamRouteProbe struct {
	At                string `json:"at"`
	ProxyMode         string `json:"proxy_mode"`
	EffectiveProxyURL string `json:"effective_proxy_url"`
	UsingProxy        bool   `json:"using_proxy"`
	IP                string `json:"ip"`
	Loc               string `json:"loc"`
	Blocked           bool   `json:"blocked"`
	Error             string `json:"error,omitempty"`
}

func NewUpstreamClient(cfg Config, proxy *EmbeddedMihomoManager) *UpstreamClient {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = buildProxyFunc(cfg, proxy)

	return &UpstreamClient{
		baseURL: cfg.UpstreamBaseURL,
		httpClient: &http.Client{
			Timeout:   cfg.RequestTimeout,
			Transport: transport,
		},
		userAgent: cfg.UserAgent,
		proxyMode: cfg.ProxyBackendMode,
		proxy:     proxy,
		fallbackProxyURL: strings.TrimSpace(cfg.HTTPProxy),
	}
}

func buildProxyFunc(cfg Config, proxy *EmbeddedMihomoManager) func(*http.Request) (*url.URL, error) {
	fallback := strings.TrimSpace(cfg.HTTPProxy)
	return func(*http.Request) (*url.URL, error) {
		if cfg.UsesEmbeddedMihomo() && proxy != nil {
			if current := strings.TrimSpace(proxy.ProxyURL()); current != "" {
				return url.Parse(current)
			}
			return nil, nil
		}
		if fallback == "" {
			return nil, nil
		}
		return url.Parse(fallback)
	}
}

func (c *UpstreamClient) EffectiveProxyURL() string {
	if c.proxyMode == proxyBackendEmbeddedMihomo {
		if c.proxy == nil {
			return ""
		}
		return strings.TrimSpace(c.proxy.ProxyURL())
	}
	return strings.TrimSpace(c.fallbackProxyURL)
}

func (c *UpstreamClient) do(req *http.Request) (*http.Response, error) {
	if c.proxyMode == proxyBackendEmbeddedMihomo {
		if c.proxy == nil {
			return nil, fmt.Errorf("embedded mihomo proxy is not configured")
		}
		if !c.proxy.IsRunning() {
			if lastErr := strings.TrimSpace(c.proxy.LastError()); lastErr != "" {
				return nil, fmt.Errorf("embedded mihomo proxy is not running: %s", lastErr)
			}
			return nil, fmt.Errorf("embedded mihomo proxy is not running")
		}
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send upstream request: %w", err)
	}
	return resp, nil
}

func (c *UpstreamClient) ProbeRoute(ctx context.Context) (UpstreamRouteProbe, error) {
	probe := UpstreamRouteProbe{
		At:                time.Now().UTC().Format(time.RFC3339),
		ProxyMode:         c.proxyMode,
		EffectiveProxyURL: c.EffectiveProxyURL(),
	}
	probe.UsingProxy = probe.EffectiveProxyURL != ""

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://cloudflare.com/cdn-cgi/trace", nil)
	if err != nil {
		probe.Error = err.Error()
		return probe, fmt.Errorf("create upstream route probe request: %w", err)
	}
	req.Header.Set("Accept", "text/plain")
	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.do(req)
	if err != nil {
		probe.Error = err.Error()
		return probe, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		probe.Error = err.Error()
		return probe, fmt.Errorf("read upstream route probe response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		probe.Error = fmt.Sprintf("probe status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
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
	return probe, nil
}

func (c *UpstreamClient) StartRun(ctx context.Context, authToken, agentID string) (string, error) {
	payload := map[string]any{
		"action":  "START",
		"agentId": agentID,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal start run request: %w", err)
	}

	resp, err := c.doJSON(ctx, authToken, "/api/v1/agent-runs", body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read start run response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("start run failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(responseBody)))
	}

	var parsed struct {
		RunID string `json:"runId"`
	}
	if err := json.Unmarshal(responseBody, &parsed); err != nil {
		return "", fmt.Errorf("decode start run response: %w", err)
	}
	if strings.TrimSpace(parsed.RunID) == "" {
		return "", fmt.Errorf("start run response missing runId: %s", strings.TrimSpace(string(responseBody)))
	}

	return parsed.RunID, nil
}

func (c *UpstreamClient) FinishRun(ctx context.Context, authToken, runID string, totalSteps int) error {
	payload := map[string]any{
		"action":        "FINISH",
		"runId":         runID,
		"status":        "completed",
		"totalSteps":    totalSteps,
		"directCredits": 0,
		"totalCredits":  0,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal finish run request: %w", err)
	}

	resp, err := c.doJSON(ctx, authToken, "/api/v1/agent-runs", body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read finish run response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("finish run failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(responseBody)))
	}
	return nil
}

func (c *UpstreamClient) ChatCompletions(ctx context.Context, authToken string, body []byte) (*http.Response, []byte, error) {
	resp, err := c.doJSON(ctx, authToken, "/api/v1/chat/completions", body)
	if err != nil {
		return nil, nil, err
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil, nil
	}

	responseBody, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		return nil, nil, fmt.Errorf("read upstream error response: %w", readErr)
	}
	return resp, responseBody, nil
}

func (c *UpstreamClient) doJSON(ctx context.Context, authToken, path string, body []byte) (*http.Response, error) {
	requestURL, err := url.JoinPath(c.baseURL, path)
	if err != nil {
		return nil, fmt.Errorf("build upstream url: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+authToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func retryAfterDuration(headerValue string) time.Duration {
	headerValue = strings.TrimSpace(headerValue)
	if headerValue == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(headerValue); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	return 0
}
