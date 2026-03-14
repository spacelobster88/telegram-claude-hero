package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const maxMessageLen = 4096

type PendingPlan struct {
	ID        string
	Plan      string
	CreatedAt time.Time
}

type WaitingForSelection struct {
	ChatID     int64
	Plans      []PendingPlan
	ExpiryTime time.Time
}

type Bot struct {
	api               *tgbotapi.BotAPI
	session           *Session   // only used in local mode
	activeChatID      int64      // only used in local mode
	mu                sync.Mutex
	gateway           *GatewayClient // nil in local mode
	pendingPlans      map[int64][]PendingPlan      // chat_id -> pending plans
	selectionWaiters  map[int64]WaitingForSelection // chat_id -> waiting state
}

func NewBot(token string, gatewayURL string, botID string) (*Bot, error) {
	api, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return nil, fmt.Errorf("creating bot: %w", err)
	}
	log.Printf("Authorized as @%s", api.Self.UserName)

	// Auto-derive botID from bot username if not explicitly set
	if botID == "" {
		botID = api.Self.UserName
	}

	b := &Bot{
		api:              api,
		pendingPlans:     make(map[int64][]PendingPlan),
		selectionWaiters: make(map[int64]WaitingForSelection),
	}
	if gatewayURL != "" {
		b.gateway = NewGatewayClient(gatewayURL, botID)
		log.Printf("Gateway mode: %s (bot_id=%s)", gatewayURL, botID)
	} else {
		log.Printf("Local mode: single session")
	}
	return b, nil
}

func (b *Bot) Run() {
	b.registerCommands()

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
	log.Printf("[recv] chat=%d user=%s (@%s) text=%q doc=%v photo=%d voice=%v",
		chatID, msg.From.FirstName, msg.From.UserName, msg.Text,
		msg.Document != nil, len(msg.Photo), msg.Voice != nil)

	// Bot-level commands (intercepted, never reach Claude)
	switch msg.Command() {
	case "stop":
		b.handleStop(chatID, msg)
		return
	case "start":
		b.handleStart(chatID, msg)
		return
	case "status":
		b.handleHarnessStatus(chatID)
		return
	case "purge":
		b.handlePurge(chatID)
		return
	case "confirm":
		b.handleConfirmCommand(chatID, msg.CommandArguments())
		return
	case "background":
		b.handleExecCommand(chatID, msg, true)
		return
	case "foreground", "fg":
		b.handleExecCommand(chatID, msg, false)
		return
	}

	// Media message routing
	if msg.Document != nil {
		b.handleDocument(chatID, msg)
		return
	}
	if len(msg.Photo) > 0 {
		b.handlePhoto(chatID, msg)
		return
	}
	if msg.Voice != nil {
		b.handleVoice(chatID, msg)
		return
	}

	// Text messages (existing behavior)
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

func (b *Bot) registerCommands() {
	commands := tgbotapi.NewSetMyCommands(
		tgbotapi.BotCommand{Command: "start", Description: "重置会话 / Reset session"},
		tgbotapi.BotCommand{Command: "stop", Description: "停止会话 / Stop session"},
		tgbotapi.BotCommand{Command: "status", Description: "后台任务状态 / Background task status"},
		tgbotapi.BotCommand{Command: "purge", Description: "清理系统内存 / Purge system memory"},
		tgbotapi.BotCommand{Command: "background", Description: "后台运行 / Run in background"},
		tgbotapi.BotCommand{Command: "foreground", Description: "前台运行 / Run in foreground"},
		tgbotapi.BotCommand{Command: "confirm", Description: "确认计划 / Confirm plan"},
	)
	if _, err := b.api.Request(commands); err != nil {
		log.Printf("Warning: failed to register bot commands: %v", err)
	} else {
		log.Printf("Bot commands registered successfully")
	}
}

func (b *Bot) handlePurge(chatID int64) {
	b.send(chatID, "🧹 正在清理内存...")
	go func() {
		if b.gateway != nil {
			chatIDStr := fmt.Sprintf("%d", chatID)
			response, err := b.gateway.Send(chatIDStr,
				"执行 sudo -n purge，然后用 top -l 1 -s 0 | grep PhysMem 报告内存状态，简短回复", "", "")
			if err != nil {
				b.send(chatID, fmt.Sprintf("Error: %v", err))
				return
			}
			b.sendLong(chatID, response)
		} else {
			b.send(chatID, "Purge only available in gateway mode.")
		}
	}()
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

// isBackgroundMessage checks if a message should run in background.
// Must contain BOTH an action word AND a project keyword
func isBackgroundMessage(text string) bool {
	lower := strings.ToLower(text)

	// Must contain BOTH an action word AND a project keyword
	actionWords := []string{
		"执行", "开始", "启动", "运行", "部署",
		"go ", "start ", "run ", "execute", "deploy", "build",
		"按照", "用harness", "launch",
	}
	projectKeywords := []string{
		"harness-loop", "harness loop",
		"后台模式", "后台运行", "后台执行",
	}

	hasAction := false
	hasProject := false

	for _, a := range actionWords {
		if strings.Contains(lower, a) {
			hasAction = true
			break
		}
	}
	for _, p := range projectKeywords {
		if strings.Contains(lower, p) {
			hasProject = true
			break
		}
	}

	return hasAction && hasProject
}

// isHarnessLoopTask checks if message is a harness-loop task
func isHarnessLoopTask(text string) bool {
	lower := strings.ToLower(text)
	harnessKeywords := []string{
		"harness-loop", "harness loop",
		"后台模式", "后台运行", "后台执行",
	}
	for _, keyword := range harnessKeywords {
		if strings.Contains(lower, keyword) {
			return true
		}
	}
	return false
}

// generatePlanID generates a short unique plan ID
func generatePlanID() string {
	return strings.ToUpper(fmt.Sprintf("%06x", time.Now().UnixNano()&0xffffff))
}

// fmtDuration formats a duration for display
func fmtDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d秒", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%d分钟", int(d.Minutes()))
	}
	return fmt.Sprintf("%d小时", int(d.Hours()))
}

// promptForPlanSelection displays pending plans for user selection
func (b *Bot) promptForPlanSelection(chatID int64, plans []PendingPlan) {
	builder := strings.Builder{}
	builder.WriteString("🤔 检测到多个待确认的计划，请选择要执行哪一个：\n\n")

	for i, plan := range plans {
		elapsed := time.Since(plan.CreatedAt)
		timeStr := fmtDuration(elapsed)

		builder.WriteString(fmt.Sprintf(
			"   %d️⃣ #%s - %s\n      创建时间：%s\n\n",
			i+1, plan.ID, plan.Plan[:50], timeStr,
		))
	}

	builder.WriteString("📌 快速选择：\n")
	for i := range plans {
		builder.WriteString(fmt.Sprintf(
			"   • 输入 %d 选择 #%s\n", i+1, plans[i].ID,
		))
	}

	builder.WriteString("\n   • 输入 /confirm #ID 选择指定 plan\n")
	builder.WriteString("   • 输入 /confirm latest 选择最新的\n")

	b.send(chatID, builder.String())

	// Set waiting state
	b.mu.Lock()
	defer b.mu.Unlock()
	b.selectionWaiters[chatID] = WaitingForSelection{
		ChatID:     chatID,
		Plans:       plans,
		ExpiryTime: time.Now().Add(5 * time.Minute), // 5 minutes
	}
	log.Printf("[bot] Set waiting for plan selection for chat=%d (plans=%d)", chatID, len(plans))
}

// getPendingPlans retrieves all pending plans for a chat
func (b *Bot) getPendingPlans(chatID int64) []PendingPlan {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pendingPlans[chatID]
}

// addPendingPlan adds a new pending plan
func (b *Bot) addPendingPlan(chatID int64, plan PendingPlan) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pendingPlans[chatID] = append(b.pendingPlans[chatID], plan)
	log.Printf("[bot] Added pending plan #%s for chat=%d", plan.ID, chatID)
}

// clearPendingPlans clears all pending plans for a chat
func (b *Bot) clearPendingPlans(chatID int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.pendingPlans, chatID)
	log.Printf("[bot] Cleared pending plans for chat=%d", chatID)
}

// handleConfirmCommand processes /confirm command
func (b *Bot) handleConfirmCommand(chatID int64, args string) {
	args = strings.TrimSpace(args)

	// Check if user is selecting by number
	if waiter, ok := b.selectionWaiters[chatID]; ok {
		if !time.Now().After(waiter.ExpiryTime) {
			// Expired, remove it
			delete(b.selectionWaiters, chatID)
		} else {
			// Try to parse as number
			num, err := strconv.Atoi(args)
			if err == nil && num >= 1 && num <= len(waiter.Plans) {
				// User selected by number
				plan := waiter.Plans[num-1]
				log.Printf("[bot] User selected plan #%s by number %d", plan.ID, num)
				delete(b.selectionWaiters, chatID)
				b.executeBackgroundTask(chatID, plan.ID, plan.Plan)
				return
			}
		}
	}

	// Get pending plans
	plans := b.getPendingPlans(chatID)

	// No plans
	if len(plans) == 0 {
		b.send(chatID, "❓ 没有待确认的计划。请先提交任务（例如：按照 harness-loop 执行升级）")
		return
	}

	// Case 1: /confirm latest or no args -> confirm latest
	if args == "" || strings.ToLower(args) == "latest" {
		plan := plans[len(plans)-1]
		log.Printf("[bot] Confirming latest plan #%s", plan.ID)
		b.executeBackgroundTask(chatID, plan.ID, plan.Plan)
		return
	}

	// Case 2: /confirm #ID -> confirm specific plan
	planID := strings.TrimPrefix(args, "#")
	planID = strings.ToUpper(strings.TrimSpace(planID))

	for _, plan := range plans {
		if plan.ID == planID {
			log.Printf("[bot] Confirming plan #%s", planID)
			b.executeBackgroundTask(chatID, plan.ID, plan.Plan)
			return
		}
	}

	// Plan not found
	b.send(chatID, fmt.Sprintf("❓ 未找到计划 #%s", planID))
}

// executeBackgroundTask executes a plan in background
func (b *Bot) executeBackgroundTask(chatID int64, planID string, planText string) {
	chatIDStr := fmt.Sprintf("%d", chatID)

	typing := tgbotapi.NewChatAction(chatID, tgbotapi.ChatTyping)
	b.api.Send(typing)

	// Create request with plan ID included
	body, err := json.Marshal(gatewayBackgroundRequest{
		ChatID:   chatIDStr,
		Message:  planText,
		BotToken: b.api.Token,
		BotID:    b.gateway.botID,
		PlanID:   planID,
	})
	if err != nil {
		b.send(chatID, fmt.Sprintf("Error marshaling request: %v", err))
		return
	}

	resp, err := doWithRetry(func() (*http.Response, error) {
		return b.gateway.httpClient.Post(
			b.gateway.baseURL+"/api/gateway/send-background",
			"application/json",
			bytes.NewBuffer(body),
		)
	})
	if err != nil {
		b.send(chatID, fmt.Sprintf("Error starting background task: %v", err))
		return
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		b.send(chatID, fmt.Sprintf("Error reading response: %v", err))
		return
	}

	var result gatewayBackgroundResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		b.send(chatID, fmt.Sprintf("Error parsing response: %v", err))
		return
	}

	if result.Status == "rejected" {
		b.send(chatID, fmt.Sprintf("Background task rejected: %s", result.Reason))
		return
	}

	// Remove confirmed plan from pending
	b.mu.Lock()
	defer b.mu.Unlock()
	plans := b.pendingPlans[chatID]
	for i, p := range plans {
		if p.ID == planID {
			b.pendingPlans[chatID] = append(plans[:i], plans[i+1:]...)
			log.Printf("[bot] Removed confirmed plan #%s from pending", planID)
			break
		}
	}

	b.send(chatID, fmt.Sprintf("✅ 已选择 Plan #%s，正在后台执行...\n任务完成后会通知你。", planID))
}

// handleExecCommand handles /background or /foreground with optional combined args
// e.g. /background parallel long, /foreground parallel short
func (b *Bot) handleExecCommand(chatID int64, msg *tgbotapi.Message, isBackground bool) {
	args := strings.Fields(strings.ToLower(msg.CommandArguments()))
	if len(args) == 0 {
		// No args — fall through to original interactive flow
		b.handleBackgroundChoice(chatID, isBackground)
		return
	}

	// Parse combined flags
	useParallel := false
	isLong := false
	hasParallel := false
	hasDuration := false
	for _, arg := range args {
		switch arg {
		case "parallel", "para":
			useParallel = true
			hasParallel = true
		case "noparallel", "nopara":
			useParallel = false
			hasParallel = true
		case "long":
			isLong = true
			hasDuration = true
		case "short":
			isLong = false
			hasDuration = true
		default:
			b.send(chatID, fmt.Sprintf("⚠️ 未知参数: %s\n\n可用参数: parallel, noparallel, long, short", arg))
			return
		}
	}

	plans := b.getPendingPlans(chatID)
	if len(plans) == 0 {
		b.send(chatID, "❓ 没有待确认的计划。请先提交任务。")
		return
	}

	plan := plans[len(plans)-1]

	// Build status message
	mode := "后台"
	if !isBackground {
		mode = "前台"
	}
	parts := []string{fmt.Sprintf("✅ %s运行", mode)}
	if hasParallel {
		if useParallel {
			parts = append(parts, "并行 (Centurion)")
		} else {
			parts = append(parts, "不并行")
		}
	}
	if hasDuration {
		if isLong {
			parts = append(parts, "长任务")
		} else {
			parts = append(parts, "短任务")
		}
	}
	b.send(chatID, strings.Join(parts, " | "))

	// Always execute as background task
	b.executeBackgroundTask(chatID, plan.ID, plan.Plan)
}

// handleBackgroundChoice handles /background or /foreground command (no args)
func (b *Bot) handleBackgroundChoice(chatID int64, isBackground bool) {
	plans := b.getPendingPlans(chatID)
	if len(plans) == 0 {
		b.send(chatID, "❓ 没有待确认的计划。请先提交任务。")
		return
	}

	plan := plans[len(plans)-1]
	b.executeBackgroundTask(chatID, plan.ID, plan.Plan)
}


// cleanupExpiredSelections removes expired selection waiters
func (b *Bot) cleanupExpiredSelections() {
	now := time.Now()
	b.mu.Lock()
	defer b.mu.Unlock()

	for chatID, waiter := range b.selectionWaiters {
		if now.After(waiter.ExpiryTime) {
			delete(b.selectionWaiters, chatID)
			log.Printf("[bot] Cleaned expired selection waiter for chat=%d", chatID)
		}
	}
}

// handleTextGateway forwards messages to the mini-claude-bot gateway.
// No single-chat restriction — each chat_id gets its own isolated session.
func (b *Bot) handleTextGateway(chatID int64, text string, msg *tgbotapi.Message) {
	// Check if waiting for plan selection (single check)
	if waiter, ok := b.selectionWaiters[chatID]; ok {
		if !time.Now().After(waiter.ExpiryTime) {
			num, err := strconv.Atoi(text)
			if err == nil && num >= 1 && num <= len(waiter.Plans) {
				plan := waiter.Plans[num-1]
				log.Printf("[bot] User selected plan #%s by number %d", plan.ID, num)
				delete(b.selectionWaiters, chatID)
				b.executeBackgroundTask(chatID, plan.ID, plan.Plan)
				return
			}
		}
	}

	// If this is a reply to another message, prepend the quoted context
	if msg.ReplyToMessage != nil && msg.ReplyToMessage.Text != "" {
		text = fmt.Sprintf("[Replying to: \"%s\"]\n\n%s", msg.ReplyToMessage.Text, text)
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

		if response == "" {
			b.send(chatID, "(empty response)")
			return
		}

		// Auto-detect harness execution ready marker
		const execReadyMarker = "[HARNESS_EXEC_READY]"
		if strings.Contains(response, execReadyMarker) {
			// Strip marker from response
			cleanResponse := strings.ReplaceAll(response, execReadyMarker, "")
			cleanResponse = strings.TrimSpace(cleanResponse)
			if cleanResponse != "" {
				b.sendLong(chatID, cleanResponse)
			}

			// Auto-start background execution
			b.send(chatID, "🚀 Plan confirmed. Starting background execution...")
			planID := generatePlanID()
			execPrompt := "Resume the harness-loop. The plan has been confirmed. Enter the Execute Loop now. Execute all tasks following the harness-loop skill instructions."
			b.addPendingPlan(chatID, PendingPlan{
				ID:        planID,
				Plan:      execPrompt,
				CreatedAt: time.Now(),
			})
			b.executeBackgroundTask(chatID, planID, execPrompt)
			return
		}

		b.sendLong(chatID, response)
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

func extractPrompt(msg *tgbotapi.Message, defaultPrompt string) string {
	if msg.Caption != "" {
		return msg.Caption
	}
	return defaultPrompt
}

func (b *Bot) handleDocument(chatID int64, msg *tgbotapi.Message) {
	doc := msg.Document
	log.Printf("[recv] document: name=%s mime=%s size=%d", doc.FileName, doc.MimeType, doc.FileSize)

	// File size guard (10MB)
	if doc.FileSize > 10*1024*1024 {
		b.send(chatID, "File too large (max 10 MB).")
		return
	}

	typing := tgbotapi.NewChatAction(chatID, tgbotapi.ChatTyping)
	b.api.Send(typing)

	go func() {
		stopTyping := b.startTypingLoop(chatID)
		defer close(stopTyping)

		// Download file
		localPath, err := downloadTelegramFile(b.api, doc.FileID, doc.FileName)
		if err != nil {
			b.send(chatID, fmt.Sprintf("Failed to download file: %v", err))
			return
		}

		// Try to extract text
		var fileContent string
		ext := strings.ToLower(filepath.Ext(doc.FileName))

		if ext == ".pdf" {
			fileContent, err = extractPDFText(localPath)
			if err != nil {
				fileContent = fmt.Sprintf("[Could not extract PDF text: %v]", err)
			}
		} else {
			fileContent, err = extractTextFromFile(localPath)
			if err != nil {
				fileContent = fmt.Sprintf("[Error reading file: %v]", err)
			}
		}

		userPrompt := extractPrompt(msg, "Please analyze this document.")

		var prompt string
		if fileContent != "" {
			prompt = fmt.Sprintf("[File: %s]\n%s\n\n%s", doc.FileName, fileContent, userPrompt)
		} else {
			// Binary/unsupported file — reference the path
			prompt = fmt.Sprintf("[File saved at: %s (type: %s, %d bytes)]\n\n%s", localPath, doc.MimeType, doc.FileSize, userPrompt)
		}

		// Send to Claude
		if b.gateway != nil {
			chatIDStr := fmt.Sprintf("%d", chatID)
			userID := fmt.Sprintf("%d", msg.From.ID)
			response, err := b.gateway.Send(chatIDStr, prompt, userID, msg.From.UserName)
			if err != nil {
				b.send(chatID, fmt.Sprintf("Error: %v", err))
				return
			}
			b.sendLong(chatID, response)
		} else {
			response, err := b.session.Send(context.Background(), prompt)
			if err != nil {
				b.send(chatID, fmt.Sprintf("Error: %v", err))
				return
			}
			b.sendLong(chatID, response)
		}
	}()
}

func (b *Bot) handlePhoto(chatID int64, msg *tgbotapi.Message) {
	// Pick highest resolution photo (last in array)
	photos := msg.Photo
	photo := photos[len(photos)-1]
	log.Printf("[recv] photo: %dx%d size=%d", photo.Width, photo.Height, photo.FileSize)

	typing := tgbotapi.NewChatAction(chatID, tgbotapi.ChatTyping)
	b.api.Send(typing)

	go func() {
		stopTyping := b.startTypingLoop(chatID)
		defer close(stopTyping)

		// Download photo
		localPath, err := downloadTelegramFile(b.api, photo.FileID, "photo.jpg")
		if err != nil {
			b.send(chatID, fmt.Sprintf("Failed to download photo: %v", err))
			return
		}

		userPrompt := extractPrompt(msg, "Please describe and analyze this image.")

		if b.gateway != nil {
			// Gateway mode: reference file path in message (same machine)
			prompt := fmt.Sprintf("[Image saved at: %s]\n\nPlease read this image file and respond to: %s", localPath, userPrompt)
			chatIDStr := fmt.Sprintf("%d", chatID)
			userID := fmt.Sprintf("%d", msg.From.ID)
			response, err := b.gateway.Send(chatIDStr, prompt, userID, msg.From.UserName)
			if err != nil {
				b.send(chatID, fmt.Sprintf("Error: %v", err))
				return
			}
			b.sendLong(chatID, response)
		} else {
			// Local mode: use -f flag to pass file to claude CLI
			response, err := b.session.Send(context.Background(), userPrompt, localPath)
			if err != nil {
				b.send(chatID, fmt.Sprintf("Error: %v", err))
				return
			}
			b.sendLong(chatID, response)
		}
	}()
}

func (b *Bot) handleVoice(chatID int64, msg *tgbotapi.Message) {
	voice := msg.Voice
	log.Printf("[recv] voice: duration=%ds mime=%s size=%d", voice.Duration, voice.MimeType, voice.FileSize)

	// Duration guard (5 minutes)
	if voice.Duration > 300 {
		b.send(chatID, "Voice message too long (max 5 minutes).")
		return
	}

	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		b.send(chatID, "Voice transcription requires OPENAI_API_KEY to be configured.")
		return
	}

	typing := tgbotapi.NewChatAction(chatID, tgbotapi.ChatTyping)
	b.api.Send(typing)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[voice] PANIC in voice handler: %v", r)
				b.send(chatID, fmt.Sprintf("Internal error processing voice: %v", r))
			}
		}()

		log.Printf("[voice] starting processing for chat=%d", chatID)
		stopTyping := b.startTypingLoop(chatID)
		defer close(stopTyping)

		// Download voice file
		log.Printf("[voice] downloading file ID=%s", voice.FileID)
		localPath, err := downloadTelegramFile(b.api, voice.FileID, "voice.ogg")
		if err != nil {
			log.Printf("[voice] download failed: %v", err)
			b.send(chatID, fmt.Sprintf("Failed to download voice: %v", err))
			return
		}
		log.Printf("[voice] downloaded to %s", localPath)

		// Transcribe
		log.Printf("[voice] transcribing with Whisper...")
		transcription, err := transcribeVoice(localPath, apiKey)
		if err != nil {
			log.Printf("[voice] transcription failed: %v", err)
			b.send(chatID, fmt.Sprintf("Transcription failed: %v", err))
			return
		}
		log.Printf("[voice] transcription result: %s", transcription)

		// Show user the transcription
		b.send(chatID, fmt.Sprintf("🎙 语音转录: %s", transcription))

		// Build prompt with caption if any
		userPrompt := extractPrompt(msg, "")
		var prompt string
		if userPrompt != "" {
			prompt = fmt.Sprintf("[Voice message transcription]: %s\n\n%s", transcription, userPrompt)
		} else {
			prompt = transcription
		}

		// Send to Claude
		if b.gateway != nil {
			chatIDStr := fmt.Sprintf("%d", chatID)
			userID := fmt.Sprintf("%d", msg.From.ID)
			response, err := b.gateway.Send(chatIDStr, prompt, userID, msg.From.UserName)
			if err != nil {
				b.send(chatID, fmt.Sprintf("Error: %v", err))
				return
			}
			b.sendLong(chatID, response)
		} else {
			response, err := b.session.Send(context.Background(), prompt)
			if err != nil {
				b.send(chatID, fmt.Sprintf("Error: %v", err))
				return
			}
			b.sendLong(chatID, response)
		}
	}()
}

func (b *Bot) send(chatID int64, text string) {
	html := markdownToTelegramHTML(text)
	msg := tgbotapi.NewMessage(chatID, html)
	msg.ParseMode = tgbotapi.ModeHTML
	msg.DisableWebPagePreview = true
	if _, err := b.api.Send(msg); err != nil {
		// Fallback to plain text if HTML parse fails
		log.Printf("HTML send failed, falling back to plain text: %v", err)
		plain := tgbotapi.NewMessage(chatID, text)
		if _, err2 := b.api.Send(plain); err2 != nil {
			log.Printf("Failed to send message: %v", err2)
		}
	}
}

// markdownToTelegramHTML converts Claude's markdown output to Telegram-compatible HTML.
func markdownToTelegramHTML(text string) string {
	// Escape HTML entities first
	text = strings.ReplaceAll(text, "&", "&amp;")
	text = strings.ReplaceAll(text, "<", "&lt;")
	text = strings.ReplaceAll(text, ">", "&gt;")

	// Code blocks: ```lang\n...\n``` → <pre><code>...</code></pre>
	for {
		start := strings.Index(text, "```")
		if start == -1 {
			break
		}
		// Find the end of opening ```
		afterOpen := start + 3
		endOfLine := strings.Index(text[afterOpen:], "\n")
		if endOfLine == -1 {
			break
		}
		codeStart := afterOpen + endOfLine + 1

		// Find closing ```
		end := strings.Index(text[codeStart:], "```")
		if end == -1 {
			break
		}
		codeContent := text[codeStart : codeStart+end]
		replacement := "<pre><code>" + codeContent + "</code></pre>"
		text = text[:start] + replacement + text[codeStart+end+3:]
	}

	// Inline code: `text` → <code>text</code>
	for {
		start := strings.Index(text, "`")
		if start == -1 {
			break
		}
		end := strings.Index(text[start+1:], "`")
		if end == -1 {
			break
		}
		inner := text[start+1 : start+1+end]
		text = text[:start] + "<code>" + inner + "</code>" + text[start+1+end+1:]
	}

	// Bold: **text** → <b>text</b>
	for {
		start := strings.Index(text, "**")
		if start == -1 {
			break
		}
		end := strings.Index(text[start+2:], "**")
		if end == -1 {
			break
		}
		inner := text[start+2 : start+2+end]
		text = text[:start] + "<b>" + inner + "</b>" + text[start+2+end+2:]
	}

	// Italic: *text* → <i>text</i> (but not inside words like file*name)
	for {
		start := strings.Index(text, "*")
		if start == -1 {
			break
		}
		end := strings.Index(text[start+1:], "*")
		if end == -1 {
			break
		}
		inner := text[start+1 : start+1+end]
		if len(inner) == 0 || strings.Contains(inner, "\n") {
			break
		}
		text = text[:start] + "<i>" + inner + "</i>" + text[start+1+end+1:]
	}

	// Strikethrough: ~~text~~ → <s>text</s>
	for {
		start := strings.Index(text, "~~")
		if start == -1 {
			break
		}
		end := strings.Index(text[start+2:], "~~")
		if end == -1 {
			break
		}
		inner := text[start+2 : start+2+end]
		text = text[:start] + "<s>" + inner + "</s>" + text[start+2+end+2:]
	}

	return text
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
