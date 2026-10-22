package systemdnotify

import "testing"

func TestBuildState(t *testing.T) {
	if got := buildState("STOPPING=1", "bye"); got != "STOPPING=1\nSTATUS=bye" {
		t.Fatalf("buildState() = %q", got)
	}
	if got := buildState("", ""); got != "" {
		t.Fatalf("buildState(empty) = %q", got)
	}
}

func TestNotifyWithoutSocketIsNoop(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	if err := NotifyReady("ready"); err != nil {
		t.Fatalf("NotifyReady() = %v", err)
	}
}
