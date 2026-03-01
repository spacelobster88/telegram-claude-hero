package main

import (
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

	switch {
	case msg.Command() == "start":
		b.handleStart(chatID)
	case msg.Command() == "stop":
		b.handleStop(chatID)
	default:
		b.handleText(chatID, msg.Text)
	}
}

func (b *Bot) handleStart(chatID int64) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.session != nil {
		b.send(chatID, "A session is already active. Use /stop to end it first.")
		return
	}

	b.activeChatID = chatID
	b.session = NewSession()

	err := b.session.Start(func(output string) {
		output = cleanPTYOutput(output)
		if output == "" {
			return
		}
		b.sendLong(chatID, output)
	})
	if err != nil {
		b.send(chatID, fmt.Sprintf("Failed to start Claude session: %v", err))
		b.session = nil
		return
	}

	b.send(chatID, "Claude session started. Send me your prompts!")

	// Monitor session end
	go func() {
		b.session.Wait()
		b.mu.Lock()
		b.session = nil
		b.mu.Unlock()
		b.send(chatID, "Claude session ended.")
	}()
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
	session := b.session
	activeChatID := b.activeChatID
	b.mu.Unlock()

	if session == nil {
		b.send(chatID, "No active session. Use /start to begin.")
		return
	}

	if chatID != activeChatID {
		b.send(chatID, "Another chat has an active session.")
		return
	}

	if err := session.Write(text); err != nil {
		b.send(chatID, fmt.Sprintf("Error sending input: %v", err))
	}
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

		// Try to split at a newline
		cutoff := maxMessageLen
		if idx := strings.LastIndex(text[:cutoff], "\n"); idx > 0 {
			cutoff = idx + 1
		}
		chunks = append(chunks, text[:cutoff])
		text = text[cutoff:]
	}
	return chunks
}

// cleanPTYOutput strips common ANSI escape sequences from PTY output.
func cleanPTYOutput(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		if s[i] == '\x1b' {
			// Skip CSI sequences: ESC [ ... final_byte
			if i+1 < len(s) && s[i+1] == '[' {
				j := i + 2
				for j < len(s) && s[j] >= 0x20 && s[j] <= 0x3F {
					j++
				}
				if j < len(s) && s[j] >= 0x40 && s[j] <= 0x7E {
					j++
				}
				i = j
				continue
			}
			// Skip OSC sequences: ESC ] ... BEL/ST
			if i+1 < len(s) && s[i+1] == ']' {
				j := i + 2
				for j < len(s) {
					if s[j] == '\x07' {
						j++
						break
					}
					if s[j] == '\x1b' && j+1 < len(s) && s[j+1] == '\\' {
						j += 2
						break
					}
					j++
				}
				i = j
				continue
			}
			// Skip other two-byte escape sequences
			i += 2
			continue
		}
		// Skip carriage returns
		if s[i] == '\r' {
			i++
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return strings.TrimSpace(b.String())
}
