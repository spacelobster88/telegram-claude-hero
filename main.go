package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	cfg, err := LoadConfig()
	if err != nil {
		log.Fatalf("Error loading config: %v", err)
	}

	if cfg.TelegramBotToken == "" {
		fmt.Println("No Telegram bot token found.")
		token, err := promptInput("Enter your Telegram Bot Token: ")
		if err != nil {
			log.Fatalf("Error reading token: %v", err)
		}
		if token == "" {
			log.Fatal("Token cannot be empty")
		}
		cfg.TelegramBotToken = token
		if err := SaveConfig(cfg); err != nil {
			log.Fatalf("Error saving config: %v", err)
		}
		fmt.Println("Token saved.")
	}

	bot, err := NewBot(cfg.TelegramBotToken)
	if err != nil {
		log.Fatalf("Error creating bot: %v", err)
	}

	// Graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("\nShutting down...")
		bot.Stop()
		os.Exit(0)
	}()

	fmt.Println("Bot started. Send a message in Telegram to begin.")
	bot.Run()
}
