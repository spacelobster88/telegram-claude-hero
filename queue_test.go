package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetQueueStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/gateway/queue-status/123" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("bot_id") != "test-bot" {
			t.Errorf("missing bot_id: %s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"busy": true, "busy_message": "deploy staging", "elapsed_seconds": 65,
			"slots_used": 1, "slots_max": 16,
			"queue_wait_timeout": 900, "queue_wait_remaining": 120,
		})
	}))
	defer srv.Close()

	gw := NewGatewayClient(srv.URL, "test-bot")
	st, err := gw.GetQueueStatus("123")
	if err != nil {
		t.Fatalf("GetQueueStatus: %v", err)
	}
	if !st.Busy || st.BusyMessage != "deploy staging" || st.ElapsedSeconds != 65 {
		t.Errorf("busy fields parsed wrong: %+v", st)
	}
	if st.SlotsUsed != 1 || st.SlotsMax != 16 || st.QueueWaitTimeout != 900 || st.QueueWaitRemaining != 120 {
		t.Errorf("slot/timeout fields parsed wrong: %+v", st)
	}
}
