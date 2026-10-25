package collector

import (
	"strings"
	"testing"
	"testing/fstest"
)

func TestReadFileKeepsHostingUsersInsideTheirTree(t *testing.T) {
	// The unprivileged daemon can read every file, so a configuration a
	// hosting user controls must not point it at anything outside their tree
	host := &Host{FS: fstest.MapFS{
		"etc/shadow":                   mapFile([]byte("root:$6$synthetic:19000::::::\n")),
		"etc/nginx/nginx.conf":         mapFile([]byte("include /home/alice/nginx/*.conf;\n")),
		"home/alice/nginx/site.conf":   mapFile([]byte("server { server_name alice.example.test; }\n")),
		"home/alice/nginx/shadow.conf": symlink("/etc/shadow"),
		"home/alice/nginx/inside.conf": symlink("site.conf"),
		"proc/meminfo":                 mapFile([]byte("MemTotal: 2048 kB\n")),
	}}

	for _, name := range []string{"/etc/nginx/nginx.conf", "/home/alice/nginx/site.conf", "/home/alice/nginx/inside.conf", "/proc/meminfo"} {
		if _, err := host.readFile(name); err != nil {
			t.Errorf("readFile(%s) error = %v", name, err)
		}
	}
	if data, err := host.readFile("/home/alice/nginx/shadow.conf"); err == nil || !strings.Contains(err.Error(), "out of /home/alice") || len(data) != 0 {
		t.Fatalf("readFile(shadow.conf) = %q, %v", data, err)
	}
}
