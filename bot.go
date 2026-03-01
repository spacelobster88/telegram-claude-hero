package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const maxMessageLen = 4096

type Bot struct {
	api          *tgbotapi.BotAPI
	session      *Session
	activeChatID int64
	mu           sync.Mutex
}

func NewBot(token string) (*Bot, error) {
	api, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return nil, fmt.Errorf("creating bot: %w", err)
	}
	log.Printf("Authorized as @%s", api.Self.UserName)
	return &Bot{api: api}, nil
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
		b.handleStop(chatID)
		return
	}

	if msg.Command() == "start" {
		b.mu.Lock()
		if b.session != nil {
			b.session.Stop()
		}
		b.session = NewSession()
		b.activeChatID = chatID
		b.mu.Unlock()
		b.send(chatID, "Session started. Send me your prompts!")
		return
	}

	text := msg.Text
	if text == "" {
		return
	}

	b.handleText(chatID, text)
}

func (b *Bot) handleStop(chatID int64) {
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

func (b *Bot) handleText(chatID int64, text string) {
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
