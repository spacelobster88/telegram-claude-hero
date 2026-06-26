package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Pins the /away/stop wire contract used by /back: roundup with mixed
// done/failed items (done → pr_url; failed → error), and the request shape.
func TestAwayStop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/gateway/away/stop" || r.Method != http.MethodPost {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["chat_id"] != "123" || body["bot_id"] != "test-bot" {
			t.Errorf("request body not forwarded: %v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"stopped_new_pickups": true,
			"roundup": []map[string]any{
				{"repo": "owner/repoA", "number": 7, "title": "Fix bug", "branch": "away/issue-7",
					"status": "done", "pr_url": "https://github.com/owner/repoA/pull/8"},
				{"repo": "owner/repoB", "number": 9, "title": "Risky", "branch": "away/issue-9",
					"status": "failed", "pr_url": nil, "error": "harness loop timeout"},
			},
		})
	}))
	defer srv.Close()

	gw := NewGatewayClient(srv.URL, "test-bot")
	resp, err := gw.AwayStop("123")
	if err != nil {
		t.Fatalf("AwayStop: %v", err)
	}
	if !resp.StoppedNewPickups || len(resp.Roundup) != 2 {
		t.Fatalf("unexpected resp: %+v", resp)
	}
	done := resp.Roundup[0]
	if done.Status != "done" || done.PRURL != "https://github.com/owner/repoA/pull/8" {
		t.Errorf("done item parsed wrong: %+v", done)
	}
	failed := resp.Roundup[1]
	if failed.Status != "failed" || failed.PRURL != "" || failed.Err != "harness loop timeout" {
		t.Errorf("failed item parsed wrong: %+v", failed)
	}
}
