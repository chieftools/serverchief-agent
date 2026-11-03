package collector

import (
	"context"
	"errors"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.chief.tools/serverchief-agent/initsys"
)

// SystemInfo is the payload of the system dataset.
type SystemInfo struct {
	Hostname       string         `json:"hostname"`
	FQDN           string         `json:"fqdn"`
	OS             OSRelease      `json:"os"`
	Kernel         Kernel         `json:"kernel"`
	Arch           string         `json:"arch"`
	Virtualization Virtualization `json:"virtualization"`
	// BootTime is the kernel boot time in UTC (RFC 3339, second precision).
	BootTime string `json:"boot_time"`
	// BootID changes on every boot (/proc/sys/kernel/random/boot_id).
	BootID   string `json:"boot_id"`
	Timezone string `json:"timezone"`
	Init     string `json:"init"`
	// AgentArch is the architecture the agent binary was built for. It
	// differs from Arch under emulation (for example amd64 on arm64).
	AgentArch string `json:"agent_arch"`
	// PHPBuilds are PHP installations no package manager owns.
	PHPBuilds []PHPBuild `json:"php_builds"`
}

// Kernel identifies the running kernel.
type Kernel struct {
	Name    string `json:"name"`
	Release string `json:"release"`
	Version string `json:"version"`
	Machine string `json:"machine"`
}

// System collects the system dataset.
type System struct {
	host *Host

	mu       sync.Mutex
	bootID   string
	bootTime string
}

func NewSystem(host *Host) *System { return &System{host: host} }

func (*System) Name() string { return "system" }

func (collector *System) Collect(ctx context.Context) (any, error) {
	host := collector.host
	kernel := host.readKernel()
	hostname := host.readHostname()
	cpu := host.readCPUInfo()

	info := SystemInfo{
		Hostname:       hostname,
		FQDN:           host.resolveFQDN(ctx, hostname),
		OS:             host.readOSRelease(),
		Kernel:         kernel,
		Arch:           normalizeArch(kernel.Machine),
		Virtualization: host.detectVirtualization(cpu.Flags),
		BootTime:       collector.bootTimeFor(host.readOptional("/proc/sys/kernel/random/boot_id")),
		BootID:         host.readOptional("/proc/sys/kernel/random/boot_id"),
		Timezone:       host.readTimezone(),
		Init:           string(initsys.Detect(host.FS)),
		AgentArch:      runtime.GOARCH,
		PHPBuilds:      host.readPHPBuilds(),
	}
	return info, nil
}

// bootTimeFor returns the boot time first observed for bootID. The kernel
// derives btime from the wall clock minus uptime, so it can wobble by a
// second between reads; pinning it per boot keeps the dataset hash stable.
func (collector *System) bootTimeFor(bootID string) string {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	if bootID != "" && bootID == collector.bootID && collector.bootTime != "" {
		return collector.bootTime
	}
	collector.bootID = bootID
	collector.bootTime = collector.host.readBootTime()
	return collector.bootTime
}

func (host *Host) readKernel() Kernel {
	if host.Uname != nil {
		if name, err := host.Uname(); err == nil {
			return Kernel{Name: name.Sysname, Release: name.Release, Version: name.Version, Machine: name.Machine}
		}
	}
	// /proc/sys/kernel mirrors uname(2) except for the machine field, which
	// only newer kernels expose as /proc/sys/kernel/arch.
	return Kernel{
		Name:    host.readOptional("/proc/sys/kernel/ostype"),
		Release: host.readOptional("/proc/sys/kernel/osrelease"),
		Version: host.readOptional("/proc/sys/kernel/version"),
		Machine: host.readOptional("/proc/sys/kernel/arch"),
	}
}

// normalizeArch maps kernel machine names to Go/Debian architecture names.
func normalizeArch(machine string) string {
	switch machine {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64", "aarch64_be":
		return "arm64"
	case "i386", "i486", "i586", "i686":
		return "386"
	case "armv7l", "armv6l", "armhf":
		return "arm"
	case "":
		return runtime.GOARCH
	default:
		return machine
	}
}

func (host *Host) readHostname() string {
	for _, candidate := range []string{"/proc/sys/kernel/hostname", "/etc/hostname"} {
		if hostname := host.readOptional(candidate); hostname != "" && hostname != "(none)" {
			return hostname
		}
	}
	return ""
}

// resolveFQDN mirrors `hostname -f`: /etc/hosts first, then DNS. It only
// accepts names that extend the short hostname, so a stray hosts entry or
// a search-domain answer cannot rename the server.
func (host *Host) resolveFQDN(ctx context.Context, hostname string) string {
	if hostname == "" {
		return ""
	}
	short, _, _ := strings.Cut(hostname, ".")
	if data, err := host.readFile("/etc/hosts"); err == nil {
		for _, line := range lines(data) {
			line, _, _ = strings.Cut(line, "#")
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			names := fields[1:]
			for _, name := range names {
				if !strings.EqualFold(name, hostname) && !strings.EqualFold(name, short) {
					continue
				}
				// The canonical name is the first one on the line.
				if canonical := strings.TrimSuffix(names[0], "."); strings.Contains(canonical, ".") && fqdnMatches(canonical, short) {
					return strings.ToLower(canonical)
				}
			}
		}
	}
	if strings.Contains(hostname, ".") {
		return strings.ToLower(hostname)
	}
	if host.LookupCNAME != nil {
		lookupContext, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if canonical, err := host.LookupCNAME(lookupContext, hostname); err == nil {
			canonical = strings.TrimSuffix(canonical, ".")
			if strings.Contains(canonical, ".") && fqdnMatches(canonical, short) {
				return strings.ToLower(canonical)
			}
		}
	}
	return strings.ToLower(hostname)
}

func fqdnMatches(fqdn, short string) bool {
	first, _, _ := strings.Cut(fqdn, ".")
	return strings.EqualFold(first, short)
}

func (host *Host) readBootTime() string {
	data, err := host.readFile("/proc/stat")
	if err != nil {
		return ""
	}
	for _, line := range lines(data) {
		if value, ok := strings.CutPrefix(line, "btime "); ok {
			seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			if err != nil || seconds <= 0 {
				return ""
			}
			return time.Unix(seconds, 0).UTC().Format(time.RFC3339)
		}
	}
	return ""
}

// readTimezone returns the IANA zone from /etc/timezone (Debian family),
// the /etc/localtime symlink, or /etc/sysconfig/clock (EL6/EL7).
func (host *Host) readTimezone() string {
	if zone := host.readOptional("/etc/timezone"); zone != "" {
		return zone
	}
	if target, err := host.readLink("/etc/localtime"); err == nil {
		if _, zone, ok := strings.Cut(target, "zoneinfo/"); ok && zone != "" {
			return strings.TrimPrefix(zone, "posix/")
		}
	}
	if data, err := host.readFile("/etc/sysconfig/clock"); err == nil {
		if zone := parseKeyValue(data)["ZONE"]; zone != "" {
			return zone
		}
	}
	if host.exists("/etc/localtime") {
		return "unknown"
	}
	return "UTC"
}

// BootID is the kernel's random boot ID, which changes with every boot.
func (host *Host) BootID() (string, error) {
	data, err := host.readFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(data))
	if id == "" {
		return "", errors.New("the boot ID is empty")
	}
	return id, nil
}

// BootTime is the kernel boot time from /proc/stat, nil when unknown.
func (host *Host) BootTime() *time.Time {
	parsed, err := time.Parse(time.RFC3339, host.readBootTime())
	if err != nil {
		return nil
	}
	return &parsed
}
