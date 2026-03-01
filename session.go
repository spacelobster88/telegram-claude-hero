package main

import (
	"io"
	"log"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/creack/pty"
)

type Session struct {
	cmd  *exec.Cmd
	ptmx *os.File
	mu   sync.Mutex
	done chan struct{}
}

func NewSession() *Session {
	return &Session{
		done: make(chan struct{}),
	}
}

func (s *Session) Start(onOutput func(string)) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.cmd = exec.Command("claude", "--dangerously-skip-permissions")
	s.cmd.Env = append(os.Environ(), "TERM=dumb")

	ptmx, err := pty.Start(s.cmd)
	if err != nil {
		return err
	}
	s.ptmx = ptmx

	// Set a reasonable terminal size
	_ = pty.Setsize(ptmx, &pty.Winsize{Rows: 50, Cols: 120})

	go s.readLoop(onOutput)
	return nil
}

func (s *Session) readLoop(onOutput func(string)) {
	defer close(s.done)

	buf := make([]byte, 4096)
	var accumulated string
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	outputCh := make(chan string, 64)

	// Reader goroutine
	go func() {
		for {
			n, err := s.ptmx.Read(buf)
			if n > 0 {
				outputCh <- string(buf[:n])
			}
			if err != nil {
				if err != io.EOF {
					log.Printf("PTY read error: %v", err)
				}
				close(outputCh)
				return
			}
		}
	}()

	// Debounce and flush
	for {
		select {
		case chunk, ok := <-outputCh:
			if !ok {
				// PTY closed, flush remaining
				if accumulated != "" {
					onOutput(accumulated)
				}
				return
			}
			accumulated += chunk
		case <-ticker.C:
			if accumulated != "" {
				onOutput(accumulated)
				accumulated = ""
			}
		}
	}
}

func (s *Session) Write(input string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.ptmx == nil {
		return nil
	}
	_, err := s.ptmx.Write([]byte(input + "\n"))
	return err
}

func (s *Session) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.ptmx != nil {
		s.ptmx.Close()
	}
	if s.cmd != nil && s.cmd.Process != nil {
		s.cmd.Process.Kill()
		s.cmd.Wait()
	}
}

func (s *Session) Wait() {
	<-s.done
}
