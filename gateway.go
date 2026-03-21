package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

const (
	maxRetries    = 5
	retryBaseWait = 1 * time.Second
)

// isTransientError returns true for errors caused by server restarts (EOF, connection refused/reset).
func isTransientError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "EOF") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "connection reset")
}

// doWithRetry executes an HTTP request function with retry on transient errors.
func doWithRetry(fn func() (*http.Response, error)) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		resp, err := fn()
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !isTransientError(err) || attempt == maxRetries {
			return nil, err
		}
		wait := retryBaseWait * time.Duration(1<<uint(attempt))
		log.Printf("[gateway] transient error (attempt %d/%d), retrying in %v: %v", attempt+1, maxRetries, wait, err)
		time.Sleep(wait)
	}
	return nil, lastErr
}

// GatewayClient forwards messages to the mini-claude-bot gateway API
// instead of spawning Claude CLI directly.
type GatewayClient struct {
	baseURL    string
	botID      string // multi-tenant isolation identifier
	httpClient *http.Client
}

type gatewaySendRequest struct {
	ChatID   string `json:"chat_id"`
	Message  string `json:"message"`
	BotID    string `json:"bot_id,omitempty"`
	UserID   string `json:"user_id,omitempty"`
	Username string `json:"username,omitempty"`
}

type gatewaySendResponse struct {
	Response   string `json:"response"`
	SessionKey string `json:"session_key"`
}

func NewGatewayClient(baseURL string, botID string) *GatewayClient {
	return &GatewayClient{
		baseURL: baseURL,
		botID:   botID,
		httpClient: &http.Client{
			Timeout: 30 * time.Minute, // allow time for OOM retries (was 16min)
			Transport: &http.Transport{
				MaxIdleConns:        10,
				MaxIdleConnsPerHost: 5,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

func (g *GatewayClient) Send(chatID, message, userID, username string) (string, error) {
	body, err := json.Marshal(gatewaySendRequest{
		ChatID:   chatID,
		Message:  message,
		BotID:    g.botID,
		UserID:   userID,
		Username: username,
	})
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	// Check if this is a background session (chat_id starts with "bg-")
	isBackgroundSession := strings.HasPrefix(chatID, "bg-")

	// Use appropriate timeout: no timeout for background sessions
	var client *http.Client
	if isBackgroundSession {
		client = &http.Client{
			Timeout: 0, // no timeout for background sessions
			Transport: &http.Transport{
				DisableKeepAlives: true,
			},
		}
		log.Printf("[gateway] Using no timeout for background session %s", chatID)
	} else {
		client = g.httpClient // default 30min timeout
	}

	resp, err := doWithRetry(func() (*http.Response, error) {
		return client.Post(
			g.baseURL+"/api/gateway/send",
			"application/json",
			bytes.NewReader(body),
		)
	})
	if err != nil {
		return "", fmt.Errorf("gateway request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("gateway HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var result gatewaySendResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}

	return result.Response, nil
}

// sseEvent represents a single server-sent event from the streaming endpoint.
type sseEvent struct {
	Type    string `json:"type"`
	Content string `json:"content"`
}

// SendStream POSTs to the gateway streaming endpoint and reads the response as
// an SSE stream. For each parsed event, onEvent is called with the event type
// and content. Text events are accumulated and the full response is returned
// when a "done" event is received.
func (g *GatewayClient) SendStream(chatID, message, userID, username string, onEvent func(eventType, content string)) (string, error) {
	body, err := json.Marshal(gatewaySendRequest{
		ChatID:   chatID,
		Message:  message,
		BotID:    g.botID,
		UserID:   userID,
		Username: username,
	})
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	resp, err := doWithRetry(func() (*http.Response, error) {
		return g.httpClient.Post(
			g.baseURL+"/api/gateway/send-stream",
			"application/json",
			bytes.NewReader(body),
		)
	})
	if err != nil {
		return "", fmt.Errorf("stream request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("gateway HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var fullResponse strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")

		var evt sseEvent
		if err := json.Unmarshal([]byte(data), &evt); err != nil {
			log.Printf("[gateway] failed to parse SSE event: %v (data: %s)", err, data)
			continue
		}

		if onEvent != nil {
			onEvent(evt.Type, evt.Content)
		}

		switch evt.Type {
		case "text":
			fullResponse.WriteString(evt.Content)
		case "done":
			return evt.Content, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("stream read error: %w", err)
	}

	// Stream ended without a "done" event — return what we accumulated.
	if fullResponse.Len() > 0 {
		// Fix C2: Treat partial response as success — harness markers may still be present
		log.Printf("[gateway] stream ended without done event, returning %d bytes of accumulated text", fullResponse.Len())
		return fullResponse.String(), nil
	}
	return "", fmt.Errorf("stream ended without any events")
}

type gatewayBackgroundRequest struct {
	ChatID    string `json:"chat_id"`
	Message   string `json:"message"`
	BotToken  string `json:"bot_token"`
	BotID     string `json:"bot_id,omitempty"`
	ProjectID string `json:"project_id,omitempty"`
}

type gatewayBackgroundResponse struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

type gatewayBackgroundStatus struct {
	Status         string  `json:"status"`
	Message        string  `json:"message,omitempty"`
	ElapsedSeconds float64 `json:"elapsed_seconds,omitempty"`
	Result         string  `json:"result,omitempty"`
	StartedAt      float64 `json:"started_at,omitempty"`
}

func (g *GatewayClient) SendBackground(chatID, message, botToken string) (*gatewayBackgroundResponse, error) {
	body, err := json.Marshal(gatewayBackgroundRequest{
		ChatID:   chatID,
		Message:  message,
		BotToken: botToken,
		BotID:    g.botID,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	resp, err := doWithRetry(func() (*http.Response, error) {
		return g.httpClient.Post(
			g.baseURL+"/api/gateway/send-background",
			"application/json",
			bytes.NewReader(body),
		)
	})
	if err != nil {
		return nil, fmt.Errorf("background request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	var result gatewayBackgroundResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	return &result, nil
}

func (g *GatewayClient) GetBackgroundStatus(chatID string) (*gatewayBackgroundStatus, error) {
	resp, err := g.httpClient.Get(g.baseURL + "/api/gateway/background-status/" + chatID + "?bot_id=" + g.botID)
	if err != nil {
		return nil, fmt.Errorf("status request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	var result gatewayBackgroundStatus
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	return &result, nil
}

// Harness status types
type harnessPhaseStatus struct {
	Total      int `json:"total"`
	Done       int `json:"done"`
	InProgress int `json:"in_progress"`
	Blocked    int `json:"blocked"`
	Pending    int `json:"pending"`
}

type harnessInfo struct {
	ProjectName  string                        `json:"project_name"`
	CurrentPhase string                        `json:"current_phase"`
	Total        int                           `json:"total"`
	Done         int                           `json:"done"`
	InProgress   int                           `json:"in_progress"`
	Blocked      int                           `json:"blocked"`
	Pending      int                           `json:"pending"`
	Phases       map[string]harnessPhaseStatus `json:"phases"`
}

type gatewayHarnessStatusResponse struct {
	BgStatus       string       `json:"bg_status"`
	ElapsedSeconds float64      `json:"elapsed_seconds"`
	ChainDepth     int          `json:"chain_depth"`
	ProjectID      string       `json:"project_id"`
	CWD            string       `json:"cwd"`
	Harness        *harnessInfo `json:"harness"`
}

type gatewayHarnessStatusListResponse struct {
	Jobs []gatewayHarnessStatusResponse `json:"jobs"`
}

func (g *GatewayClient) GetAllHarnessStatus(chatID string) ([]gatewayHarnessStatusResponse, error) {
	resp, err := g.httpClient.Get(g.baseURL + "/api/gateway/harness-status/" + chatID + "?bot_id=" + g.botID)
	if err != nil {
		return nil, fmt.Errorf("harness status request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	var result gatewayHarnessStatusListResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	return result.Jobs, nil
}

// GetHarnessStatus is kept for backward compatibility; returns first job or nil.
func (g *GatewayClient) GetHarnessStatus(chatID string) (*gatewayHarnessStatusResponse, error) {
	jobs, err := g.GetAllHarnessStatus(chatID)
	if err != nil {
		return nil, err
	}
	if len(jobs) == 0 {
		return &gatewayHarnessStatusResponse{BgStatus: "idle"}, nil
	}
	return &jobs[0], nil
}

type gatewayCleanupResponse struct {
	Cleaned  int      `json:"cleaned"`
	Archived int      `json:"archived"`
	Skipped  int      `json:"skipped"`
	Details  []string `json:"details"`
}

func (g *GatewayClient) CleanupBackgroundTasks(chatID string) (*gatewayCleanupResponse, error) {
	resp, err := g.httpClient.Post(
		g.baseURL+"/api/gateway/cleanup/"+chatID+"?bot_id="+g.botID,
		"application/json",
		bytes.NewReader([]byte("{}")),
	)
	if err != nil {
		return nil, fmt.Errorf("cleanup request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("cleanup HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var result gatewayCleanupResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	return &result, nil
}

// chatMessage represents a single message from the chat session API.
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// GetRecentMessages fetches the last N messages for a chat session.
func (g *GatewayClient) GetRecentMessages(chatID string, limit int) ([]chatMessage, error) {
	sessionID := fmt.Sprintf("gw-%s-%s", g.botID, chatID)
	url := fmt.Sprintf("%s/api/chat/sessions/%s?limit=%d", g.baseURL, sessionID, limit)

	resp, err := g.httpClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("get recent messages: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		return nil, nil // no history yet
	}

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("chat API HTTP %d: %s", resp.StatusCode, string(body))
	}

	var msgs []chatMessage
	if err := json.NewDecoder(resp.Body).Decode(&msgs); err != nil {
		return nil, fmt.Errorf("decode messages: %w", err)
	}
	return msgs, nil
}

// NirmanaResponse is the response from the nirmana mode toggle endpoint.
type NirmanaResponse struct {
	Status   string `json:"status"`
	Message  string `json:"message"`
	Briefing string `json:"briefing"`
}

// NirmanaStateResponse is the response from the nirmana state query endpoint.
type NirmanaStateResponse struct {
	NirmanaMode        bool     `json:"nirmana_mode"`
	NirmanaActivatedAt float64  `json:"nirmana_activated_at"`
	AwayDuration       *float64 `json:"away_duration_seconds"`
}

// SetNirmanaMode toggles nirmana mode (away/back) for a chat.
func (g *GatewayClient) SetNirmanaMode(chatID string, action string) (*NirmanaResponse, error) {
	body, err := json.Marshal(map[string]string{
		"chat_id": chatID,
		"bot_id":  g.botID,
		"action":  action,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	resp, err := doWithRetry(func() (*http.Response, error) {
		return g.httpClient.Post(
			g.baseURL+"/api/gateway/nirmana",
			"application/json",
			bytes.NewReader(body),
		)
	})
	if err != nil {
		return nil, fmt.Errorf("nirmana request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("nirmana HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var result NirmanaResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	return &result, nil
}

// GetNirmanaState returns current nirmana state for a chat.
func (g *GatewayClient) GetNirmanaState(chatID string) (*NirmanaStateResponse, error) {
	resp, err := g.httpClient.Get(g.baseURL + "/api/gateway/nirmana/" + chatID + "?bot_id=" + g.botID)
	if err != nil {
		return nil, fmt.Errorf("nirmana state request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	var result NirmanaStateResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	return &result, nil
}

func (g *GatewayClient) Stop(chatID string) error {
	body, _ := json.Marshal(map[string]string{"chat_id": chatID, "bot_id": g.botID})
	resp, err := g.httpClient.Post(
		g.baseURL+"/api/gateway/stop",
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}
