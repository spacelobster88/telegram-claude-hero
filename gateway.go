package main

import (
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
				DisableKeepAlives: true, // prevent EOF on stale keep-alive connections
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

type gatewayBackgroundRequest struct {
	ChatID   string `json:"chat_id"`
	Message  string `json:"message"`
	BotToken string `json:"bot_token"`
	BotID    string `json:"bot_id,omitempty"`
	PlanID    string `json:"plan_id,omitempty"`
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
