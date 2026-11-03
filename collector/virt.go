package collector

import (
	"bytes"
	"slices"
	"strings"
)

// Virtualization describes what the host runs on. It is detected from
// files alone, so it works on hosts without systemd-detect-virt.
type Virtualization struct {
	// Role is "container", "vm" or "none" (bare metal or undetected).
	Role string `json:"role"`
	// Technology is the hypervisor or container runtime, such as kvm,
	// vmware, hyperv, xen, docker, lxc or openvz. Empty when unknown.
	Technology string `json:"technology"`
	// Vendor and Product come from DMI and identify the platform even on
	// bare metal ("Dell Inc." / "PowerEdge R640", "Hetzner" / "vServer").
	Vendor  string `json:"vendor"`
	Product string `json:"product"`
	// Lxcfs is true in a container whose /proc/meminfo comes from lxcfs,
	// which shows the container's own memory, CPU time and load. Without
	// it, a container's /proc shows those of the host.
	Lxcfs bool `json:"lxcfs"`
}

type dmiRule struct {
	match      string
	technology string
}

// dmiRules are matched case-insensitively against sys_vendor,
// product_name, board_vendor and bios_vendor, in that order.
var dmiRules = []dmiRule{
	{"kvm", "kvm"},
	{"qemu", "qemu"},
	{"vmware", "vmware"},
	{"innotek", "oracle"},
	{"virtualbox", "oracle"},
	{"xen", "xen"},
	{"bochs", "bochs"},
	{"parallels", "parallels"},
	{"bhyve", "bhyve"},
	{"amazon ec2", "amazon"},
	{"google compute engine", "google"},
	{"openstack", "kvm"},
	{"digitalocean", "kvm"},
	{"hetzner", "kvm"},
	{"apple virtualization", "apple"},
}

func (host *Host) detectVirtualization(cpuFlags []string) Virtualization {
	virtualization := Virtualization{
		Role:    "none",
		Vendor:  host.readOptional("/sys/class/dmi/id/sys_vendor"),
		Product: host.readOptional("/sys/class/dmi/id/product_name"),
	}

	if technology := host.detectContainer(); technology != "" {
		virtualization.Role = "container"
		virtualization.Technology = technology
		virtualization.Lxcfs = host.procFromLxcfs()
		return virtualization
	}
	technology := host.detectVM(virtualization.Vendor, virtualization.Product, cpuFlags)
	// QEMU with hardware acceleration is KVM; the guest sees kvm-clock.
	if technology == "qemu" || technology == "unknown" || technology == "" {
		if strings.Contains(host.readOptional("/sys/devices/system/clocksource/clocksource0/available_clocksource"), "kvm-clock") {
			technology = "kvm"
		}
	}
	if technology != "" {
		virtualization.Role = "vm"
		if technology != "unknown" {
			virtualization.Technology = technology
		}
	}
	return virtualization
}

func (host *Host) detectContainer() string {
	// systemd-nspawn, LXC, podman and others export container= to PID 1.
	// Reading /proc/1/environ needs root; failure falls through to hints.
	if data, err := host.readFile("/proc/1/environ"); err == nil {
		for _, variable := range bytes.Split(data, []byte{0}) {
			if value, ok := bytes.CutPrefix(variable, []byte("container=")); ok && len(value) > 0 {
				return normalizeContainer(string(value))
			}
		}
	}
	if host.exists("/run/.containerenv") {
		return "podman"
	}
	if host.exists("/.dockerenv") {
		return "docker"
	}
	if host.exists("/proc/vz") && !host.exists("/proc/bc") {
		return "openvz"
	}
	if release := strings.ToLower(host.readOptional("/proc/sys/kernel/osrelease")); strings.Contains(release, "microsoft") {
		return "wsl"
	}
	if data, err := host.readFile("/proc/1/cgroup"); err == nil {
		cgroup := string(data)
		switch {
		case strings.Contains(cgroup, "/docker/") || strings.Contains(cgroup, "/docker-"):
			return "docker"
		case strings.Contains(cgroup, "/kubepods"):
			return "kubernetes"
		case strings.Contains(cgroup, "/lxc/") || strings.Contains(cgroup, "/lxc.payload"):
			return "lxc"
		}
	}
	return ""
}

func normalizeContainer(value string) string {
	switch value {
	case "lxc-libvirt":
		return "lxc"
	case "systemd-nspawn":
		return "nspawn"
	case "oci":
		return "podman"
	default:
		return value
	}
}

func (host *Host) detectVM(vendor, product string, cpuFlags []string) string {
	dmi := []string{
		strings.ToLower(vendor),
		strings.ToLower(product),
		strings.ToLower(host.readOptional("/sys/class/dmi/id/board_vendor")),
		strings.ToLower(host.readOptional("/sys/class/dmi/id/bios_vendor")),
	}
	// Hyper-V reports "Microsoft Corporation" / "Virtual Machine"; Surface
	// hardware is also made by Microsoft, so require the product name.
	if dmi[0] == "microsoft corporation" && dmi[1] == "virtual machine" {
		return "hyperv"
	}
	for _, value := range dmi {
		if value == "" {
			continue
		}
		for _, rule := range dmiRules {
			if strings.Contains(value, rule.match) {
				return rule.technology
			}
		}
	}

	if hypervisor := host.readOptional("/sys/hypervisor/type"); hypervisor == "xen" {
		return "xen"
	}
	// A Xen dom0 advertises control_d and runs the hypervisor rather than
	// inside it.
	if capabilities, err := host.readString("/proc/xen/capabilities"); err == nil && !strings.Contains(capabilities, "control_d") {
		return "xen"
	}
	for _, flag := range cpuFlags {
		if flag == "hypervisor" {
			return "unknown"
		}
	}
	return ""
}

// procFromLxcfs reports whether lxcfs is mounted over /proc/meminfo.
func (host *Host) procFromLxcfs() bool {
	data, err := host.readFile("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	for _, line := range lines(data) {
		fields := strings.Fields(line)
		// The mount point is the fifth field, the type follows the "-".
		separator := slices.Index(fields, "-")
		if len(fields) > 4 && separator > 4 && len(fields) > separator+1 &&
			unescapeMount(fields[4]) == "/proc/meminfo" && strings.HasPrefix(fields[separator+1], "fuse.lxcfs") {
			return true
		}
	}
	return false
}

// Container is the container technology the host runs in, such as docker,
// lxc or nspawn, or "" outside a container.
func (host *Host) Container() string {
	return host.detectContainer()
}
