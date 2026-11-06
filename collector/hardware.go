package collector

import (
	"context"
	"io/fs"
	"slices"
	"strconv"
	"strings"
)

// HardwareInfo is the payload of the hardware dataset. It only carries
// capacities; usage figures change constantly and belong to metrics.
type HardwareInfo struct {
	CPU          CPUInfo       `json:"cpu"`
	Memory       MemoryInfo    `json:"memory"`
	BlockDevices []BlockDevice `json:"block_devices"`
	Filesystems  []Filesystem  `json:"filesystems"`
}

// MemoryInfo holds total RAM and swap.
type MemoryInfo struct {
	TotalBytes     uint64 `json:"total_bytes"`
	SwapTotalBytes uint64 `json:"swap_total_bytes"`
}

// BlockDevice is a whole disk from /sys/block.
type BlockDevice struct {
	Name       string      `json:"name"`
	Type       string      `json:"type"`
	SizeBytes  uint64      `json:"size_bytes"`
	Rotational bool        `json:"rotational"`
	Removable  bool        `json:"removable"`
	ReadOnly   bool        `json:"read_only"`
	Vendor     string      `json:"vendor"`
	Model      string      `json:"model"`
	Partitions []Partition `json:"partitions"`
}

// Partition is a partition of a block device.
type Partition struct {
	Name      string `json:"name"`
	SizeBytes uint64 `json:"size_bytes"`
}

// Filesystem is a mounted, storage-backed file system.
type Filesystem struct {
	Device     string `json:"device"`
	MountPoint string `json:"mount_point"`
	Type       string `json:"type"`
	ReadOnly   bool   `json:"read_only"`
	Network    bool   `json:"network"`
	SizeBytes  uint64 `json:"size_bytes"`
}

// Normalize sorts the unordered lists for canonical encoding.
func (info *HardwareInfo) Normalize() {
	slices.SortFunc(info.BlockDevices, func(a, b BlockDevice) int { return strings.Compare(a.Name, b.Name) })
	for index := range info.BlockDevices {
		slices.SortFunc(info.BlockDevices[index].Partitions, func(a, b Partition) int { return strings.Compare(a.Name, b.Name) })
	}
	slices.SortFunc(info.Filesystems, func(a, b Filesystem) int {
		if order := strings.Compare(a.MountPoint, b.MountPoint); order != 0 {
			return order
		}
		return strings.Compare(a.Device, b.Device)
	})
}

// Hardware collects the hardware dataset.
type Hardware struct {
	host *Host
}

func NewHardware(host *Host) *Hardware { return &Hardware{host: host} }

func (*Hardware) Name() string { return "hardware" }

func (collector *Hardware) Collect(context.Context) (any, error) {
	host := collector.host
	info := &HardwareInfo{
		CPU:          host.readCPUInfo(),
		Memory:       host.readMemory(),
		BlockDevices: host.readBlockDevices(),
		Filesystems:  host.readFilesystems(),
	}
	info.Normalize()
	return info, nil
}

func parseMeminfo(data []byte) MemoryInfo {
	var info MemoryInfo
	for _, line := range lines(data) {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields := strings.Fields(value)
		if len(fields) == 0 {
			continue
		}
		amount, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		if len(fields) > 1 && strings.EqualFold(fields[1], "kB") {
			amount *= 1024
		}
		switch key {
		case "MemTotal":
			info.TotalBytes = amount
		case "SwapTotal":
			info.SwapTotalBytes = amount
		}
	}
	return info
}

func (host *Host) readMemory() MemoryInfo {
	data, err := host.readFile("/proc/meminfo")
	if err != nil {
		return MemoryInfo{}
	}
	return parseMeminfo(data)
}

// skippedBlockPrefixes are virtual devices that never hold server data or
// come and go at runtime.
var skippedBlockPrefixes = []string{"loop", "ram", "zram", "nbd", "fd"}

func (host *Host) readBlockDevices() []BlockDevice {
	devices := []BlockDevice{}
	entries, err := host.readDir("/sys/block")
	if err != nil {
		return devices
	}
	for _, entry := range entries {
		name := entry.Name()
		if hasAnyPrefix(name, skippedBlockPrefixes) {
			continue
		}
		base := "/sys/block/" + name
		size := host.readSectors(base + "/size")
		if size == 0 {
			// Empty card readers and optical drives without media.
			continue
		}
		device := BlockDevice{
			Name:       name,
			Type:       blockDeviceType(name),
			SizeBytes:  size,
			Rotational: host.readOptional(base+"/queue/rotational") == "1",
			Removable:  host.readOptional(base+"/removable") == "1",
			ReadOnly:   host.readOptional(base+"/ro") == "1",
			Vendor:     blockVendor(host.readOptional(base + "/device/vendor")),
			Model:      host.readOptional(base + "/device/model"),
			Partitions: []Partition{},
		}
		if children, err := host.readDir(base); err == nil {
			for _, child := range children {
				if !strings.HasPrefix(child.Name(), name) || !host.exists(base+"/"+child.Name()+"/partition") {
					continue
				}
				device.Partitions = append(device.Partitions, Partition{
					Name:      child.Name(),
					SizeBytes: host.readSectors(base + "/" + child.Name() + "/size"),
				})
			}
		}
		devices = append(devices, device)
	}
	return devices
}

// readSectors reads a sysfs size file, which counts 512-byte sectors
// regardless of the device's logical block size.
func (host *Host) readSectors(name string) uint64 {
	value, err := host.readString(name)
	if err != nil {
		return 0
	}
	sectors, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0
	}
	return sectors * 512
}

// blockVendor drops numeric PCI/virtio vendor IDs ("0x1af4"), which some
// drivers expose instead of a name.
func blockVendor(vendor string) string {
	if strings.HasPrefix(vendor, "0x") {
		if _, err := strconv.ParseUint(vendor[2:], 16, 32); err == nil {
			return ""
		}
	}
	return vendor
}

func blockDeviceType(name string) string {
	switch {
	case strings.HasPrefix(name, "nvme"):
		return "nvme"
	case strings.HasPrefix(name, "vd"):
		return "virtio"
	case strings.HasPrefix(name, "xvd"):
		return "xen"
	case strings.HasPrefix(name, "sd"):
		return "scsi"
	case strings.HasPrefix(name, "hd"):
		return "ide"
	case strings.HasPrefix(name, "md"):
		return "raid"
	case strings.HasPrefix(name, "dm-"):
		return "device-mapper"
	case strings.HasPrefix(name, "mmcblk"):
		return "mmc"
	case strings.HasPrefix(name, "sr"):
		return "optical"
	case strings.HasPrefix(name, "rbd"):
		return "ceph"
	case strings.HasPrefix(name, "zd"):
		return "zvol"
	default:
		return "other"
	}
}

// pseudoFilesystems carry no persistent storage.
var pseudoFilesystems = map[string]bool{
	"autofs": true, "binfmt_misc": true, "bpf": true, "cgroup": true, "cgroup2": true,
	"configfs": true, "debugfs": true, "devfs": true, "devpts": true, "devtmpfs": true,
	"efivarfs": true, "fusectl": true, "hugetlbfs": true, "mqueue": true, "nsfs": true,
	"proc": true, "pstore": true, "ramfs": true, "rootfs": true, "rpc_pipefs": true,
	"securityfs": true, "selinuxfs": true, "squashfs": true, "sysfs": true, "tmpfs": true,
	"tracefs": true, "nfsd": true, "fuse.lxcfs": true, "fuse.gvfsd-fuse": true,
	"fuse.portal": true, "fuse.snapfuse": true, "iso9660": true,
}

var networkFilesystems = map[string]bool{
	"nfs": true, "nfs4": true, "cifs": true, "smb3": true, "smbfs": true, "ceph": true,
	"glusterfs": true, "fuse.glusterfs": true, "fuse.sshfs": true, "9p": true,
	"fuse.s3fs": true, "fuse.rclone": true, "lustre": true,
}

// skippedMountPrefixes hold kernel and runtime interfaces even when the
// file system type looks real (bind mounts into /proc, /sys or /dev).
var skippedMountPrefixes = []string{"/proc", "/sys", "/dev", "/run/user/", "/snap/", "/var/lib/docker/", "/var/lib/containers/"}

func parseMounts(data []byte) []Filesystem {
	filesystems := []Filesystem{}
	seen := make(map[string]bool)
	for _, line := range lines(data) {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		device, mountPoint, fsType := unescapeMount(fields[0]), unescapeMount(fields[1]), fields[2]
		if pseudoFilesystems[fsType] || skippedMount(mountPoint) {
			continue
		}
		// A later mount over the same point hides the earlier one.
		if seen[mountPoint] {
			for index := range filesystems {
				if filesystems[index].MountPoint == mountPoint {
					filesystems = slices.Delete(filesystems, index, index+1)
					break
				}
			}
		}
		seen[mountPoint] = true
		options := strings.Split(fields[3], ",")
		filesystems = append(filesystems, Filesystem{
			Device:     device,
			MountPoint: mountPoint,
			Type:       fsType,
			ReadOnly:   slices.Contains(options, "ro"),
			Network:    networkFilesystems[fsType] || strings.HasPrefix(fsType, "nfs"),
		})
	}
	return filesystems
}

func skippedMount(mountPoint string) bool {
	for _, prefix := range skippedMountPrefixes {
		trimmed := strings.TrimSuffix(prefix, "/")
		if mountPoint == trimmed || strings.HasPrefix(mountPoint, trimmed+"/") {
			return true
		}
	}
	return false
}

// unescapeMount decodes the octal escapes /proc/mounts uses for spaces,
// tabs, newlines and backslashes.
func unescapeMount(value string) string {
	if !strings.Contains(value, `\`) {
		return value
	}
	var builder strings.Builder
	for index := 0; index < len(value); index++ {
		if value[index] == '\\' && index+3 < len(value) {
			if code, err := strconv.ParseUint(value[index+1:index+4], 8, 8); err == nil {
				builder.WriteByte(byte(code))
				index += 3
				continue
			}
		}
		builder.WriteByte(value[index])
	}
	return builder.String()
}

func (host *Host) readFilesystems() []Filesystem {
	filesystems := host.readMountedFilesystems()
	if host.Statfs == nil {
		return filesystems
	}
	for index := range filesystems {
		// statfs on a hung network mount blocks; capacities of remote file
		// systems are not inventory anyway.
		if filesystems[index].Network {
			continue
		}
		if stats, err := host.Statfs(filesystems[index].MountPoint); err == nil {
			filesystems[index].SizeBytes = stats.SizeBytes
		}
	}
	return filesystems
}

// readMountedFilesystems lists the mounted file systems the inventory
// covers, without their sizes.
func (host *Host) readMountedFilesystems() []Filesystem {
	// The daemon's own mount namespace is sandboxed (ProtectSystem=strict
	// remounts everything read-only, PrivateTmp and ReadWritePaths add bind
	// mounts), so the mounts of PID 1 come first: the host's, or the
	// container's own in a container.
	var data []byte
	var err error
	for _, name := range []string{"/proc/1/mounts", "/proc/mounts", "/proc/self/mounts"} {
		if data, err = host.readFile(name); err == nil && len(data) > 0 {
			break
		}
	}
	if err != nil {
		return []Filesystem{}
	}
	parsed := parseMounts(data)
	filesystems := parsed[:0]
	for _, filesystem := range parsed {
		// Containers bind-mount single files such as /etc/hosts; those are
		// not file systems of their own.
		if info, err := statPath(host, filesystem.MountPoint); err == nil && !info.IsDir() {
			continue
		}
		filesystems = append(filesystems, filesystem)
	}
	return filesystems
}

func statPath(host *Host, name string) (fs.FileInfo, error) {
	return fs.Stat(host.FS, relative(name))
}

func hasAnyPrefix(value string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}
