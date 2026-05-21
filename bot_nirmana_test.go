package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Smoke-test for the /away → /back command plumbing.
//
// Bot.handleAway (bot.go:1320) and Bot.handleBack (bot.go:1344) delegate to
// GatewayClient.SetNirmanaMode. The auto-confirm path in handleTextGateway
// (bot.go:435-441) reads GatewayClient.GetNirmanaState and gates on
// NirmanaMode (gateway.go:448). These tests pin that wire contract.

// nirmanaTestServer is an in-memory fake of the mini-claude-bot gateway's
// nirmana endpoints. It mirrors the request/response shape the real server
// uses so the same GatewayClient code path runs end-to-end.
type nirmanaTestServer struct {
	mu             sync.Mutex
	state          map[string]bool // chat_id -> NirmanaMode
	setRequests    []nirmanaSetReq
	getRequests    []nirmanaGetReq
	briefingOnBack string
}

type nirmanaSetReq struct {
	ChatID string `json:"chat_id"`
	BotID  string `json:"bot_id"`
	Action string `json:"action"`
}

type nirmanaGetReq struct {
	ChatID string
	BotID  string
}

func newNirmanaTestServer() (*nirmanaTestServer, *httptest.Server) {
	s := &nirmanaTestServer{
		state:          make(map[string]bool),
		briefingOnBack: "welcome back, Eddie",
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/gateway/nirmana", s.handleToggle)
	mux.HandleFunc("/api/gateway/nirmana/", s.handleStateGet)
	return s, httptest.NewServer(mux)
}

func (s *nirmanaTestServer) handleToggle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req nirmanaSetReq
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.setRequests = append(s.setRequests, req)
	switch req.Action {
	case "away":
		s.state[req.ChatID] = true
	case "back":
		s.state[req.ChatID] = false
	default:
		s.mu.Unlock()
		http.Error(w, "invalid action: "+req.Action, http.StatusBadRequest)
		return
	}
	s.mu.Unlock()

	resp := NirmanaResponse{
		Status:  "ok",
		Message: "nirmana " + req.Action,
	}
	if req.Action == "back" {
		resp.Briefing = s.briefingOnBack
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *nirmanaTestServer) handleStateGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	chatID := strings.TrimPrefix(r.URL.Path, "/api/gateway/nirmana/")
	botID := r.URL.Query().Get("bot_id")
	s.mu.Lock()
	s.getRequests = append(s.getRequests, nirmanaGetReq{ChatID: chatID, BotID: botID})
	mode := s.state[chatID]
	s.mu.Unlock()

	resp := NirmanaStateResponse{NirmanaMode: mode}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// TestNirmanaRoundTrip exercises the full /away → state-check → /back → state-check
// sequence end-to-end through the real GatewayClient HTTP code path.
// This is the smoke test for the load-bearing Nirmana plumbing.
func TestNirmanaRoundTrip(t *testing.T) {
	fake, srv := newNirmanaTestServer()
	defer srv.Close()

	const (
		chatID = "12345"
		botID  = "test-bot"
	)
	gw := NewGatewayClient(srv.URL, botID)

	// 1. /away — Nirmana takes over.
	if _, err := gw.SetNirmanaMode(chatID, "away"); err != nil {
		t.Fatalf("SetNirmanaMode(away): %v", err)
	}
	state, err := gw.GetNirmanaState(chatID)
	if err != nil {
		t.Fatalf("GetNirmanaState after away: %v", err)
	}
	if !state.NirmanaMode {
		t.Fatalf("after /away: NirmanaMode = false, want true (this is the field bot.go:439 gates auto-confirm on)")
	}

	// 2. /back — Eddie returns.
	backResp, err := gw.SetNirmanaMode(chatID, "back")
	if err != nil {
		t.Fatalf("SetNirmanaMode(back): %v", err)
	}
	if backResp.Briefing == "" {
		t.Errorf("/back response missing briefing; bot.go:1365 expects this to render via sendLong")
	}
	state, err = gw.GetNirmanaState(chatID)
	if err != nil {
		t.Fatalf("GetNirmanaState after back: %v", err)
	}
	if state.NirmanaMode {
		t.Fatalf("after /back: NirmanaMode = true, want false")
	}

	// 3. Verify request wire format — chat_id, bot_id, and action must be on every POST.
	if got := len(fake.setRequests); got != 2 {
		t.Fatalf("set requests = %d, want 2", got)
	}
	for i, want := range []nirmanaSetReq{
		{ChatID: chatID, BotID: botID, Action: "away"},
		{ChatID: chatID, BotID: botID, Action: "back"},
	} {
		got := fake.setRequests[i]
		if got != want {
			t.Errorf("set request %d: got %+v, want %+v", i, got, want)
		}
	}

	// 4. Verify state-get requests carry bot_id query param (gateway.go:495).
	if got := len(fake.getRequests); got != 2 {
		t.Fatalf("get requests = %d, want 2", got)
	}
	for i, got := range fake.getRequests {
		if got.ChatID != chatID {
			t.Errorf("get request %d chat_id = %q, want %q", i, got.ChatID, chatID)
		}
		if got.BotID != botID {
			t.Errorf("get request %d bot_id = %q, want %q (multi-tenant isolation)", i, got.BotID, botID)
		}
	}
}

// TestNirmanaStateIsolatedByChatID verifies multi-tenant correctness:
// flipping NirmanaMode for one chat must not affect another. This pins the
// invariant that bot.go:439's per-chat auto-confirm decision doesn't bleed
// across sessions.
func TestNirmanaStateIsolatedByChatID(t *testing.T) {
	_, srv := newNirmanaTestServer()
	defer srv.Close()
	gw := NewGatewayClient(srv.URL, "test-bot")

	if _, err := gw.SetNirmanaMode("chat-A", "away"); err != nil {
		t.Fatalf("SetNirmanaMode(chat-A, away): %v", err)
	}

	stateA, err := gw.GetNirmanaState("chat-A")
	if err != nil {
		t.Fatalf("GetNirmanaState(chat-A): %v", err)
	}
	if !stateA.NirmanaMode {
		t.Errorf("chat-A NirmanaMode = false, want true")
	}

	stateB, err := gw.GetNirmanaState("chat-B")
	if err != nil {
		t.Fatalf("GetNirmanaState(chat-B): %v", err)
	}
	if stateB.NirmanaMode {
		t.Errorf("chat-B NirmanaMode = true, want false (cross-chat leak — bot.go:439 would auto-confirm on the wrong chat)")
	}
}

// TestNirmanaSetUnknownAction verifies the client surfaces non-200 errors
// rather than silently treating them as success. handleAway/handleBack
// (bot.go:1328, bot.go:1352) print these errors to the user.
func TestNirmanaSetUnknownAction(t *testing.T) {
	_, srv := newNirmanaTestServer()
	defer srv.Close()
	gw := NewGatewayClient(srv.URL, "test-bot")

	_, err := gw.SetNirmanaMode("chat-A", "sideways")
	if err == nil {
		t.Fatalf("SetNirmanaMode with invalid action returned nil error; expected HTTP 400 to surface")
	}
}
