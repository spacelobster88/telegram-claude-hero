package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// GatewayClient forwards messages to the mini-claude-bot gateway API
// instead of spawning Claude CLI directly.
type GatewayClient struct {
	baseURL    string
	httpClient *http.Client
}

type gatewaySendRequest struct {
	ChatID   string `json:"chat_id"`
	Message  string `json:"message"`
	UserID   string `json:"user_id,omitempty"`
	Username string `json:"username,omitempty"`
}

type gatewaySendResponse struct {
	Response   string `json:"response"`
	SessionKey string `json:"session_key"`
}

func NewGatewayClient(baseURL string) *GatewayClient {
	return &GatewayClient{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 16 * time.Minute, // longer than server-side Claude timeout (15min)
		},
	}
}

func (g *GatewayClient) Send(chatID, message, userID, username string) (string, error) {
	body, err := json.Marshal(gatewaySendRequest{
		ChatID:   chatID,
		Message:  message,
		UserID:   userID,
		Username: username,
	})
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	resp, err := g.httpClient.Post(
		g.baseURL+"/api/gateway/send",
		"application/json",
		bytes.NewReader(body),
	)
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

func (g *GatewayClient) Stop(chatID string) error {
	body, _ := json.Marshal(map[string]string{"chat_id": chatID})
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
