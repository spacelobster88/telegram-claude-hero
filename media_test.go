package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIsTextFile(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"file.txt", true},
		{"file.csv", true},
		{"file.go", true},
		{"file.json", true},
		{"file.md", true},
		{"file.py", true},
		{"file.pdf", false},
		{"file.jpg", false},
		{"file.exe", false},
		{"file.png", false},
		{"file", false},
		{"FILE.TXT", true},  // case insensitive
		{"file.Go", true},   // case insensitive
		{"file.JS", true},
		{"file.yaml", true},
		{"file.yml", true},
		{"file.sh", true},
		{"file.rs", true},
		{"file.cpp", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTextFile(tt.name); got != tt.want {
				t.Errorf("isTextFile(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestExtractTextFromFile(t *testing.T) {
	dir := t.TempDir()

	// Test with a valid text file
	txtFile := filepath.Join(dir, "test.txt")
	content := "Hello, world!\nLine 2"
	if err := os.WriteFile(txtFile, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	text, err := extractTextFromFile(txtFile)
	if err != nil {
		t.Fatalf("extractTextFromFile error: %v", err)
	}
	if text != content {
		t.Errorf("got %q, want %q", text, content)
	}

	// Non-text file extension returns empty string
	binFile := filepath.Join(dir, "test.jpg")
	if err := os.WriteFile(binFile, []byte{0xFF, 0xD8, 0xFF}, 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	text, err = extractTextFromFile(binFile)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "" {
		t.Errorf("expected empty for non-text file, got %q", text)
	}

	// Empty text file
	emptyFile := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(emptyFile, []byte{}, 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	text, err = extractTextFromFile(emptyFile)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "" {
		t.Errorf("expected empty for empty file, got %q", text)
	}
}

func TestExtractTextFromFile_Truncation(t *testing.T) {
	dir := t.TempDir()
	bigFile := filepath.Join(dir, "big.txt")
	data := make([]byte, 150*1024) // 150KB exceeds the 100KB limit
	for i := range data {
		data[i] = 'A'
	}
	if err := os.WriteFile(bigFile, data, 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	text, err := extractTextFromFile(bigFile)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	// Result should be 100KB of data + truncation note
	if len(text) > 110*1024 {
		t.Errorf("text too long: %d bytes", len(text))
	}
	if !strings.Contains(text, "[truncated") {
		t.Error("missing truncation note")
	}
}

func TestExtractTextFromFile_InvalidUTF8(t *testing.T) {
	dir := t.TempDir()
	badFile := filepath.Join(dir, "bad.txt")
	// Invalid UTF-8 bytes
	if err := os.WriteFile(badFile, []byte{0xFF, 0xFE, 0x80, 0x81}, 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	_, err := extractTextFromFile(badFile)
	if err == nil {
		t.Error("expected error for invalid UTF-8 file, got nil")
	}
}

func TestCleanupOldFiles(t *testing.T) {
	// Ensure the function does not panic, even if tempDir doesn't exist or is empty
	cleanupOldFiles(1 * time.Hour)
}

func TestEnsureTempDir(t *testing.T) {
	err := ensureTempDir()
	if err != nil {
		t.Fatalf("ensureTempDir error: %v", err)
	}
	info, err := os.Stat(tempDir)
	if err != nil {
		t.Fatalf("temp dir not created: %v", err)
	}
	if !info.IsDir() {
		t.Error("tempDir is not a directory")
	}
}
