//go:build linux

package systemdnotify

import (
	"net"
	"path/filepath"
	"testing"
	"time"
)

func receive(t *testing.T, send func() error) string {
	t.Helper()
	socketPath := filepath.Join(t.TempDir(), "notify.sock")

	listener, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: socketPath, Net: "unixgram"})
	if err != nil {
		t.Fatalf("listen unixgram: %v", err)
	}
	defer listener.Close()

	t.Setenv("NOTIFY_SOCKET", socketPath)
	if err := listener.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	if err := send(); err != nil {
		t.Fatalf("notify: %v", err)
	}

	buf := make([]byte, 256)
	n, _, err := listener.ReadFromUnix(buf)
	if err != nil {
		t.Fatalf("read notify payload: %v", err)
	}
	return string(buf[:n])
}

func TestNotifyReadyWritesDatagram(t *testing.T) {
	if payload := receive(t, func() error { return NotifyReady("ready") }); payload != "READY=1\nSTATUS=ready" {
		t.Fatalf("unexpected payload: %q", payload)
	}
}

func TestNotifyReloadingWritesDatagram(t *testing.T) {
	if payload := receive(t, func() error { return NotifyReloading("") }); payload != "RELOADING=1" {
		t.Fatalf("unexpected payload: %q", payload)
	}
}
