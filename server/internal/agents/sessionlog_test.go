package agents

import "testing"

func TestCanonicalReason(t *testing.T) {
	for in, want := range map[string]string{
		"viewer:viewer_closed": "viewer_closed",
		"viewer_left":          "viewer_closed",
		"agent:customer_ended": "agent_closed",
		"rejected:busy":        "agent_closed",
		"device_deleted":       "agent_closed",
		"quick_closed":         "agent_closed",
		"agent_disconnected":   "timeout",
		"viewer_stalled":       "timeout",
		"timeout":              "timeout",
		"server_shutdown":      "server_shutdown",
	} {
		if got := CanonicalReason(in); got != want {
			t.Errorf("CanonicalReason(%q) = %q, want %q", in, got, want)
		}
	}
}
