package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const maxMessageLen = 4096

type Bot struct {
	api          *tgbotapi.BotAPI
	session      *Session   // only used in local mode
	activeChatID int64      // only used in local mode
	mu           sync.Mutex
	gateway      *GatewayClient // nil in local mode
}

func NewBot(token string, gatewayURL string) (*Bot, error) {
	api, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return nil, fmt.Errorf("creating bot: %w", err)
	}
	log.Printf("Authorized as @%s", api.Self.UserName)

	b := &Bot{api: api}
	if gatewayURL != "" {
		b.gateway = NewGatewayClient(gatewayURL)
		log.Printf("Gateway mode: %s", gatewayURL)
	} else {
		log.Printf("Local mode: single session")
	}
	return b, nil
}

func (b *Bot) Run() {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := b.api.GetUpdatesChan(u)

	for update := range updates {
		if update.Message == nil {
			continue
		}
		b.handleMessage(update.Message)
	}
}

func (b *Bot) Stop() {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.session != nil {
		b.session.Stop()
		b.session = nil
	}
	b.api.StopReceivingUpdates()
}

func (b *Bot) handleMessage(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	log.Printf("[recv] chat=%d user=%s (@%s) text=%q",
		chatID, msg.From.FirstName, msg.From.UserName, msg.Text)

	if msg.Command() == "stop" {
		b.handleStop(chatID, msg)
		return
	}

	if msg.Command() == "start" {
		b.handleStart(chatID, msg)
		return
	}

	text := msg.Text
	if text == "" {
		return
	}

	if b.gateway != nil {
		b.handleTextGateway(chatID, text, msg)
	} else {
		b.handleTextLocal(chatID, text)
	}
}

func (b *Bot) handleStop(chatID int64, msg *tgbotapi.Message) {
	if b.gateway != nil {
		chatIDStr := fmt.Sprintf("%d", chatID)
		if err := b.gateway.Stop(chatIDStr); err != nil {
			b.send(chatID, fmt.Sprintf("Error stopping session: %v", err))
			return
		}
		b.send(chatID, "Session stopped.")
		return
	}

	// Local mode
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.session == nil {
		b.send(chatID, "No active session.")
		return
	}

	b.session.Stop()
	b.session = nil
	b.send(chatID, "Session stopped.")
}

func (b *Bot) handleStart(chatID int64, msg *tgbotapi.Message) {
	if b.gateway != nil {
		chatIDStr := fmt.Sprintf("%d", chatID)
		// Stop existing session to start fresh
		b.gateway.Stop(chatIDStr)
		b.send(chatID, "Session started. Send me your prompts!")
		return
	}

	// Local mode
	b.mu.Lock()
	if b.session != nil {
		b.session.Stop()
	}
	b.session = NewSession()
	b.activeChatID = chatID
	b.mu.Unlock()
	b.send(chatID, "Session started. Send me your prompts!")
}

// handleHarnessQuery queries harness progress from chat history
func (b *Bot) handleHarnessQuery(chatID int64, msg *tgbotapi.Message) {
	count := 5
	args := msg.CommandArguments()
	if len(args) > 0 {
		if n, err := strconv.Atoi(string(args[0])); err == nil && n > 0 && n <= 20 {
			count = n
		}
	}

	b.send(chatID, fmt.Sprintf("🔍 Querying last %d harness progress messages...", count))

	go func() {
		progress := b.queryHarnessProgress(count)
		if progress == "" {
			b.send(chatID, "No harness progress found in recent messages.")
		} else {
			b.sendLong(chatID, progress)
		}
	}()
}

// queryHarnessProgress fetches recent assistant messages from mini-claude-bot
// and extracts harness progress information
func (b *Bot) queryHarnessProgress(count int) string {
	if b.gateway == nil {
		return "[ERROR] Harness progress requires gateway mode"
	}

	url := b.gateway.baseURL + "/api/chat/search?limit=" + strconv.Itoa(count*10)

	resp, err := http.Get(url)
	if err != nil {
		log.Printf("Failed to fetch chat history: %v", err)
		return fmt.Sprintf("[ERROR] Failed to fetch progress: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Sprintf("[ERROR] Gateway returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Sprintf("[ERROR] Failed to read response: %v", err)
	}

	type chatMessage struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}

	type chatResponse struct {
		Result []chatMessage `json:"result"`
	}

	var chatResp chatResponse
	if err := json.Unmarshal(body, &chatResp); err != nil {
		log.Printf("Failed to parse chat response: %v", err)
		return fmt.Sprintf("[ERROR] Failed to parse response: %v", err)
	}

	if len(chatResp.Result) == 0 {
		return "No harness progress found."
	}

	// Collect harness-related messages (assistant messages containing progress keywords)
	var progressMessages []string
	harnessKeywords := []string{"harness", "progress", "phase", "阶段", "进度", "task", "任务", "完成", "completed", "implemented", "implemented", "fix", "修复", "persistence", "持久化", "MCP", "SQLite"}

	for _, msg := range chatResp.Result {
		if msg.Role == "assistant" {
			content := msg.Content
			for _, keyword := range harnessKeywords {
				if strings.Contains(strings.ToLower(content), strings.ToLower(keyword)) {
					progressMessages = append(progressMessages, content)
					break
				}
			}
		}
	}

	if len(progressMessages) == 0 {
		return "No harness progress found."
	}

	// Format and return
	var builder strings.Builder
	builder.WriteString("📊 **Harness Progress Report**\n\n")

	for i, msg := range progressMessages {
		builder.WriteString(fmt.Sprintf("**%d.** %s\n\n", i+1, msg))

		// Limit to avoid huge messages
		if i >= count-1 {
			break
		}
	}

	return builder.String()
}

// startTypingLoop sends a "typing" indicator every 4 seconds until the
// returned stop channel is closed.
func (b *Bot) startTypingLoop(chatID int64) chan struct{} {
	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				typing := tgbotapi.NewChatAction(chatID, tgbotapi.ChatTyping)
				b.api.Send(typing)
			}
		}
	}()
	return stop
}

func isBackgroundMessage(text string) bool {
	lower := strings.ToLower(text)
	patterns := []string{"harness-loop", "/harness", "centurion"}
	for _, p := range patterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

func isHarnessStatusQuery(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	return lower == "harness status" || lower == "/harness_status"
}

// handleTextGateway forwards messages to the mini-claude-bot gateway.
// No single-chat restriction — each chat_id gets its own isolated session.
func (b *Bot) handleTextGateway(chatID int64, text string, msg *tgbotapi.Message) {
	// Check harness status query first
	if isHarnessStatusQuery(text) {
		b.handleHarnessStatus(chatID)
		return
	}

	// Check if this is a background message
	if isBackgroundMessage(text) {
		b.handleBackgroundMessage(chatID, text)
		return
	}

	// Regular message flow
	typing := tgbotapi.NewChatAction(chatID, tgbotapi.ChatTyping)
	b.api.Send(typing)

	chatIDStr := fmt.Sprintf("%d", chatID)
	userID := fmt.Sprintf("%d", msg.From.ID)
	username := msg.From.UserName

	go func() {
		stopTyping := b.startTypingLoop(chatID)
		response, err := b.gateway.Send(chatIDStr, text, userID, username)
		close(stopTyping)
		if err != nil {
			b.send(chatID, fmt.Sprintf("Error: gateway request failed: %v", err))
			return
		}
		// Only treat actual empty string as "empty response"
		if response == "" {
			b.send(chatID, "(empty response)")
			return
		}
		// Don't send empty response if it's an error message or has content
		if response != "" {
			b.sendLong(chatID, response)
		}
	}()
}

func (b *Bot) handleBackgroundMessage(chatID int64, text string) {
	chatIDStr := fmt.Sprintf("%d", chatID)

	typing := tgbotapi.NewChatAction(chatID, tgbotapi.ChatTyping)
	b.api.Send(typing)

	result, err := b.gateway.SendBackground(chatIDStr, text, b.api.Token)
	if err != nil {
		b.send(chatID, fmt.Sprintf("Error starting background task: %v", err))
		return
	}

	if result.Status == "rejected" {
		b.send(chatID, fmt.Sprintf("Background task rejected: %s", result.Reason))
		return
	}

	b.send(chatID, "Started in background. You can keep chatting. I'll send results when done.")
}

func (b *Bot) handleHarnessStatus(chatID int64) {
	chatIDStr := fmt.Sprintf("%d", chatID)

	status, err := b.gateway.GetBackgroundStatus(chatIDStr)
	if err != nil {
		b.send(chatID, fmt.Sprintf("Error getting status: %v", err))
		return
	}

	switch status.Status {
	case "idle":
		b.send(chatID, "No background task running.")
	case "running":
		elapsed := int(status.ElapsedSeconds)
		mins := elapsed / 60
		secs := elapsed % 60
		preview := status.Message
		if len(preview) > 100 {
			preview = preview[:100] + "..."
		}
		b.send(chatID, fmt.Sprintf("Background task running (%dm%ds elapsed)\nMessage: %s", mins, secs, preview))
	case "completed":
		preview := status.Result
		if len(preview) > 200 {
			preview = preview[:200] + "..."
		}
		b.send(chatID, fmt.Sprintf("Last background task completed.\nResult: %s", preview))
	case "failed":
		b.send(chatID, fmt.Sprintf("Last background task failed.\nResult: %s", status.Result))
	default:
		b.send(chatID, fmt.Sprintf("Background status: %s", status.Status))
	}
}

// handleTextLocal is the original single-session behavior.
func (b *Bot) handleTextLocal(chatID int64, text string) {
	b.mu.Lock()
	// Auto-start session on first message
	if b.session == nil {
		b.session = NewSession()
		b.activeChatID = chatID
		log.Printf("[bot] auto-started session for chat=%d", chatID)
	}
	session := b.session
	activeChatID := b.activeChatID
	b.mu.Unlock()

	if chatID != activeChatID {
		b.send(chatID, "Another chat has an active session.")
		return
	}

	if session.IsBusy() {
		b.send(chatID, "Still processing the previous message, please wait...")
		return
	}

	typing := tgbotapi.NewChatAction(chatID, tgbotapi.ChatTyping)
	b.api.Send(typing)

	go func() {
		response, err := session.Send(context.Background(), text)
		if err != nil {
			b.send(chatID, fmt.Sprintf("Error: %v", err))
			return
		}
		if response == "" {
			b.send(chatID, "(empty response)")
			return
		}
		b.sendLong(chatID, response)
	}()
}

func (b *Bot) send(chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	if _, err := b.api.Send(msg); err != nil {
		log.Printf("Failed to send message: %v", err)
	}
}

func (b *Bot) sendLong(chatID int64, text string) {
	chunks := splitMessage(text)
	for _, chunk := range chunks {
		b.send(chatID, chunk)
	}
}

func splitMessage(text string) []string {
	if len(text) <= maxMessageLen {
		return []string{text}
	}

	var chunks []string
	for len(text) > 0 {
		if len(text) <= maxMessageLen {
			chunks = append(chunks, text)
			break
		}

		cutoff := maxMessageLen
		if idx := strings.LastIndex(text[:cutoff], "\n"); idx > 0 {
			cutoff = idx + 1
		}
		chunks = append(chunks, text[:cutoff])
		text = text[cutoff:]
	}
	return chunks
}
