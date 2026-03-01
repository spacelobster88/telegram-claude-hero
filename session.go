package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os/exec"
	"sync"
	"syscall"
)

type Session struct {
	mu        sync.Mutex
	busy      bool
	cancel    context.CancelFunc
	cmd       *exec.Cmd
	firstDone bool
}

func NewSession() *Session {
	return &Session{}
}

func (s *Session) Send(ctx context.Context, prompt string) (string, error) {
	s.mu.Lock()
	if s.busy {
		s.mu.Unlock()
		return "", fmt.Errorf("still processing the previous message, please wait")
	}
	s.busy = true
	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	continued := s.firstDone
	s.mu.Unlock()

	defer func() {
		cancel()
		s.mu.Lock()
		s.cmd = nil
		s.busy = false
		s.cancel = nil
		s.mu.Unlock()
	}()

	args := []string{"-p", "--output-format", "text", "--dangerously-skip-permissions"}
	if continued {
		args = append(args, "--continue")
	}
	args = append(args, prompt)

	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	s.mu.Lock()
	s.cmd = cmd
	s.mu.Unlock()

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	log.Printf("[claude] running (continue=%v) prompt=%q", continued, prompt)

	if err := cmd.Run(); err != nil {
		// Don't report errors if we were intentionally cancelled
		if ctx.Err() != nil {
			return "", fmt.Errorf("session stopped")
		}
		errMsg := stderr.String()
		if errMsg != "" {
			return "", fmt.Errorf("claude: %s", errMsg)
		}
		return "", fmt.Errorf("claude: %w", err)
	}

	s.mu.Lock()
	s.firstDone = true
	s.mu.Unlock()

	response := stdout.String()
	log.Printf("[claude] response length=%d", len(response))
	return response, nil
}

func (s *Session) IsBusy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.busy
}

func (s *Session) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cancel != nil {
		s.cancel()
	}
	if s.cmd != nil && s.cmd.Process != nil {
		pgid, err := syscall.Getpgid(s.cmd.Process.Pid)
		if err == nil {
			log.Printf("[claude] killing process group %d", pgid)
			syscall.Kill(-pgid, syscall.SIGKILL)
		} else {
			s.cmd.Process.Kill()
		}
	}
}
