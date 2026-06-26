package main

import (
	"strings"
	"testing"
)

// Issue #7: before a task DAG exists (harness == nil), a running job's /status
// must say it's initializing, not just show a bare "Running (Xm elapsed)" timer.
func TestFormatJobStatusPreDAGShowsInitializing(t *testing.T) {
	var b Bot
	var sb strings.Builder
	job := gatewayHarnessStatusResponse{BgStatus: "running", ElapsedSeconds: 599, Harness: nil}
	b.formatJobStatus(&sb, &job)
	out := sb.String()

	if !strings.Contains(out, "Running (9m59s") {
		t.Errorf("expected the elapsed timer, got: %q", out)
	}
	if !strings.Contains(out, "Initializing") {
		t.Errorf("pre-DAG status must explain it's initializing, got: %q", out)
	}
}

// Once a DAG exists, the rich view is unchanged and the initializing line is absent.
func TestFormatJobStatusWithDAGUnchanged(t *testing.T) {
	var b Bot
	var sb strings.Builder
	h := &harnessInfo{
		ProjectName:  "proj",
		CurrentPhase: "engineering",
		Total:        5,
		Done:         2,
		Phases:       map[string]harnessPhaseStatus{"engineering": {Total: 5, Done: 2}},
	}
	job := gatewayHarnessStatusResponse{BgStatus: "running", ElapsedSeconds: 30, Harness: h}
	b.formatJobStatus(&sb, &job)
	out := sb.String()

	if strings.Contains(out, "Initializing") {
		t.Errorf("with-DAG view must NOT show the initializing line, got: %q", out)
	}
	if !strings.Contains(out, "Project: proj") || !strings.Contains(out, "Progress: 2/5") {
		t.Errorf("with-DAG view missing rich content, got: %q", out)
	}
}

// A completed/idle batch with no harness data should not get the initializing line.
func TestFormatJobStatusCompletedNoInitLine(t *testing.T) {
	var b Bot
	var sb strings.Builder
	job := gatewayHarnessStatusResponse{BgStatus: "completed", Harness: nil}
	b.formatJobStatus(&sb, &job)
	if strings.Contains(sb.String(), "Initializing") {
		t.Errorf("completed status should not show the initializing line, got: %q", sb.String())
	}
}
