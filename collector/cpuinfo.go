package collector

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// CPUInfo summarises the processors.
type CPUInfo struct {
	Vendor string `json:"vendor"`
	Model  string `json:"model"`
	// Sockets, Cores and Threads count physical packages, physical cores and
	// logical CPUs. Without topology information Cores equals Threads.
	Sockets int `json:"sockets"`
	Cores   int `json:"cores"`
	Threads int `json:"threads"`
	// Flags are the x86 "flags" or ARM "Features", sorted.
	Flags []string `json:"flags"`
}

type cpuEntry struct {
	processor  int
	physicalID string
	coreID     string
}

// armImplementers maps "CPU implementer" codes to vendors.
var armImplementers = map[string]string{
	"0x41": "ARM",
	"0x42": "Broadcom",
	"0x43": "Cavium",
	"0x46": "Fujitsu",
	"0x48": "HiSilicon",
	"0x4e": "NVIDIA",
	"0x50": "APM",
	"0x51": "Qualcomm",
	"0x61": "Apple",
	"0x6d": "Microsoft",
	"0xc0": "Ampere",
}

// armParts maps implementer/part pairs to core names for the CPUs most
// likely to run servers.
var armParts = map[string]string{
	"0x41/0xd03": "Cortex-A53",
	"0x41/0xd04": "Cortex-A35",
	"0x41/0xd05": "Cortex-A55",
	"0x41/0xd07": "Cortex-A57",
	"0x41/0xd08": "Cortex-A72",
	"0x41/0xd09": "Cortex-A73",
	"0x41/0xd0b": "Cortex-A76",
	"0x41/0xd0c": "Neoverse-N1",
	"0x41/0xd40": "Neoverse-V1",
	"0x41/0xd49": "Neoverse-N2",
	"0x41/0xd4f": "Neoverse-V2",
	"0x41/0xd8e": "Neoverse-N3",
	"0x41/0xd84": "Neoverse-V3",
	"0x43/0x0af": "ThunderX2",
	"0x46/0x001": "A64FX",
	"0x48/0xd01": "Kunpeng-920",
	"0x50/0x000": "X-Gene",
	"0xc0/0xac3": "AmpereOne",
	"0xc0/0xac4": "AmpereOne",
}

// parseCPUInfo understands both the x86 layout (one block per logical CPU
// with "model name", "physical id" and "core id") and the ARM layout
// ("CPU implementer", "CPU part" and "Features", with an optional
// "Hardware"/"model name" line).
func parseCPUInfo(data []byte) CPUInfo {
	var (
		info        CPUInfo
		entries     []cpuEntry
		current     *cpuEntry
		implementer string
		part        string
		flags       []string
		armModel    string
	)
	for _, line := range lines(data) {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		switch key {
		case "processor":
			number, err := strconv.Atoi(value)
			if err != nil {
				// Old ARM kernels: "Processor : AArch64 Processor rev 4 (aarch64)".
				if armModel == "" {
					armModel = value
				}
				continue
			}
			entries = append(entries, cpuEntry{processor: number})
			current = &entries[len(entries)-1]
		case "Processor":
			if armModel == "" {
				armModel = value
			}
		case "vendor_id":
			if info.Vendor == "" {
				info.Vendor = value
			}
		case "model name":
			if info.Model == "" {
				info.Model = strings.Join(strings.Fields(value), " ")
			}
		case "Hardware":
			if armModel == "" {
				armModel = value
			}
		case "physical id":
			if current != nil {
				current.physicalID = value
			}
		case "core id":
			if current != nil {
				current.coreID = value
			}
		case "CPU implementer":
			if implementer == "" {
				implementer = strings.ToLower(value)
			}
		case "CPU part":
			if part == "" {
				part = strings.ToLower(value)
			}
		case "flags", "Features":
			if flags == nil {
				flags = strings.Fields(value)
			}
		}
	}

	if implementer != "" {
		if info.Vendor == "" {
			info.Vendor = armImplementers[implementer]
			if info.Vendor == "" {
				info.Vendor = "implementer " + implementer
			}
		}
		if info.Model == "" {
			if name, ok := armParts[implementer+"/"+part]; ok {
				info.Model = name
			} else if armModel != "" {
				info.Model = armModel
			} else if part != "" {
				info.Model = fmt.Sprintf("%s part %s", info.Vendor, part)
			}
		}
	}
	if info.Model == "" {
		info.Model = armModel
	}

	info.Threads = len(entries)
	sockets := make(map[string]bool)
	cores := make(map[string]bool)
	for _, entry := range entries {
		if entry.physicalID != "" {
			sockets[entry.physicalID] = true
		}
		if entry.coreID != "" {
			cores[entry.physicalID+"/"+entry.coreID] = true
		}
	}
	info.Sockets = max(len(sockets), min(info.Threads, 1))
	info.Cores = len(cores)
	if info.Cores == 0 {
		info.Cores = info.Threads
	}

	slices.Sort(flags)
	info.Flags = slices.Compact(flags)
	if info.Flags == nil {
		info.Flags = []string{}
	}
	return info
}

func (host *Host) readCPUInfo() CPUInfo {
	data, err := host.readFile("/proc/cpuinfo")
	if err != nil {
		return CPUInfo{Flags: []string{}}
	}
	info := parseCPUInfo(data)
	host.applyCPUTopology(&info)
	return info
}

// applyCPUTopology prefers sysfs topology, which ARM kernels populate even
// though their cpuinfo has no "physical id" or "core id".
func (host *Host) applyCPUTopology(info *CPUInfo) {
	entries, err := host.readDir("/sys/devices/system/cpu")
	if err != nil {
		return
	}
	packages := make(map[string]bool)
	cores := make(map[string]bool)
	threads := 0
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "cpu") {
			continue
		}
		if _, err := strconv.Atoi(name[3:]); err != nil {
			continue
		}
		base := "/sys/devices/system/cpu/" + name + "/topology/"
		packageID, err := host.readString(base + "physical_package_id")
		if err != nil {
			continue
		}
		coreID, err := host.readString(base + "core_id")
		if err != nil {
			continue
		}
		// Some ARM firmware reports -1 for an unknown package.
		packages[packageID] = true
		cores[packageID+"/"+coreID] = true
		threads++
	}
	if threads == 0 || threads != info.Threads {
		// Offline CPUs or hot-plug make the views disagree; keep cpuinfo.
		return
	}
	info.Sockets = len(packages)
	info.Cores = len(cores)
}
