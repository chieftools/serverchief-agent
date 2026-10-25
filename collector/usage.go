package collector

import (
	"slices"
	"strconv"
	"strings"
	"time"
)

// Usage is one reading of the kernel counters and gauges host metrics are
// derived from. Counters only mean something as the difference between two
// readings; the metrics package turns pairs of readings into samples.
type Usage struct {
	// At is when the reading was taken, for the elapsed time between two.
	At time.Time

	CPU CPUTimes
	// Cores is the number of online CPUs.
	Cores        int
	ProcsRunning uint64
	ProcsBlocked uint64

	Load [3]float64
	// Threads is the number of scheduling entities, from /proc/loadavg.
	Threads uint64

	Memory MemoryUsage

	Filesystems []FilesystemUsage
	Disks       []DiskCounters
	Interfaces  []InterfaceCounters

	// TCPEstablished and TCPTimeWait are only set when HasTCP.
	HasTCP         bool
	TCPEstablished uint64
	TCPTimeWait    uint64

	// Uptime is in seconds, 0 when unknown.
	Uptime float64
}

// CPUTimes are the aggregate /proc/stat CPU counters in clock ticks. Guest
// time is already included in User and Nice.
type CPUTimes struct {
	User, Nice, System, Idle, IOWait, IRQ, SoftIRQ, Steal uint64
}

// Total is the sum of every state.
func (times CPUTimes) Total() uint64 {
	return times.User + times.Nice + times.System + times.Idle + times.IOWait + times.IRQ + times.SoftIRQ + times.Steal
}

// MemoryUsage is from /proc/meminfo, in bytes.
type MemoryUsage struct {
	Total     uint64
	Available uint64
	// Cached is Buffers + Cached + SReclaimable, the "buff/cache" of free(1).
	Cached    uint64
	SwapTotal uint64
	SwapFree  uint64
}

// FilesystemUsage is statfs(2) of one mounted file system.
type FilesystemUsage struct {
	MountPoint string
	FSStats
}

// DiskCounters are /proc/diskstats counters of one whole block device.
type DiskCounters struct {
	// Name is the kernel name, or the device-mapper name (vg-root) for dm-N.
	Name         string
	Reads        uint64
	Writes       uint64
	ReadSectors  uint64
	WriteSectors uint64
	// IOMillis is the time the device had I/O in flight.
	IOMillis uint64
	// Underlying is true for a disk with no other block device beneath it.
	// Device-mapper (LVM) and md (RAID) devices are not: their I/O also
	// shows on the disks they are built on, so host totals leave them out.
	Underlying bool
}

// InterfaceCounters are /proc/net/dev counters of one interface.
type InterfaceCounters struct {
	Name      string
	RxBytes   uint64
	TxBytes   uint64
	RxPackets uint64
	TxPackets uint64
	// Errors and Drops add the receive and transmit directions.
	Errors uint64
	Drops  uint64
	// Underlying is true for an interface whose traffic counts towards the
	// host's total: one backed by a (virtual) network device. Bonds,
	// bridges, VLANs and tunnels carry traffic that also crosses such a
	// device. In a container without devices it is every interface not
	// stacked on another.
	Underlying bool
}

// MaximumInstances caps each group of per-instance usage (file systems,
// disks, interfaces), so a host with thousands of mounts or virtual
// interfaces cannot flood the metrics store.
const MaximumInstances = 64

// skippedInterfacePrefixes are container and bridge plumbing whose traffic
// is already counted on a physical interface.
var skippedInterfacePrefixes = []string{"veth", "docker", "br-", "cali", "flannel", "cni", "virbr", "vnet", "tap", "fwbr", "fwpr", "fwln", "lxc"}

// ReadUsage reads every source; a missing source leaves its part empty.
func (host *Host) ReadUsage() Usage {
	return host.readUsage(true)
}

// ReadLiveUsage reads every source except file systems, for live samples
// each second: statfs on a slow network mount must not hold them up.
func (host *Host) ReadLiveUsage() Usage {
	return host.readUsage(false)
}

func (host *Host) readUsage(filesystems bool) Usage {
	usage := Usage{At: host.now()}
	if data, err := host.readFile("/proc/stat"); err == nil {
		usage.CPU, usage.Cores, usage.ProcsRunning, usage.ProcsBlocked = parseStat(data)
	}
	if data, err := host.readFile("/proc/loadavg"); err == nil {
		usage.Load, usage.Threads = parseLoadavg(data)
	}
	if data, err := host.readFile("/proc/meminfo"); err == nil {
		usage.Memory = parseMemoryUsage(data)
	}
	if filesystems {
		usage.Filesystems = host.readFilesystemUsage()
	}
	usage.Disks = host.readDiskCounters()
	if data, err := host.readFile("/proc/net/dev"); err == nil {
		usage.Interfaces = host.markUnderlyingInterfaces(parseNetDev(data))
	}
	if data, err := host.readFile("/proc/net/snmp"); err == nil {
		if established, ok := parseTCPEstablished(data); ok {
			usage.HasTCP = true
			usage.TCPEstablished = established
			if sockstat, err := host.readFile("/proc/net/sockstat"); err == nil {
				usage.TCPTimeWait = parseTimeWait(sockstat)
			}
		}
	}
	if data, err := host.readFile("/proc/uptime"); err == nil {
		if fields := strings.Fields(string(data)); len(fields) > 0 {
			usage.Uptime, _ = strconv.ParseFloat(fields[0], 64)
		}
	}
	return usage
}

func parseStat(data []byte) (times CPUTimes, cores int, running, blocked uint64) {
	for _, line := range lines(data) {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch {
		case fields[0] == "cpu":
			values := make([]uint64, 8)
			for index := range values {
				if index+1 < len(fields) {
					values[index], _ = strconv.ParseUint(fields[index+1], 10, 64)
				}
			}
			times = CPUTimes{
				User: values[0], Nice: values[1], System: values[2], Idle: values[3],
				IOWait: values[4], IRQ: values[5], SoftIRQ: values[6], Steal: values[7],
			}
		case strings.HasPrefix(fields[0], "cpu"):
			cores++
		case fields[0] == "procs_running" && len(fields) > 1:
			running, _ = strconv.ParseUint(fields[1], 10, 64)
		case fields[0] == "procs_blocked" && len(fields) > 1:
			blocked, _ = strconv.ParseUint(fields[1], 10, 64)
		}
	}
	return times, cores, running, blocked
}

func parseLoadavg(data []byte) (load [3]float64, threads uint64) {
	fields := strings.Fields(string(data))
	for index := 0; index < 3 && index < len(fields); index++ {
		load[index], _ = strconv.ParseFloat(fields[index], 64)
	}
	if len(fields) > 3 {
		if _, total, ok := strings.Cut(fields[3], "/"); ok {
			threads, _ = strconv.ParseUint(total, 10, 64)
		}
	}
	return load, threads
}

func parseMemoryUsage(data []byte) MemoryUsage {
	values := make(map[string]uint64)
	for _, line := range lines(data) {
		key, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		value, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		if len(fields) > 1 && fields[1] == "kB" {
			value *= 1024
		}
		values[key] = value
	}
	usage := MemoryUsage{
		Total:     values["MemTotal"],
		Available: values["MemAvailable"],
		Cached:    values["Buffers"] + values["Cached"] + values["SReclaimable"],
		SwapTotal: values["SwapTotal"],
		SwapFree:  values["SwapFree"],
	}
	if _, ok := values["MemAvailable"]; !ok {
		// Kernels before 3.14 have no MemAvailable; free plus page cache
		// is what free(1) estimated there.
		usage.Available = min(usage.Total, values["MemFree"]+values["Buffers"]+values["Cached"])
	}
	return usage
}

// readFilesystemUsage covers the inventory's local file systems, once per
// file system, so bind mounts of one file system are not counted twice.
func (host *Host) readFilesystemUsage() []FilesystemUsage {
	if host.Statfs == nil {
		return nil
	}
	filesystems := host.readMountedFilesystems()
	mounts := host.readMountInfo()
	// Of several mounts of one file system, keep the one that mounts it
	// whole rather than a bind mount of a directory in it.
	preferred := make(map[string]string)
	for _, filesystem := range filesystems {
		if info, ok := mounts[filesystem.MountPoint]; ok {
			if _, found := preferred[info.Device]; !found || info.Root == "/" && mounts[preferred[info.Device]].Root != "/" {
				preferred[info.Device] = filesystem.MountPoint
			}
		}
	}
	devices := make(map[string]bool)
	var result []FilesystemUsage
	for _, filesystem := range filesystems {
		if filesystem.Network {
			continue
		}
		if info, ok := mounts[filesystem.MountPoint]; ok {
			if preferred[info.Device] != filesystem.MountPoint {
				continue
			}
		} else if strings.HasPrefix(filesystem.Device, "/") {
			// Without mountinfo, the device path tells file systems apart.
			if devices[filesystem.Device] {
				continue
			}
			devices[filesystem.Device] = true
		}
		stats, err := host.Statfs(filesystem.MountPoint)
		if err != nil || stats.SizeBytes == 0 {
			continue
		}
		result = append(result, FilesystemUsage{MountPoint: filesystem.MountPoint, FSStats: stats})
		if len(result) == MaximumInstances {
			break
		}
	}
	return result
}

// mountInfo is a mount's file system, by its major:minor device number,
// and the directory of that file system the mount shows: "/" for the whole
// file system, a subdirectory for a bind mount of one.
type mountInfo struct {
	Device string
	Root   string
}

// readMountInfo reads /proc/self/mountinfo by mount point; a later mount
// over the same point hides the earlier one. Empty when it is unreadable.
func (host *Host) readMountInfo() map[string]mountInfo {
	data, err := host.readFile("/proc/self/mountinfo")
	if err != nil {
		return nil
	}
	mounts := make(map[string]mountInfo)
	for _, line := range lines(data) {
		fields := strings.Fields(line)
		if len(fields) < 5 || !strings.Contains(fields[2], ":") {
			continue
		}
		mounts[unescapeMount(fields[4])] = mountInfo{Device: fields[2], Root: unescapeMount(fields[3])}
	}
	return mounts
}

// readDiskCounters reads whole devices from /proc/diskstats: those listed in
// /sys/block, without the virtual devices the inventory skips.
func (host *Host) readDiskCounters() []DiskCounters {
	data, err := host.readFile("/proc/diskstats")
	if err != nil {
		return nil
	}
	whole := make(map[string]bool)
	if entries, err := host.readDir("/sys/block"); err == nil {
		for _, entry := range entries {
			// Like the inventory: no virtual devices, no empty optical drives.
			name := entry.Name()
			if !hasAnyPrefix(name, skippedBlockPrefixes) && host.readSectors("/sys/block/"+name+"/size") > 0 {
				whole[name] = true
			}
		}
	}
	var result []DiskCounters
	for _, line := range lines(data) {
		fields := strings.Fields(line)
		if len(fields) < 13 || !whole[fields[2]] {
			continue
		}
		number := func(index int) uint64 {
			value, _ := strconv.ParseUint(fields[index], 10, 64)
			return value
		}
		name := fields[2]
		underlying := true
		if slaves, err := host.readDir("/sys/block/" + name + "/slaves"); err == nil && len(slaves) > 0 {
			underlying = false
		}
		if strings.HasPrefix(name, "dm-") {
			if mapped := host.readOptional("/sys/block/" + name + "/dm/name"); mapped != "" {
				name = mapped
			}
		}
		result = append(result, DiskCounters{
			Name:         name,
			Reads:        number(3),
			ReadSectors:  number(5),
			Writes:       number(7),
			WriteSectors: number(9),
			IOMillis:     number(12),
			Underlying:   underlying,
		})
	}
	slices.SortFunc(result, func(a, b DiskCounters) int { return strings.Compare(a.Name, b.Name) })
	if len(result) > MaximumInstances {
		result = result[:MaximumInstances]
	}
	return result
}

func parseNetDev(data []byte) []InterfaceCounters {
	var result []InterfaceCounters
	for _, line := range lines(data) {
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		fields := strings.Fields(rest)
		if name == "lo" || len(fields) < 16 || hasAnyPrefix(name, skippedInterfacePrefixes) {
			continue
		}
		number := func(index int) uint64 {
			value, _ := strconv.ParseUint(fields[index], 10, 64)
			return value
		}
		result = append(result, InterfaceCounters{
			Name:      name,
			RxBytes:   number(0),
			RxPackets: number(1),
			Errors:    number(2) + number(10),
			Drops:     number(3) + number(11),
			TxBytes:   number(8),
			TxPackets: number(9),
		})
	}
	slices.SortFunc(result, func(a, b InterfaceCounters) int { return strings.Compare(a.Name, b.Name) })
	if len(result) > MaximumInstances {
		result = result[:MaximumInstances]
	}
	return result
}

// markUnderlyingInterfaces marks the interfaces backed by a device, or,
// when none is (a container's veth), those not stacked on another.
func (host *Host) markUnderlyingInterfaces(interfaces []InterfaceCounters) []InterfaceCounters {
	backed := false
	for index := range interfaces {
		interfaces[index].Underlying = host.exists("/sys/class/net/" + interfaces[index].Name + "/device")
		backed = backed || interfaces[index].Underlying
	}
	if backed {
		return interfaces
	}
	for index := range interfaces {
		interfaces[index].Underlying = true
		entries, _ := host.readDir("/sys/class/net/" + interfaces[index].Name)
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "lower_") {
				interfaces[index].Underlying = false
				break
			}
		}
	}
	return interfaces
}

// parseTCPEstablished reads CurrEstab from the Tcp lines of /proc/net/snmp,
// which count IPv4 and IPv6 connections.
func parseTCPEstablished(data []byte) (uint64, bool) {
	var header []string
	for _, line := range lines(data) {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "Tcp:" {
			continue
		}
		if header == nil {
			header = fields
			continue
		}
		index := slices.Index(header, "CurrEstab")
		if index < 0 || index >= len(fields) {
			return 0, false
		}
		value, err := strconv.ParseUint(fields[index], 10, 64)
		return value, err == nil
	}
	return 0, false
}

func parseTimeWait(data []byte) uint64 {
	for _, line := range lines(data) {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "TCP:" {
			continue
		}
		for index := 1; index+1 < len(fields); index += 2 {
			if fields[index] == "tw" {
				value, _ := strconv.ParseUint(fields[index+1], 10, 64)
				return value
			}
		}
	}
	return 0
}
