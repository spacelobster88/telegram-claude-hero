package main

import (
	"context"
	"fmt"
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

type Bot struct {
	api          *tgbotapi.BotAPI
	session      *Session       // only used in local mode
	activeChatID int64          // only used in local mode
	mu           sync.Mutex
	gateway      *GatewayClient // nil in local mode
	pendingExec  map[int64]string // chat_id -> exec prompt, awaiting user confirmation
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
		api:         api,
		pendingExec: make(map[int64]string),
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
	case "purge":
		b.handlePurge(chatID)
		return
	case "start":
		b.handleStart(chatID, msg)
		return
	case "stop":
		b.handleStop(chatID, msg)
		return
	case "cleanup":
		b.handleCleanup(chatID)
		return
	case "status":
		b.handleHarnessStatus(chatID)
		return
	case "confirm":
		b.handleConfirm(chatID)
		return
	case "resume":
		b.handleResume(chatID)
		return
	case "away":
		b.handleAway(chatID)
		return
	case "back":
		b.handleBack(chatID)
		return
	case "metastatus":
		b.handleMetaStatus(chatID)
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
		tgbotapi.BotCommand{Command: "purge", Description: "清理系统内存 / Purge system memory"},
		tgbotapi.BotCommand{Command: "start", Description: "重置会话 / Reset session"},
		tgbotapi.BotCommand{Command: "stop", Description: "停止会话 / Stop session"},
		tgbotapi.BotCommand{Command: "cleanup", Description: "清理过期任务 / Clean up stale background jobs"},
		tgbotapi.BotCommand{Command: "status", Description: "后台任务状态 / Background task status"},
		tgbotapi.BotCommand{Command: "confirm", Description: "确认计划开始执行 / Confirm plan and start"},
		tgbotapi.BotCommand{Command: "resume", Description: "恢复后台任务 / Resume harness loop in background"},
		tgbotapi.BotCommand{Command: "away", Description: "离开模式 / Away - Nirmana takes over"},
		tgbotapi.BotCommand{Command: "back", Description: "回来了 / Back - Eddie returns"},
		tgbotapi.BotCommand{Command: "metastatus", Description: "元循环状态 / AROS Meta Loop status"},
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

// startTypingLoop sends a "typing" indicator every 4 seconds until the
// returned stop channel is closed.
func (b *Bot) startTypingLoop(chatID int64) chan struct{} {
	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()
		timeout := time.NewTimer(30 * time.Minute)
		defer timeout.Stop()
		for {
			select {
			case <-stop:
				return
			case <-timeout.C:
				return
			case <-ticker.C:
				typing := tgbotapi.NewChatAction(chatID, tgbotapi.ChatTyping)
				b.api.Send(typing)
			}
		}
	}()
	return stop
}

// buildExecWithContext prepends recent channel messages as context to the exec prompt.
func (b *Bot) buildExecWithContext(chatID int64, execPrompt string) string {
	chatIDStr := fmt.Sprintf("%d", chatID)
	msgs, err := b.gateway.GetRecentMessages(chatIDStr, 3)
	if err != nil {
		log.Printf("[bot] Failed to fetch recent messages for context (chat=%d): %v", chatID, err)
		return execPrompt
	}
	if len(msgs) == 0 {
		return execPrompt
	}

	var ctx strings.Builder
	ctx.WriteString("[Recent channel context]\n")
	for _, m := range msgs {
		ctx.WriteString(fmt.Sprintf("%s: %s\n", m.Role, m.Content))
	}
	ctx.WriteString("[End of context]\n\n")
	ctx.WriteString(execPrompt)
	return ctx.String()
}

// handleConfirm starts background execution if a pending exec is waiting.
func (b *Bot) handleConfirm(chatID int64) {
	b.mu.Lock()
	execPrompt, ok := b.pendingExec[chatID]
	if ok {
		delete(b.pendingExec, chatID)
	}
	b.mu.Unlock()

	if !ok {
		b.send(chatID, "No pending plan to confirm.")
		return
	}

	b.send(chatID, "🚀 Confirmed. Starting background execution...")
	b.startBackgroundExecution(chatID, b.buildExecWithContext(chatID, execPrompt))
}

// handleResume directly starts background execution for harness-loop resume.
// Unlike regular "Resume" text messages that go through the foreground (where
// HARNESS_BATCH_DONE markers are never parsed), /resume goes straight to
// send_background() where the chain dispatch logic lives. See issue #7.
func (b *Bot) handleResume(chatID int64) {
	if b.gateway == nil {
		b.send(chatID, "Gateway not configured.")
		return
	}

	chatIDStr := fmt.Sprintf("%d", chatID)

	// Check if a bg task is already running
	bgStatus, err := b.gateway.GetBackgroundStatus(chatIDStr)
	if err == nil && bgStatus.Status == "running" {
		b.send(chatID, "Chain is already running. Use /status to check progress.")
		return
	}

	resumePrompt := "Resume the harness-loop. Continue the Execute Loop — pick up the next batch of ready tasks."
	b.send(chatID, "🔄 Resuming harness loop in background...")
	b.startBackgroundExecution(chatID, b.buildExecWithContext(chatID, resumePrompt))
}

// startBackgroundExecution sends a message to the gateway's background endpoint.
func (b *Bot) startBackgroundExecution(chatID int64, message string) {
	chatIDStr := fmt.Sprintf("%d", chatID)

	resp, err := b.gateway.SendBackground(chatIDStr, message, b.api.Token)
	if err != nil {
		b.send(chatID, fmt.Sprintf("Error starting background task: %v", err))
		return
	}

	if resp.Status == "rejected" {
		if resp.Reason == "already running" {
			b.send(chatID, "Chain is already running. Use /status to check progress.")
		} else {
			b.send(chatID, fmt.Sprintf("Background task rejected: %s", resp.Reason))
		}
		return
	}

	log.Printf("[bot] Background execution started for chat=%d", chatID)
	b.send(chatID, "✅ Harness loop started in background. Use /status to monitor progress.")
}

// sendStreamingToTelegram uses SendStream to display Claude's response with
// edit-in-place updates in Telegram.
func (b *Bot) sendStreamingToTelegram(chatID int64, chatIDStr, text, userID, username string) {
	stopTyping := b.startTypingLoop(chatID)

	var msgID int // Telegram message ID for editing
	var accumulated strings.Builder
	var lastEditTime time.Time
	const editThrottle = 3 * time.Second
	thinkingShown := false

	response, err := b.gateway.SendStream(chatIDStr, text, userID, username, func(eventType, content string) {
		switch eventType {
		case "thinking":
			if !thinkingShown {
				// Send initial "Thinking..." message and capture message_id
				msg := tgbotapi.NewMessage(chatID, "💭 Thinking...")
				sent, sendErr := b.api.Send(msg)
				if sendErr == nil {
					msgID = sent.MessageID
					thinkingShown = true
				}
			}
		case "text":
			accumulated.WriteString(content)
			now := time.Now()
			// Throttle edits to 1 per 3 seconds
			if msgID != 0 && now.Sub(lastEditTime) >= editThrottle {
				currentText := accumulated.String()
				if len(currentText) > 4000 {
					currentText = currentText[:4000] + "..."
				}
				edit := tgbotapi.NewEditMessageText(chatID, msgID, currentText)
				b.api.Send(edit)
				lastEditTime = now
			} else if msgID == 0 {
				// No thinking phase — send first message with text
				msg := tgbotapi.NewMessage(chatID, content)
				sent, sendErr := b.api.Send(msg)
				if sendErr == nil {
					msgID = sent.MessageID
					lastEditTime = now
				}
			}
		}
	})

	close(stopTyping)

	if err != nil {
		log.Printf("[stream] SSE failed for chat=%d: %v — falling back to non-streaming", chatID, err)
		// Fallback: retry via non-streaming Send() so the user gets a response
		fallbackResp, fallbackErr := b.gateway.Send(chatIDStr, text, userID, username)
		if fallbackErr != nil {
			b.send(chatID, fmt.Sprintf("Error: %v", fallbackErr))
			return
		}
		response = fallbackResp
		err = nil
	}

	if response == "" {
		if msgID != 0 {
			edit := tgbotapi.NewEditMessageText(chatID, msgID, "(empty response)")
			b.api.Send(edit)
		} else {
			b.send(chatID, "(empty response)")
		}
		return
	}

	// Check for HARNESS_EXEC_READY marker
	const execReadyMarker = "[HARNESS_EXEC_READY]"
	if strings.Contains(response, execReadyMarker) {
		cleanResponse := strings.ReplaceAll(response, execReadyMarker, "")
		cleanResponse = strings.TrimSpace(cleanResponse)
		if msgID != 0 {
			if len(cleanResponse) <= 4096 {
				edit := tgbotapi.NewEditMessageText(chatID, msgID, markdownToTelegramHTML(cleanResponse))
				edit.ParseMode = tgbotapi.ModeHTML
				b.api.Send(edit)
			} else {
				b.sendLong(chatID, cleanResponse)
			}
		} else if cleanResponse != "" {
			b.sendLong(chatID, cleanResponse)
		}

		// Gate: store exec prompt and wait for user confirmation
		execPrompt := "Resume the harness-loop. The plan has been confirmed. Enter the Execute Loop now. Execute all tasks following the harness-loop skill instructions."
		b.mu.Lock()
		b.pendingExec[chatID] = execPrompt
		b.mu.Unlock()

		// If Nirmana mode is active (/away), auto-confirm as Eddie-Nirmana
		if b.gateway != nil {
			chatIDStr := strconv.FormatInt(chatID, 10)
			state, err := b.gateway.GetNirmanaState(chatIDStr)
			if err == nil && state != nil && state.NirmanaMode {
				log.Printf("[nirmana] Auto-confirming harness-loop for chat %d (Eddie is /away)", chatID)
				b.send(chatID, "🤖 Nirmana auto-confirming plan (Eddie is /away)...")
				go func() {
					// Small delay to let the pending exec settle
					time.Sleep(2 * time.Second)
					b.handleConfirm(chatID)
				}()
				return
			}
		}

		b.send(chatID, "📋 Plan ready. Reply /confirm to start background execution, or send feedback to revise.")
		return
	}

	// Final edit with complete response
	if msgID != 0 {
		if len(response) <= 4096 {
			html := markdownToTelegramHTML(response)
			edit := tgbotapi.NewEditMessageText(chatID, msgID, html)
			edit.ParseMode = tgbotapi.ModeHTML
			edit.DisableWebPagePreview = true
			if _, editErr := b.api.Send(edit); editErr != nil {
				// Fallback to plain text edit
				edit2 := tgbotapi.NewEditMessageText(chatID, msgID, response)
				b.api.Send(edit2)
			}
		} else {
			// Response too long for single edit — delete streaming msg and send fresh
			del := tgbotapi.NewDeleteMessage(chatID, msgID)
			b.api.Send(del)
			b.sendLong(chatID, response)
		}
	} else {
		b.sendLong(chatID, response)
	}
}

// handleTextGateway forwards messages to the mini-claude-bot gateway.
// No single-chat restriction — each chat_id gets its own isolated session.
func (b *Bot) handleTextGateway(chatID int64, text string, msg *tgbotapi.Message) {
	// Check if there's a pending exec awaiting confirmation
	lower := strings.ToLower(strings.TrimSpace(text))
	b.mu.Lock()
	execPrompt, hasPending := b.pendingExec[chatID]
	b.mu.Unlock()
	if hasPending {
		// Check for confirmation keywords
		confirmWords := []string{"yes", "y", "ok", "confirm", "go", "go ahead", "approved", "start",
			"确认", "可以", "开始", "好", "好的", "执行", "没问题"}
		isConfirm := false
		for _, w := range confirmWords {
			if lower == w {
				isConfirm = true
				break
			}
		}
		if isConfirm {
			b.mu.Lock()
			delete(b.pendingExec, chatID)
			b.mu.Unlock()
			b.send(chatID, "🚀 Confirmed. Starting background execution...")
			b.startBackgroundExecution(chatID, b.buildExecWithContext(chatID, execPrompt))
			return
		}
		// Not a confirm — user is revising, clear pending and forward to Claude
		b.mu.Lock()
		delete(b.pendingExec, chatID)
		b.mu.Unlock()
		log.Printf("[bot] Pending exec cleared for chat=%d, forwarding revision to Claude", chatID)
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
		b.sendStreamingToTelegram(chatID, chatIDStr, text, userID, username)
	}()
}

func (b *Bot) handleHarnessStatus(chatID int64) {
	chatIDStr := fmt.Sprintf("%d", chatID)

	jobs, err := b.gateway.GetAllHarnessStatus(chatIDStr)
	if err != nil {
		// Fallback to basic background status
		bgStatus, bgErr := b.gateway.GetBackgroundStatus(chatIDStr)
		if bgErr != nil {
			b.send(chatID, fmt.Sprintf("Error getting status: %v", err))
			return
		}
		switch bgStatus.Status {
		case "idle":
			b.send(chatID, "No background task running.")
		case "running":
			elapsed := int(bgStatus.ElapsedSeconds)
			b.send(chatID, fmt.Sprintf("Background task running (%dm%ds elapsed)", elapsed/60, elapsed%60))
		default:
			b.send(chatID, fmt.Sprintf("Background status: %s", bgStatus.Status))
		}
		return
	}

	if len(jobs) == 0 {
		b.send(chatID, "No background tasks running.")
		return
	}

	var sb strings.Builder
	for i, job := range jobs {
		if i > 0 {
			sb.WriteString("\n\n")
		}
		// Multi-job header (only if 2+ jobs)
		if len(jobs) > 1 {
			sb.WriteString(fmt.Sprintf("[%d/%d] ", i+1, len(jobs)))
		}
		b.formatJobStatus(&sb, &job)
	}

	b.send(chatID, sb.String())
}

// formatJobStatus writes a single job's status into the string builder.
func (b *Bot) formatJobStatus(sb *strings.Builder, job *gatewayHarnessStatusResponse) {
	h := job.Harness

	// Header with background status
	switch job.BgStatus {
	case "idle":
		if h == nil {
			sb.WriteString("Idle (no harness data)")
			return
		}
	case "running":
		elapsed := int(job.ElapsedSeconds)
		sb.WriteString(fmt.Sprintf("Running (%dm%ds elapsed", elapsed/60, elapsed%60))
		if job.ChainDepth > 0 {
			sb.WriteString(fmt.Sprintf(", chain #%d", job.ChainDepth))
		}
		sb.WriteString(")\n")
	case "completed":
		sb.WriteString("Last batch completed.\n")
	case "failed":
		sb.WriteString("Last batch failed.\n")
	}

	if h == nil {
		return
	}

	sb.WriteString(fmt.Sprintf("Project: %s\n", h.ProjectName))
	sb.WriteString(fmt.Sprintf("Phase: %s\n", h.CurrentPhase))
	sb.WriteString(fmt.Sprintf("Progress: %d/%d done", h.Done, h.Total))
	if h.InProgress > 0 {
		sb.WriteString(fmt.Sprintf(", %d in progress", h.InProgress))
	}
	if h.Blocked > 0 {
		sb.WriteString(fmt.Sprintf(", %d blocked", h.Blocked))
	}
	sb.WriteString("\n")

	phaseOrder := []string{"architecture", "uiux", "engineering", "qa"}
	for _, phase := range phaseOrder {
		ps, ok := h.Phases[phase]
		if !ok {
			continue
		}
		barLen := 10
		filled := 0
		if ps.Total > 0 {
			filled = ps.Done * barLen / ps.Total
		}
		bar := strings.Repeat("█", filled) + strings.Repeat("░", barLen-filled)
		sb.WriteString(fmt.Sprintf("\n  %s [%s] %d/%d", phase, bar, ps.Done, ps.Total))
		if ps.InProgress > 0 {
			sb.WriteString(fmt.Sprintf(" (%d running)", ps.InProgress))
		}
		if ps.Blocked > 0 {
			sb.WriteString(fmt.Sprintf(" (%d blocked)", ps.Blocked))
		}
	}

	if job.ChainDepth > 0 {
		sb.WriteString(fmt.Sprintf("\n\nChain count: %d", job.ChainDepth))
	}
}

func (b *Bot) handleMetaStatus(chatID int64) {
	if b.gateway == nil {
		b.send(chatID, "Meta-loop status only available in gateway mode.")
		return
	}

	status, err := b.gateway.GetMetaLoopStatus()
	if err != nil {
		b.send(chatID, "⚠️ Meta-loop service unavailable: "+err.Error())
		return
	}

	var sb strings.Builder
	sb.WriteString("🧠 **AROS Meta Loop Status**\n\n")

	// Mode and state
	modeEmoji := "⚖️"
	switch status.CadenceMode {
	case "aggressive":
		modeEmoji = "🔥"
	case "conservative":
		modeEmoji = "🐢"
	case "frozen":
		modeEmoji = "❄️"
	}

	runningText := "idle"
	if status.Running {
		runningText = "🔄 running"
	}

	sb.WriteString(fmt.Sprintf("%s Mode: `%s` | State: %s\n", modeEmoji, status.CadenceMode, runningText))

	// Last cycle
	if status.LastCycle != nil {
		sb.WriteString(fmt.Sprintf("\n📊 Last Cycle #%d: `%s`\n", status.LastCycle.CycleNum, status.LastCycle.Status))
		sb.WriteString(fmt.Sprintf("   Trigger: %s | Steps: %d/6\n", status.LastCycle.Trigger, status.LastCycle.StepsCompleted))
		if status.LastCycle.IdentityVerdict != "" {
			sb.WriteString(fmt.Sprintf("   Identity: %s\n", status.LastCycle.IdentityVerdict))
		}
	}

	// Meta-goal scores with progress bars
	if status.MetaGoalScores != nil {
		sb.WriteString("\n📈 **Meta-Goals:**\n")
		goals := []struct{ key, label string }{
			{"G1_truthful", "G1 Truthful"},
			{"G2_efficient", "G2 Efficient"},
			{"G3_reliable", "G3 Reliable"},
			{"G4_aligned", "G4 Aligned"},
			{"G5_ambitious", "G5 Ambitious"},
			{"G6_self_know", "G6 Self-Know"},
		}
		for _, g := range goals {
			if val, ok := status.MetaGoalScores[g.key]; ok {
				score := 0.0
				switch v := val.(type) {
				case float64:
					score = v
				case int:
					score = float64(v)
				}
				bar := metaProgressBar(score, 10)
				sb.WriteString(fmt.Sprintf("   %s %s `%.0f%%`\n", g.label, bar, score*100))
			}
		}
		if agg, ok := status.MetaGoalScores["aggregate"]; ok {
			if v, ok := agg.(float64); ok {
				sb.WriteString(fmt.Sprintf("\n   **Aggregate: %.0f%%**\n", v*100))
			}
		}
	}

	// Pending approvals
	if status.PendingApprovals > 0 {
		sb.WriteString(fmt.Sprintf("\n⏳ %d pending approval(s)\n", status.PendingApprovals))
	}

	b.send(chatID, sb.String())
}

func metaProgressBar(value float64, width int) string {
	filled := int(value * float64(width))
	if filled > width {
		filled = width
	}
	if filled < 0 {
		filled = 0
	}
	return strings.Repeat("#", filled) + strings.Repeat("-", width-filled)
}

func (b *Bot) handleCleanup(chatID int64) {
	if b.gateway == nil {
		b.send(chatID, "Cleanup only available in gateway mode.")
		return
	}

	chatIDStr := fmt.Sprintf("%d", chatID)
	b.send(chatID, "Cleaning up stale background jobs...")

	result, err := b.gateway.CleanupBackgroundTasks(chatIDStr)
	if err != nil {
		b.send(chatID, fmt.Sprintf("Error during cleanup: %v", err))
		return
	}

	if result.Cleaned == 0 && result.Skipped == 0 {
		b.send(chatID, "No background jobs found.")
		return
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Cleanup complete: %d cleaned, %d archived, %d skipped (still running)", result.Cleaned, result.Archived, result.Skipped))
	if len(result.Details) > 0 {
		sb.WriteString("\n")
		for _, d := range result.Details {
			sb.WriteString(fmt.Sprintf("\n• %s", d))
		}
	}
	b.send(chatID, sb.String())
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

func (b *Bot) handleAway(chatID int64) {
	if b.gateway == nil {
		b.send(chatID, "Gateway not configured.")
		return
	}

	chatIDStr := fmt.Sprintf("%d", chatID)
	_, err := b.gateway.SetNirmanaMode(chatIDStr, "away")
	if err != nil {
		b.send(chatID, fmt.Sprintf("Error activating away mode: %v", err))
		return
	}

	// Notify meta-loop of Nirmana activation
	go func() {
		resp, err := http.Post("http://localhost:8200/api/meta-loop/nirmana?activate=true", "application/json", nil)
		if err == nil {
			resp.Body.Close()
		}
	}()

	b.send(chatID, "接管了。I have the conn.")
}

func (b *Bot) handleBack(chatID int64) {
	if b.gateway == nil {
		b.send(chatID, "Gateway not configured.")
		return
	}

	chatIDStr := fmt.Sprintf("%d", chatID)
	result, err := b.gateway.SetNirmanaMode(chatIDStr, "back")
	if err != nil {
		b.send(chatID, fmt.Sprintf("Error deactivating away mode: %v", err))
		return
	}

	// Notify meta-loop of Nirmana deactivation
	go func() {
		resp, err := http.Post("http://localhost:8200/api/meta-loop/nirmana?activate=false", "application/json", nil)
		if err == nil {
			resp.Body.Close()
		}
	}()

	if result.Briefing != "" {
		b.sendLong(chatID, result.Briefing)
	} else {
		b.send(chatID, "Welcome back!")
	}
}
