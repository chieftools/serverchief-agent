package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"go.chief.tools/serverchief-agent/helper"
)

// SMART statuses of the storage dataset.
const (
	// SMARTRead means smartctl ran; disks it could not read (asleep,
	// failed) have no smart data.
	SMARTRead = "read"
	// SMARTNotInstalled means smartctl (smartmontools) is missing.
	SMARTNotInstalled = "not_installed"
	// SMARTVirtual means the host is a virtual machine or container,
	// whose disks report no SMART data.
	SMARTVirtual = "virtual"
	// SMARTNoDisks means the host has no disk SMART applies to.
	SMARTNoDisks = "none"
	// SMARTUnavailable means smartctl could not run, such as without the
	// root helper.
	SMARTUnavailable = "unavailable"
)

// SMARTStatus tells whether the disks' SMART data could be read.
type SMARTStatus struct {
	Status string `json:"status"`
}

// DiskSMART holds the facts a disk reports about its own health that stay
// put while it is healthy: no temperature, power-on hours or amounts
// written. A field is null when the disk does not report it.
type DiskSMART struct {
	// Passed is the disk's overall SMART verdict.
	Passed *bool `json:"passed"`
	// CriticalWarning is the NVMe critical warning bit field: spare below
	// threshold (1), temperature (2), reliability degraded (4), read-only
	// (8), volatile memory backup failed (16), persistent memory region
	// read-only (32).
	CriticalWarning *int64 `json:"critical_warning"`
	// AvailableSpare and AvailableSpareThreshold are NVMe percentages.
	AvailableSpare          *int64 `json:"available_spare"`
	AvailableSpareThreshold *int64 `json:"available_spare_threshold"`
	// PercentageUsed is the NVMe estimate of the rated endurance used,
	// above 100 once exceeded.
	PercentageUsed *int64 `json:"percentage_used"`
	// MediaErrors counts unrecovered NVMe data integrity errors.
	MediaErrors *int64 `json:"media_errors"`
	// ErrorLogEntries counts the entries of the disk's error log.
	ErrorLogEntries *int64 `json:"error_log_entries"`
	// ReallocatedSectors is ATA attribute 5, or the SCSI grown defects.
	ReallocatedSectors *int64 `json:"reallocated_sectors"`
	// PendingSectors is ATA attribute 197: unreadable sectors waiting to be
	// remapped.
	PendingSectors *int64 `json:"pending_sectors"`
	// OfflineUncorrectable is ATA attribute 198.
	OfflineUncorrectable *int64 `json:"offline_uncorrectable"`
	// ReportedUncorrectable is ATA attribute 187, or the uncorrected read
	// and write errors of SCSI.
	ReportedUncorrectable *int64 `json:"reported_uncorrectable"`
	// CRCErrors is ATA attribute 199, usually a cable or backplane issue.
	CRCErrors *int64 `json:"crc_errors"`
	// SelfTestPassed is the result of the latest completed self-test the
	// disk logged; the agent never starts one.
	SelfTestPassed *bool `json:"self_test_passed"`
}

// DiskHealth is what the root helper returns for helper.ActionDiskHealth:
// the SMART data of each disk it could read, by disk name.
type DiskHealth struct {
	Status string                `json:"status"`
	Disks  map[string]*DiskSMART `json:"disks"`
}

const (
	// smartInterval is how often SMART data is read; the storage dataset
	// reuses it in between.
	smartInterval = time.Hour
	// smartDiskTimeout bounds smartctl for one disk.
	smartDiskTimeout = 20 * time.Second
	// maxSMARTDisks caps the disks read in one round.
	maxSMARTDisks = 64
)

// virtualDiskModels are the vendors and models of emulated disks, which
// report no SMART data of their own.
var virtualDiskModels = []string{"qemu", "vmware", "vbox", "virtual disk", "msft", "virtio", "xen"}

type smartTarget struct {
	names  []string
	device string
}

// FetchDiskHealth reads the SMART data of the physical disks with
// smartctl. It needs root, so the daemon calls it through the root helper.
// Virtual machines and containers are skipped: their disks have no SMART
// data.
func (host *Host) FetchDiskHealth(ctx context.Context) DiskHealth {
	if host.detectVirtualization(host.readCPUInfo().Flags).Role != "none" {
		return DiskHealth{Status: SMARTVirtual}
	}
	smartctl := host.findCommand("smartctl")
	if smartctl == "" {
		return DiskHealth{Status: SMARTNotInstalled}
	}
	targets := host.smartTargets()
	if len(targets) == 0 {
		return DiskHealth{Status: SMARTNoDisks}
	}
	health := DiskHealth{Status: SMARTUnavailable, Disks: map[string]*DiskSMART{}}
	for _, target := range targets {
		diskContext, cancel := context.WithTimeout(ctx, smartDiskTimeout)
		result, err := host.run(diskContext, smartctl, "-j", "-H", "-A", "-l", "error", "-l", "selftest", "-n", "standby", target.device)
		cancel()
		if err != nil {
			continue
		}
		smart, ran := parseSmartctl(result.Stdout)
		if ran {
			health.Status = SMARTRead
		}
		if smart != nil {
			for _, name := range target.names {
				health.Disks[name] = smart
			}
		}
	}
	return health
}

// smartTargets lists the disks smartctl reads: NVMe controllers the kernel
// still talks to and non-removable SCSI/SATA disks that are not emulated.
func (host *Host) smartTargets() []smartTarget {
	targets := []smartTarget{}
	for _, disk := range host.readNVMeDisks() {
		if slices.Contains([]string{"dead", "deleting", "deleting (no IO)"}, disk.State) {
			continue
		}
		controller := nvmeController(disk.Name)
		index := slices.IndexFunc(targets, func(target smartTarget) bool { return target.device == "/dev/"+controller })
		if index >= 0 {
			targets[index].names = append(targets[index].names, disk.Name)
			continue
		}
		targets = append(targets, smartTarget{names: []string{disk.Name}, device: "/dev/" + controller})
	}
	for _, disk := range host.readSCSIDisks() {
		vendor := strings.ToLower(host.readOptional("/sys/block/"+disk.Name+"/device/vendor") + " " + disk.Model)
		if disk.State != "running" || slices.ContainsFunc(virtualDiskModels, func(model string) bool { return strings.Contains(vendor, model) }) {
			continue
		}
		targets = append(targets, smartTarget{names: []string{disk.Name}, device: "/dev/" + disk.Name})
	}
	if len(targets) > maxSMARTDisks {
		targets = targets[:maxSMARTDisks]
	}
	return targets
}

// nvmeController is the controller of a namespace: nvme0 for nvme0n1.
func nvmeController(name string) string {
	if index := strings.LastIndex(name, "n"); index > len("nvme") {
		return name[:index]
	}
	return name
}

// smartctlOutput is the part of `smartctl --json` the agent reads.
type smartctlOutput struct {
	Smartctl struct {
		ExitStatus int `json:"exit_status"`
	} `json:"smartctl"`
	PowerMode   string `json:"power_mode"`
	SmartStatus *struct {
		Passed *bool `json:"passed"`
	} `json:"smart_status"`
	NVMe *struct {
		CriticalWarning         *int64 `json:"critical_warning"`
		AvailableSpare          *int64 `json:"available_spare"`
		AvailableSpareThreshold *int64 `json:"available_spare_threshold"`
		PercentageUsed          *int64 `json:"percentage_used"`
		MediaErrors             *int64 `json:"media_errors"`
		ErrorLogEntries         *int64 `json:"num_err_log_entries"`
	} `json:"nvme_smart_health_information_log"`
	NVMeSelfTests *struct {
		Table []struct {
			Result struct {
				Value int64 `json:"value"`
			} `json:"self_test_result"`
		} `json:"table"`
	} `json:"nvme_self_test_log"`
	ATAAttributes *struct {
		Table []struct {
			ID  int `json:"id"`
			Raw struct {
				Value int64 `json:"value"`
			} `json:"raw"`
		} `json:"table"`
	} `json:"ata_smart_attributes"`
	ATAErrors *struct {
		Summary struct {
			Count *int64 `json:"count"`
		} `json:"summary"`
	} `json:"ata_smart_error_log"`
	ATASelfTests *struct {
		Standard struct {
			Table []struct {
				Status struct {
					Value  int64 `json:"value"`
					Passed *bool `json:"passed"`
				} `json:"status"`
			} `json:"table"`
		} `json:"standard"`
	} `json:"ata_smart_self_test_log"`
	SCSIGrownDefects *int64 `json:"scsi_grown_defect_list"`
	SCSIErrors       *struct {
		Read struct {
			Uncorrected *int64 `json:"total_uncorrected_errors"`
		} `json:"read"`
		Write struct {
			Uncorrected *int64 `json:"total_uncorrected_errors"`
		} `json:"write"`
	} `json:"scsi_error_counter_log"`
}

// smartctl exit status bits: the command line was wrong (1), the device
// could not be opened or is asleep with -n standby (2).
const (
	smartctlCommandLineError = 1 << 0
	smartctlOpenFailed       = 1 << 1
)

// parseSmartctl reads one disk's `smartctl --json` output. ran is false
// when smartctl itself failed; the data is nil when the disk could not be
// read, such as one asleep or gone.
func parseSmartctl(data []byte) (smart *DiskSMART, ran bool) {
	var output smartctlOutput
	if err := json.Unmarshal(data, &output); err != nil {
		return nil, false
	}
	if output.Smartctl.ExitStatus&smartctlCommandLineError != 0 {
		return nil, false
	}
	if output.Smartctl.ExitStatus&smartctlOpenFailed != 0 || strings.EqualFold(output.PowerMode, "standby") {
		return nil, true
	}
	smart = &DiskSMART{}
	if output.SmartStatus != nil {
		smart.Passed = output.SmartStatus.Passed
	}
	if nvme := output.NVMe; nvme != nil {
		smart.CriticalWarning = nvme.CriticalWarning
		smart.AvailableSpare = nvme.AvailableSpare
		smart.AvailableSpareThreshold = nvme.AvailableSpareThreshold
		smart.PercentageUsed = nvme.PercentageUsed
		smart.MediaErrors = nvme.MediaErrors
		smart.ErrorLogEntries = nvme.ErrorLogEntries
	}
	if tests := output.NVMeSelfTests; tests != nil {
		// 0 passed, 5 to 7 failed; the others are aborted tests (1 to 4,
		// 8, 9) or unused entries (15), which say nothing about the disk.
		for _, entry := range tests.Table {
			if value := entry.Result.Value; value == 0 || value >= 5 && value <= 7 {
				passed := value == 0
				smart.SelfTestPassed = &passed
				break
			}
		}
	}
	if attributes := output.ATAAttributes; attributes != nil {
		for _, attribute := range attributes.Table {
			// Vendors pack more into the upper bytes of some raw values.
			value := attribute.Raw.Value & 0xFFFFFFFF
			switch attribute.ID {
			case 5:
				smart.ReallocatedSectors = &value
			case 187:
				smart.ReportedUncorrectable = &value
			case 197:
				smart.PendingSectors = &value
			case 198:
				smart.OfflineUncorrectable = &value
			case 199:
				smart.CRCErrors = &value
			}
		}
	}
	if errorLog := output.ATAErrors; errorLog != nil {
		smart.ErrorLogEntries = errorLog.Summary.Count
	}
	if tests := output.ATASelfTests; tests != nil {
		// Status 15 (in progress) and 1 to 2 (aborted, interrupted) say
		// nothing about the disk.
		for _, entry := range tests.Standard.Table {
			if value := entry.Status.Value >> 4; value == 0 || value > 2 && value < 15 {
				passed := value == 0
				smart.SelfTestPassed = &passed
				break
			}
		}
	}
	if output.SCSIGrownDefects != nil {
		smart.ReallocatedSectors = output.SCSIGrownDefects
	}
	if scsiErrors := output.SCSIErrors; scsiErrors != nil && (scsiErrors.Read.Uncorrected != nil || scsiErrors.Write.Uncorrected != nil) {
		total := int64(0)
		for _, count := range []*int64{scsiErrors.Read.Uncorrected, scsiErrors.Write.Uncorrected} {
			if count != nil {
				total += *count
			}
		}
		smart.ReportedUncorrectable = &total
	}
	return smart, true
}

// diskHealth reads the SMART data through the root helper when the daemon
// runs unprivileged, directly otherwise.
func (host *Host) diskHealth(ctx context.Context) (DiskHealth, error) {
	if host.Privileged == nil {
		return host.FetchDiskHealth(ctx), nil
	}
	result, err := host.Privileged.Do(ctx, helper.ActionDiskHealth, nil)
	if err != nil {
		return DiskHealth{}, err
	}
	var health DiskHealth
	if err := json.Unmarshal(result.Data, &health); err != nil {
		return DiskHealth{}, fmt.Errorf("read the disk health from the helper: %w", err)
	}
	return health, nil
}

// smartCache keeps the last SMART read for smartInterval, and each disk's
// last data while it sleeps, so a disk in standby keeps its values instead
// of flapping between data and none.
type smartCache struct {
	mutex   sync.Mutex
	readAt  time.Time
	health  DiskHealth
	lastFor map[string]*DiskSMART
}

func (cache *smartCache) get(ctx context.Context, host *Host) DiskHealth {
	cache.mutex.Lock()
	defer cache.mutex.Unlock()
	now := host.now()
	if !cache.readAt.IsZero() && now.Sub(cache.readAt) < smartInterval {
		return cache.health
	}
	health, err := host.diskHealth(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || cache.readAt.IsZero() {
			return DiskHealth{Status: SMARTUnavailable}
		}
		return cache.health
	}
	if cache.lastFor == nil {
		cache.lastFor = map[string]*DiskSMART{}
	}
	if health.Status == SMARTRead {
		for name, smart := range health.Disks {
			cache.lastFor[name] = smart
		}
		health.Disks = maps.Clone(cache.lastFor)
	}
	cache.readAt, cache.health = now, health
	return health
}
