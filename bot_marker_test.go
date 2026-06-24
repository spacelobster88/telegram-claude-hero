package main

import "testing"

// Issue #12: the HARNESS_EXEC_READY marker detection must tolerate markdown and
// whitespace noise so a confirmed plan never silently fails to trigger execution,
// while never false-firing on unrelated text.
func TestContainsExecReadyMarker(t *testing.T) {
	positives := []string{
		"plan ready [HARNESS_EXEC_READY]",
		"**[HARNESS_EXEC_READY]**",
		"text\n[ HARNESS_EXEC_READY ]\n",
		"done [HARNESS_EXEC_READY] trailing",
		`escaped \[HARNESS\_EXEC\_READY\]`,
		"lower [harness_exec_ready]",
	}
	for _, p := range positives {
		if !containsExecReadyMarker(p) {
			t.Errorf("expected marker detected in %q", p)
		}
	}

	negatives := []string{
		"no marker here",
		"HARNESS EXEC READY without brackets",
		"[HARNESS_EXEC] incomplete",
		"discussing the harness exec readiness in prose",
		"",
	}
	for _, n := range negatives {
		if containsExecReadyMarker(n) {
			t.Errorf("did not expect marker in %q", n)
		}
	}
}

func TestStripExecReadyMarker(t *testing.T) {
	cases := map[string]string{
		"plan summary [HARNESS_EXEC_READY]": "plan summary",
		"**[HARNESS_EXEC_READY]**":          "****",
		`a \[HARNESS\_EXEC\_READY\] b`:       "a  b",
	}
	for in, wantContains := range cases {
		got := stripExecReadyMarker(in)
		if containsExecReadyMarker(got) {
			t.Errorf("marker survived strip for %q -> %q", in, got)
		}
		_ = wantContains
	}
}
