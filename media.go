package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ledongthuc/pdf"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const tempDir = "/tmp/tg-hero-files/"

// MediaResult holds the result of processing a media attachment.
type MediaResult struct {
	Text      string
	FilePath  string
	MediaType string
}

// ensureTempDir creates the temporary directory if it does not exist.
func ensureTempDir() error {
	return os.MkdirAll(tempDir, 0o755)
}

// cleanupOldFiles removes files older than maxAge from the temp directory.
func cleanupOldFiles(maxAge time.Duration) {
	entries, err := os.ReadDir(tempDir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-maxAge)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			os.Remove(filepath.Join(tempDir, entry.Name()))
		}
	}
}

// downloadTelegramFile downloads a file from Telegram servers and saves it locally.
func downloadTelegramFile(bot *tgbotapi.BotAPI, fileID string, suggestedName string) (string, error) {
	if err := ensureTempDir(); err != nil {
		return "", fmt.Errorf("failed to create temp dir: %w", err)
	}

	cleanupOldFiles(1 * time.Hour)

	file, err := bot.GetFile(tgbotapi.FileConfig{FileID: fileID})
	if err != nil {
		return "", fmt.Errorf("failed to get file info from Telegram: %w", err)
	}

	fileURL := file.Link(bot.Token)

	resp, err := http.Get(fileURL)
	if err != nil {
		return "", fmt.Errorf("failed to download file: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to download file: HTTP %d", resp.StatusCode)
	}

	filename := fmt.Sprintf("%d_%s", time.Now().UnixNano(), suggestedName)
	localPath := filepath.Join(tempDir, filename)

	out, err := os.Create(localPath)
	if err != nil {
		return "", fmt.Errorf("failed to create local file: %w", err)
	}
	defer out.Close()

	if _, err := io.Copy(out, resp.Body); err != nil {
		return "", fmt.Errorf("failed to write file: %w", err)
	}

	return localPath, nil
}

// isTextFile returns true if the file appears to be a plain text file.
func isTextFile(filename string) bool {
	textExtensions := map[string]bool{
		".txt": true, ".csv": true, ".json": true, ".md": true, ".xml": true,
		".yaml": true, ".yml": true, ".log": true, ".py": true, ".go": true,
		".js": true, ".ts": true, ".html": true, ".css": true, ".sql": true,
		".sh": true, ".toml": true, ".ini": true, ".cfg": true, ".conf": true,
		".env": true, ".tsx": true, ".jsx": true, ".rb": true, ".rs": true,
		".java": true, ".kt": true, ".c": true, ".h": true, ".cpp": true, ".hpp": true,
	}
	ext := strings.ToLower(filepath.Ext(filename))
	return textExtensions[ext]
}

// extractTextFromFile extracts readable text content from a file.
func extractTextFromFile(filePath string) (string, error) {
	if !isTextFile(filePath) {
		return "", nil
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to read text file: %w", err)
	}

	if !utf8.Valid(data) {
		return "", fmt.Errorf("file is not valid UTF-8")
	}

	const maxSize = 100 * 1024
	if len(data) > maxSize {
		data = data[:maxSize]
		return string(data) + "\n... [truncated, showing first 100KB]", nil
	}

	return string(data), nil
}

// extractPDFText extracts text content from a PDF file.
func extractPDFText(filePath string) (string, error) {
	f, r, err := pdf.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to open PDF: %w", err)
	}
	defer f.Close()

	var buf strings.Builder
	for i := 1; i <= r.NumPage(); i++ {
		p := r.Page(i)
		if p.V.IsNull() {
			continue
		}
		text, err := p.GetPlainText(nil)
		if err != nil {
			continue // skip unreadable pages
		}
		buf.WriteString(text)
		buf.WriteString("\n")
	}

	result := buf.String()
	if strings.TrimSpace(result) == "" {
		return "", fmt.Errorf("PDF contains no extractable text (may be scanned/image-based)")
	}
	return result, nil
}

// transcribeVoice transcribes audio from a voice message using OpenAI's Whisper API.
func transcribeVoice(filePath string, apiKey string) (string, error) {
	if apiKey == "" {
		return "", fmt.Errorf("OPENAI_API_KEY not configured — voice transcription unavailable")
	}

	// Open the audio file
	f, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to open audio file: %w", err)
	}
	defer f.Close()

	// Build multipart form
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	// Add the audio file
	part, err := writer.CreateFormFile("file", filepath.Base(filePath))
	if err != nil {
		return "", fmt.Errorf("failed to create form file: %w", err)
	}
	if _, err := io.Copy(part, f); err != nil {
		return "", fmt.Errorf("failed to write audio data: %w", err)
	}

	// Add model field
	writer.WriteField("model", "whisper-1")
	writer.Close()

	// Make request
	req, err := http.NewRequest("POST", "https://api.openai.com/v1/audio/transcriptions", &buf)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("Whisper API request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read Whisper response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Whisper API error (HTTP %d): %s", resp.StatusCode, string(body))
	}

	var result struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("failed to parse Whisper response: %w", err)
	}

	return result.Text, nil
}
