package collector

import (
	"context"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// StorageInfo is the payload of the storage dataset: the health of software
// RAID arrays, ZFS pools and the disks themselves, as the kernel and zpool
// report it. Like
// every dataset it only holds values that stay put while nothing changes,
// so a rebuild's progress and the md "clean"/"active" flip are left out.
type StorageInfo struct {
	Arrays []RAIDArray   `json:"arrays"`
	Pools  []ZFSPool     `json:"pools"`
	Disks  []StorageDisk `json:"disks"`
}

// RAIDArray is a Linux software RAID (md) array from /sys/block/md*/md.
type RAIDArray struct {
	Name  string `json:"name"`
	Level string `json:"level"`
	// State is the array_state, with clean, active, active-idle,
	// write-pending and read-auto, which follow writes, folded into
	// "active". Others are inactive, readonly, suspended, clear and broken.
	State     string `json:"state"`
	RAIDDisks int    `json:"raid_disks"`
	// Degraded counts the member slots without a working device; 0 for
	// levels without redundancy.
	Degraded int `json:"degraded"`
	// SyncAction is what the array is doing: idle, resync, recover (a
	// rebuild onto a new device), check, repair, reshape or frozen.
	SyncAction string       `json:"sync_action"`
	SizeBytes  uint64       `json:"size_bytes"`
	Members    []RAIDMember `json:"members"`
}

// RAIDMember is a device of an md array.
type RAIDMember struct {
	Device string `json:"device"`
	// Slot is the member's position in the array, null for spares and
	// failed devices.
	Slot *int `json:"slot"`
	// State lists the kernel's flags: in_sync, faulty, spare,
	// write_mostly, blocked, write_error, want_replacement, replacement,
	// journal.
	State []string `json:"state"`
}

// StorageDisk is a disk whose state the kernel reports: NVMe controllers
// and SCSI/SATA devices. Serial numbers are only known for NVMe; SCSI keeps
// them in binary VPD pages. Disks that disappeared from /sys/block (an NVMe
// controller marked dead) are still listed through their controller.
type StorageDisk struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	Model  string `json:"model"`
	Serial string `json:"serial"`
	// Firmware is the firmware revision of the drive.
	Firmware string `json:"firmware"`
	// State is the kernel's own word: live, dead, resetting, connecting or
	// deleting for NVMe; running, offline, transport-offline, blocked or
	// created for SCSI.
	State     string `json:"state"`
	SizeBytes uint64 `json:"size_bytes"`
}

// Normalize sorts the unordered lists for canonical encoding.
func (info *StorageInfo) Normalize() {
	slices.SortFunc(info.Arrays, func(a, b RAIDArray) int { return strings.Compare(a.Name, b.Name) })
	for index := range info.Arrays {
		slices.SortFunc(info.Arrays[index].Members, func(a, b RAIDMember) int { return strings.Compare(a.Device, b.Device) })
	}
	slices.SortFunc(info.Pools, func(a, b ZFSPool) int { return strings.Compare(a.Name, b.Name) })
	slices.SortFunc(info.Disks, func(a, b StorageDisk) int { return strings.Compare(a.Name, b.Name) })
}

// Storage collects the storage dataset.
type Storage struct {
	host *Host
}

func NewStorage(host *Host) *Storage { return &Storage{host: host} }

func (*Storage) Name() string { return "storage" }

func (collector *Storage) Collect(ctx context.Context) (any, error) {
	host := collector.host
	info := &StorageInfo{
		Arrays: host.readRAIDArrays(),
		Pools:  host.readZFSPools(ctx),
		Disks:  append(host.readNVMeDisks(), host.readSCSIDisks()...),
	}
	info.Normalize()
	return info, nil
}

func (host *Host) readRAIDArrays() []RAIDArray {
	arrays := []RAIDArray{}
	entries, err := host.readDir("/sys/block")
	if err != nil {
		return arrays
	}
	for _, entry := range entries {
		name := entry.Name()
		base := "/sys/block/" + name + "/md"
		if !strings.HasPrefix(name, "md") || !host.isDir(base) {
			continue
		}
		// An IMSM or DDF container ("external:imsm") only groups the disks
		// of the volumes inside it, which are arrays of their own.
		if metadata := host.readOptional(base + "/metadata_version"); strings.HasPrefix(metadata, "external:") && !strings.Contains(metadata, "/") {
			continue
		}
		array := RAIDArray{
			Name:       name,
			Level:      host.readOptional(base + "/level"),
			State:      raidState(host.readOptional(base + "/array_state")),
			RAIDDisks:  readInt(host.readOptional(base + "/raid_disks")),
			Degraded:   readInt(host.readOptional(base + "/degraded")),
			SyncAction: host.readOptional(base + "/sync_action"),
			SizeBytes:  host.readSectors("/sys/block/" + name + "/size"),
			Members:    []RAIDMember{},
		}
		if children, err := host.readDir(base); err == nil {
			for _, child := range children {
				device, ok := strings.CutPrefix(child.Name(), "dev-")
				if !ok {
					continue
				}
				member := RAIDMember{Device: device, State: []string{}}
				if slot, err := strconv.Atoi(host.readOptional(base + "/" + child.Name() + "/slot")); err == nil {
					member.Slot = &slot
				}
				for _, flag := range strings.Split(host.readOptional(base+"/"+child.Name()+"/state"), ",") {
					if flag = strings.TrimSpace(flag); flag != "" {
						member.State = append(member.State, flag)
					}
				}
				slices.Sort(member.State)
				array.Members = append(array.Members, member)
			}
		}
		arrays = append(arrays, array)
	}
	return arrays
}

// raidState folds the array states that follow write activity into one, so
// the payload does not change whenever the array is written to.
func raidState(state string) string {
	switch state {
	case "clean", "active", "active-idle", "write-pending", "read-auto":
		return "active"
	default:
		return state
	}
}

func readInt(value string) int {
	number, err := strconv.Atoi(value)
	if err != nil {
		return 0
	}
	return number
}

// nvmeNamespace matches the namespaces of a controller, including the
// hidden per-path ones of native multipathing (nvme0c0n1).
var nvmeNamespace = regexp.MustCompile(`^nvme\d+(c\d+)?n\d+$`)

var nvmePath = regexp.MustCompile(`c\d+n`)

func (host *Host) readNVMeDisks() []StorageDisk {
	disks := []StorageDisk{}
	entries, err := host.readDir("/sys/class/nvme")
	if err != nil {
		return disks
	}
	for _, entry := range entries {
		controller := entry.Name()
		base := "/sys/class/nvme/" + controller
		state := host.readOptional(base + "/state")
		if state == "" {
			continue
		}
		disk := StorageDisk{
			Type:     "nvme",
			Model:    host.readOptional(base + "/model"),
			Serial:   host.readOptional(base + "/serial"),
			Firmware: host.readOptional(base + "/firmware_rev"),
			State:    state,
		}
		namespaces := []string{}
		if children, err := host.readDir(base); err == nil {
			for _, child := range children {
				if nvmeNamespace.MatchString(child.Name()) {
					namespaces = append(namespaces, nvmePath.ReplaceAllString(child.Name(), "n"))
				}
			}
		}
		slices.Sort(namespaces)
		namespaces = slices.Compact(namespaces)
		if len(namespaces) == 0 {
			// A controller that lost its namespaces is still a disk.
			disk.Name = controller
			disks = append(disks, disk)
			continue
		}
		for _, namespace := range namespaces {
			disk.Name = namespace
			disk.SizeBytes = host.readSectors("/sys/block/" + namespace + "/size")
			disks = append(disks, disk)
		}
	}
	return disks
}

func (host *Host) readSCSIDisks() []StorageDisk {
	disks := []StorageDisk{}
	entries, err := host.readDir("/sys/block")
	if err != nil {
		return disks
	}
	for _, entry := range entries {
		name := entry.Name()
		base := "/sys/block/" + name
		if !strings.HasPrefix(name, "sd") || host.readOptional(base+"/removable") == "1" {
			continue
		}
		state := host.readOptional(base + "/device/state")
		if state == "" {
			continue
		}
		disks = append(disks, StorageDisk{
			Name:      name,
			Type:      "scsi",
			Model:     host.readOptional(base + "/device/model"),
			Firmware:  host.readOptional(base + "/device/rev"),
			State:     state,
			SizeBytes: host.readSectors(base + "/size"),
		})
	}
	return disks
}
