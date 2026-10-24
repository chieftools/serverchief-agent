// Package systemdnotify implements the sd_notify(3) datagram protocol so
// the agent can run as a Type=notify unit. Without NOTIFY_SOCKET (SysV
// hosts, manual runs) every call is a no-op.
package systemdnotify

import (
	"net"
	"os"
	"strings"
)

// NotifyReady tells systemd that startup finished.
func NotifyReady(status string) error {
	return notify(buildState("READY=1", status))
}

// NotifyReloading tells systemd that a SIGHUP reload started. Call
// NotifyReady once it completes.
func NotifyReloading(status string) error {
	return notify(buildState("RELOADING=1", status))
}

// NotifyStopping tells systemd that shutdown started.
func NotifyStopping(status string) error {
	return notify(buildState("STOPPING=1", status))
}

func buildState(prefix string, status string) string {
	parts := make([]string, 0, 2)
	if prefix != "" {
		parts = append(parts, prefix)
	}
	if status != "" {
		parts = append(parts, "STATUS="+status)
	}
	return strings.Join(parts, "\n")
}

func notify(state string) error {
	if state == "" {
		return nil
	}

	socket := os.Getenv("NOTIFY_SOCKET")
	if socket == "" {
		return nil
	}

	// The standard library maps a leading "@" to the Linux abstract
	// namespace, matching systemd's convention.
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: socket, Net: "unixgram"})
	if err != nil {
		return err
	}
	defer conn.Close()

	_, err = conn.Write([]byte(state))
	return err
}
