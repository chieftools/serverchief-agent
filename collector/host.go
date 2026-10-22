package collector

import (
	"bufio"
	"bytes"
	"context"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Uname mirrors the fields of uname(2) that collectors use.
type Uname struct {
	Sysname string
	Release string
	Version string
	Machine string
}

// FSStats holds the stable part of statfs(2) for one mount point.
type FSStats struct {
	SizeBytes   uint64
	TotalInodes uint64
}

// NetInterface is the subset of net.Interface plus addresses that the
// network collector needs.
type NetInterface struct {
	Name      string
	Index     int
	MTU       int
	MAC       string
	Up        bool
	Loopback  bool
	Addresses []netip.Prefix
}

// Host is the view of the machine that collectors read from. The zero value
// of each function field disables that source, which tests rely on.
type Host struct {
	// FS is rooted at the host's "/". Paths passed to it have no leading slash.
	FS fs.FS
	// Root is the on-disk directory FS refers to. It prefixes statfs calls.
	Root string

	Uname       func() (Uname, error)
	Statfs      func(path string) (FSStats, error)
	Interfaces  func() ([]NetInterface, error)
	LookupCNAME func(ctx context.Context, host string) (string, error)
}

// NewHost returns a Host for the running machine, optionally rooted at a
// different directory (for example a mounted image or a fixture tree).
func NewHost(root string) *Host {
	if root == "" {
		root = "/"
	}
	host := &Host{
		FS:          os.DirFS(root),
		Root:        root,
		Interfaces:  systemInterfaces,
		LookupCNAME: net.DefaultResolver.LookupCNAME,
	}
	host.Uname = systemUname
	host.Statfs = func(mountPoint string) (FSStats, error) {
		return systemStatfs(filepath.Join(root, mountPoint))
	}
	if root != "/" {
		// uname(2) and the interface list describe the running kernel, not
		// the tree under root, so only use them for the live system.
		host.Uname = nil
		host.Interfaces = nil
		host.LookupCNAME = nil
	}
	return host
}

func relative(name string) string {
	name = strings.TrimPrefix(path.Clean("/"+name), "/")
	if name == "" {
		return "."
	}
	return name
}

func (host *Host) readFile(name string) ([]byte, error) {
	return fs.ReadFile(host.FS, relative(name))
}

// readString reads a small attribute file and trims surrounding whitespace.
func (host *Host) readString(name string) (string, error) {
	data, err := host.readFile(name)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// readOptional is readString that maps every error to "".
func (host *Host) readOptional(name string) string {
	value, err := host.readString(name)
	if err != nil {
		return ""
	}
	return value
}

func (host *Host) readDir(name string) ([]fs.DirEntry, error) {
	return fs.ReadDir(host.FS, relative(name))
}

func (host *Host) exists(name string) bool {
	_, err := fs.Stat(host.FS, relative(name))
	return err == nil
}

func (host *Host) isDir(name string) bool {
	info, err := fs.Stat(host.FS, relative(name))
	return err == nil && info.IsDir()
}

func (host *Host) readLink(name string) (string, error) {
	return fs.ReadLink(host.FS, relative(name))
}

// lines splits data into lines without trailing carriage returns.
func lines(data []byte) []string {
	var result []string
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		result = append(result, strings.TrimRight(scanner.Text(), "\r"))
	}
	return result
}

// parseKeyValue parses shell-style KEY=value files such as os-release.
func parseKeyValue(data []byte) map[string]string {
	values := make(map[string]string)
	for _, line := range lines(data) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		values[key] = unquote(strings.TrimSpace(value))
	}
	return values
}

func unquote(value string) string {
	if len(value) >= 2 {
		first, last := value[0], value[len(value)-1]
		if (first == '"' || first == '\'') && first == last {
			value = value[1 : len(value)-1]
			if first == '"' {
				replacer := strings.NewReplacer(`\"`, `"`, `\\`, `\`, `\$`, `$`, "\\`", "`")
				value = replacer.Replace(value)
			}
		}
	}
	return value
}

func systemInterfaces() ([]NetInterface, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	result := make([]NetInterface, 0, len(interfaces))
	for _, iface := range interfaces {
		entry := NetInterface{
			Name:     iface.Name,
			Index:    iface.Index,
			MTU:      iface.MTU,
			MAC:      iface.HardwareAddr.String(),
			Up:       iface.Flags&net.FlagUp != 0,
			Loopback: iface.Flags&net.FlagLoopback != 0,
		}
		addresses, err := iface.Addrs()
		if err == nil {
			for _, address := range addresses {
				network, ok := address.(*net.IPNet)
				if !ok {
					continue
				}
				ip, ok := netip.AddrFromSlice(network.IP)
				if !ok {
					continue
				}
				ones, bits := network.Mask.Size()
				ip = ip.Unmap()
				if ip.Is4() && bits == 128 {
					ones -= 96
				}
				entry.Addresses = append(entry.Addresses, netip.PrefixFrom(ip, ones))
			}
		}
		result = append(result, entry)
	}
	return result, nil
}
