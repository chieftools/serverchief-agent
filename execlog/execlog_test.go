package execlog

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestLogFormatsCommandsAndOutput(t *testing.T) {
	var log Log
	log.Command([]string{"/sbin/shutdown", "-r", "+1", "Server Chief: rebooting in 1 minute"})
	_, _ = log.Write([]byte("Shutdown scheduled"))
	log.Command([]string{"echo", "it's"})
	_, _ = log.Write([]byte("bad \xff byte\n"))
	text, truncated := log.Text()
	want := "$ /sbin/shutdown -r +1 'Server Chief: rebooting in 1 minute'\nShutdown scheduled\n$ echo 'it'\\''s'\nbad \uFFFD byte\n"
	if text != want || truncated {
		t.Fatalf("text = %q (truncated %v)", text, truncated)
	}
}

func TestCapKeepsTheEndAtACharacterBoundary(t *testing.T) {
	output := strings.Repeat("é", MaxBytes) // two bytes each
	text, truncated := Cap(output)
	if !truncated || len(text) > MaxBytes || !utf8.ValidString(text) || !strings.HasSuffix(output, text) {
		t.Fatalf("len %d, truncated %v, valid %v", len(text), truncated, utf8.ValidString(text))
	}
	if text, truncated := Cap("short"); text != "short" || truncated {
		t.Fatalf("short = %q %v", text, truncated)
	}
}

func TestLogBoundsMemoryAndMarksTruncation(t *testing.T) {
	var log Log
	chunk := []byte(strings.Repeat("x", 64<<10))
	for range 40 { // 2.5 MiB
		_, _ = log.Write(chunk)
	}
	_, _ = log.Write([]byte("END"))
	text, truncated := log.Text()
	if !truncated || len(text) != MaxBytes || !strings.HasSuffix(text, "END") {
		t.Fatalf("len %d, truncated %v", len(text), truncated)
	}
	if len(log.buffer) > 2*MaxBytes {
		t.Fatalf("buffer holds %d bytes", len(log.buffer))
	}
}

func TestRunRecordsArgvAndExitCode(t *testing.T) {
	var log Log
	code, err := Run(context.Background(), &log, []string{"/bin/sh", "-c", "echo out; echo err >&2; exit 3"})
	if err != nil || code != 3 {
		t.Fatalf("code %d, err %v", code, err)
	}
	text, _ := log.Text()
	if !strings.HasPrefix(text, "$ /bin/sh -c 'echo out; echo err >&2; exit 3'\n") || !strings.Contains(text, "out\n") || !strings.Contains(text, "err\n") {
		t.Fatalf("text = %q", text)
	}
	if _, err := Run(context.Background(), &log, []string{"/nonexistent/program"}); err == nil {
		t.Fatal("missing program did not fail")
	}
}
