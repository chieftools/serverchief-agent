package collector

import (
	"context"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ZFSPool is an imported ZFS pool. State comes from the kernel module's
// kstat, the layout, error counters and scrub from `zpool status`.
type ZFSPool struct {
	Name string `json:"name"`
	// State is ONLINE, DEGRADED, FAULTED, OFFLINE, REMOVED, UNAVAIL or
	// SUSPENDED.
	State string  `json:"state"`
	Scan  ZFSScan `json:"scan"`
	// DataErrors counts the permanent errors in files the pool could not
	// repair.
	DataErrors int         `json:"data_errors"`
	Vdevs      []ZFSVdev   `json:"vdevs"`
	Volumes    []ZFSVolume `json:"volumes"`
}

// ZFSScan is the pool's last or running scrub or resilver, without its
// progress.
type ZFSScan struct {
	// Function is scrub or resilver, "" when none ran.
	Function string `json:"function"`
	// State is none, in_progress, finished, canceled or paused.
	State string `json:"state"`
	// Errors counts what a finished scan could not repair.
	Errors int `json:"errors"`
	// EndedAt is when a finished or canceled scan ended, in UTC.
	EndedAt *string `json:"ended_at"`
}

// ZFSVdev is a top-level vdev: a mirror, raidz or draid group, or a single
// disk, in the pool's data, log, cache, spare, special or dedup class.
type ZFSVdev struct {
	Name     string      `json:"name"`
	Type     string      `json:"type"`
	Class    string      `json:"class"`
	State    string      `json:"state"`
	Read     uint64      `json:"read"`
	Write    uint64      `json:"write"`
	Checksum uint64      `json:"checksum"`
	Devices  []ZFSDevice `json:"devices"`
}

// ZFSDevice is a disk or partition of a vdev. Device is the kernel name,
// or the vdev GUID when the device is gone.
type ZFSDevice struct {
	Device   string `json:"device"`
	State    string `json:"state"`
	Read     uint64 `json:"read"`
	Write    uint64 `json:"write"`
	Checksum uint64 `json:"checksum"`
	// Note is what zpool adds after the counters, such as
	// "(resilvering)" or "was /dev/sdb1".
	Note string `json:"note"`
}

// ZFSVolume is a zvol: a block device backed by the pool, such as a
// virtual machine's disk.
type ZFSVolume struct {
	Name      string `json:"name"`
	Device    string `json:"device"`
	SizeBytes uint64 `json:"size_bytes"`
}

// zpoolTimeout bounds `zpool status`, which blocks on a suspended pool.
const zpoolTimeout = 15 * time.Second

// maxVolumes caps the zvols listed per pool.
const maxVolumes = 1000

func (host *Host) readZFSPools(ctx context.Context) []ZFSPool {
	// The kstat state of each imported pool, OpenZFS 0.8 and later.
	states := map[string]string{}
	if entries, err := host.readDir("/proc/spl/kstat/zfs"); err == nil {
		for _, entry := range entries {
			if state := host.readOptional("/proc/spl/kstat/zfs/" + entry.Name() + "/state"); state != "" {
				states[entry.Name()] = state
			}
		}
	}
	if len(states) == 0 && !host.isDir("/sys/module/zfs") {
		return []ZFSPool{}
	}

	byName := map[string]ZFSPool{}
	// Without zpool, or when it fails, the pools keep only their state.
	if zpool := host.findCommand("zpool"); zpool != "" {
		ctx, cancel := context.WithTimeout(ctx, zpoolTimeout)
		defer cancel()
		if result, err := host.run(ctx, zpool, "status", "-L", "-P", "-p"); err == nil && result.ExitCode == 0 {
			byName = parseZpoolStatus(result.Stdout, host.now().Location())
		}
	}
	for name, state := range states {
		pool, ok := byName[name]
		if !ok {
			pool = ZFSPool{Name: name, Scan: ZFSScan{State: "none"}, Vdevs: []ZFSVdev{}}
		}
		pool.State = state
		byName[name] = pool
	}

	pools := make([]ZFSPool, 0, len(byName))
	for _, pool := range byName {
		pool.Volumes = host.readZFSVolumes(pool.Name)
		pools = append(pools, pool)
	}
	slices.SortFunc(pools, func(a, b ZFSPool) int { return strings.Compare(a.Name, b.Name) })
	return pools
}

// readZFSVolumes maps the udev links in /dev/zvol/<pool> to their zd
// devices, leaving out the links to partitions.
func (host *Host) readZFSVolumes(pool string) []ZFSVolume {
	volumes := []ZFSVolume{}
	root := "dev/zvol/" + pool
	if !host.isDir("/" + root) {
		return volumes
	}
	_ = fs.WalkDir(host.FS, root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil || len(volumes) >= maxVolumes {
			return fs.SkipDir
		}
		if entry.Type()&fs.ModeSymlink == 0 {
			return nil
		}
		target, err := host.readLink("/" + name)
		if err != nil {
			return nil
		}
		device := path.Base(target)
		if !strings.HasPrefix(device, "zd") || strings.Contains(device, "p") {
			return nil
		}
		volumes = append(volumes, ZFSVolume{
			Name:      strings.TrimPrefix(name, "dev/zvol/"),
			Device:    device,
			SizeBytes: host.readSectors("/sys/block/" + device + "/size"),
		})
		return nil
	})
	slices.SortFunc(volumes, func(a, b ZFSVolume) int { return strings.Compare(a.Name, b.Name) })
	return volumes
}

// zpoolClasses are the headers zpool status puts before the vdevs of
// other allocation classes.
var zpoolClasses = map[string]string{"logs": "log", "cache": "cache", "spares": "spare", "special": "special", "dedup": "dedup"}

var (
	scanCount   = regexp.MustCompile(`with (\d+) errors`)
	scanEndedAt = regexp.MustCompile(` on (\w{3} \w{3} [ \d]\d \d{2}:\d{2}:\d{2} \d{4})`)
	dataErrors  = regexp.MustCompile(`^(\d+) data errors`)
)

// parseZpoolStatus reads `zpool status -L -P -p`: the scan, the error
// summary and the vdev tree of each pool, by name. Times are in loc, the
// host's zone, like zpool prints them.
func parseZpoolStatus(data []byte, loc *time.Location) map[string]ZFSPool {
	pools := map[string]ZFSPool{}
	var current *ZFSPool
	section := ""
	var config []string
	finish := func() {
		if current != nil {
			current.Vdevs = parseZpoolConfig(config)
			pools[current.Name] = *current
		}
		config = nil
	}
	for _, line := range lines(data) {
		trimmed := strings.TrimSpace(line)
		key, value, hasKey := strings.Cut(trimmed, ":")
		if hasKey && !strings.HasPrefix(line, "\t") {
			switch key {
			case "pool":
				finish()
				current = &ZFSPool{Name: strings.TrimSpace(value), Scan: ZFSScan{State: "none"}, Vdevs: []ZFSVdev{}, Volumes: []ZFSVolume{}}
				section = ""
				continue
			case "state":
				if current != nil {
					current.State = strings.TrimSpace(value)
				}
				section = ""
				continue
			case "scan":
				if current != nil {
					current.Scan = parseZpoolScan(strings.TrimSpace(value), loc)
				}
				section = "scan"
				continue
			case "config":
				section = "config"
				continue
			case "errors":
				if current != nil {
					if match := dataErrors.FindStringSubmatch(strings.TrimSpace(value)); match != nil {
						current.DataErrors, _ = strconv.Atoi(match[1])
					}
				}
				section = ""
				continue
			default:
				section = key
				continue
			}
		}
		if section == "config" && strings.HasPrefix(line, "\t") {
			config = append(config, strings.TrimPrefix(line, "\t"))
		}
	}
	finish()
	return pools
}

func parseZpoolScan(value string, loc *time.Location) ZFSScan {
	scan := ZFSScan{State: "none"}
	switch {
	case strings.HasPrefix(value, "scrub"):
		scan.Function = "scrub"
	case strings.HasPrefix(value, "resilver"):
		scan.Function = "resilver"
	default:
		return scan
	}
	switch {
	case strings.Contains(value, "in progress"):
		scan.State = "in_progress"
	case strings.Contains(value, "canceled"):
		scan.State = "canceled"
	case strings.Contains(value, "paused"):
		scan.State = "paused"
	case strings.Contains(value, "repaired") || strings.Contains(value, "resilvered"):
		scan.State = "finished"
	default:
		scan.State = "none"
	}
	if match := scanCount.FindStringSubmatch(value); match != nil {
		scan.Errors, _ = strconv.Atoi(match[1])
	}
	if scan.State == "finished" || scan.State == "canceled" {
		if match := scanEndedAt.FindStringSubmatch(value); match != nil {
			if ended, err := time.ParseInLocation("Mon Jan _2 15:04:05 2006", match[1], loc); err == nil {
				formatted := ended.UTC().Format(time.RFC3339)
				scan.EndedAt = &formatted
			}
		}
	}
	return scan
}

type zpoolLine struct {
	depth                 int
	name, state, note     string
	read, write, checksum uint64
}

// parseZpoolConfig turns the indented config table into top-level vdevs.
// The pool itself is depth 0 and class headers sit at depth 0 too; leaves
// below intermediate vdevs (replacing-1, spare-0) belong to their
// top-level vdev.
func parseZpoolConfig(config []string) []ZFSVdev {
	parsed := []zpoolLine{}
	for _, line := range config {
		fields := strings.Fields(line)
		if len(fields) == 0 || (fields[0] == "NAME" && len(fields) > 1 && fields[1] == "STATE") {
			continue
		}
		entry := zpoolLine{depth: (len(line) - len(strings.TrimLeft(line, " "))) / 2, name: fields[0]}
		if len(fields) > 1 {
			entry.state = fields[1]
		}
		if len(fields) > 4 {
			entry.read, _ = strconv.ParseUint(fields[2], 10, 64)
			entry.write, _ = strconv.ParseUint(fields[3], 10, 64)
			entry.checksum, _ = strconv.ParseUint(fields[4], 10, 64)
			entry.note = strings.Join(fields[5:], " ")
		} else if len(fields) > 2 {
			entry.note = strings.Join(fields[2:], " ")
		}
		parsed = append(parsed, entry)
	}

	vdevs := []ZFSVdev{}
	class := "data"
	for index, entry := range parsed {
		if entry.depth == 0 {
			if mapped, ok := zpoolClasses[entry.name]; ok && entry.state == "" {
				class = mapped
			}
			continue
		}
		hasChildren := index+1 < len(parsed) && parsed[index+1].depth > entry.depth
		if entry.depth == 1 {
			vdev := ZFSVdev{Name: entry.name, Type: zfsVdevType(entry.name, hasChildren), Class: class, State: entry.state, Read: entry.read, Write: entry.write, Checksum: entry.checksum, Devices: []ZFSDevice{}}
			if !hasChildren {
				vdev.Name = zfsDeviceName(entry.name)
				vdev.Devices = append(vdev.Devices, zfsDevice(entry))
			}
			vdevs = append(vdevs, vdev)
			continue
		}
		if !hasChildren && len(vdevs) > 0 {
			last := &vdevs[len(vdevs)-1]
			last.Devices = append(last.Devices, zfsDevice(entry))
		}
	}
	return vdevs
}

func zfsVdevType(name string, hasChildren bool) string {
	if !hasChildren {
		return "disk"
	}
	for _, kind := range []string{"mirror", "raidz3", "raidz2", "raidz1", "raidz", "draid"} {
		if strings.HasPrefix(name, kind) {
			return kind
		}
	}
	return "other"
}

func zfsDevice(entry zpoolLine) ZFSDevice {
	return ZFSDevice{Device: zfsDeviceName(entry.name), State: entry.state, Read: entry.read, Write: entry.write, Checksum: entry.checksum, Note: entry.note}
}

// zfsDeviceName strips /dev/ from the resolved path of `zpool status -L
// -P`: /dev/nvme0n1p3 becomes nvme0n1p3, /dev/mapper/crypt stays
// mapper/crypt.
func zfsDeviceName(name string) string {
	return strings.TrimPrefix(name, "/dev/")
}
