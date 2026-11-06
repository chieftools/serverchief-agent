package collector

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"
	"testing/fstest"
	"time"

	"go.chief.tools/serverchief-agent/helper"
)

func readSmartFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/smart/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func pointer[T any](value T) *T { return &value }

func TestParseSmartctl(t *testing.T) {
	tests := map[string]struct {
		want *DiskSMART
		ran  bool
	}{
		"nvme-healthy.json": {ran: true, want: &DiskSMART{
			Passed: pointer(true), CriticalWarning: pointer[int64](0), AvailableSpare: pointer[int64](100), AvailableSpareThreshold: pointer[int64](10),
			PercentageUsed: pointer[int64](11), MediaErrors: pointer[int64](0), ErrorLogEntries: pointer[int64](0),
		}},
		"nvme-failing.json": {ran: true, want: &DiskSMART{
			Passed: pointer(false), CriticalWarning: pointer[int64](5), AvailableSpare: pointer[int64](4), AvailableSpareThreshold: pointer[int64](10),
			PercentageUsed: pointer[int64](103), MediaErrors: pointer[int64](17), ErrorLogEntries: pointer[int64](240), SelfTestPassed: pointer(false),
		}},
		"sata-failing.json": {ran: true, want: &DiskSMART{
			Passed: pointer(true), ReallocatedSectors: pointer[int64](8), PendingSectors: pointer[int64](2), OfflineUncorrectable: pointer[int64](0),
			ReportedUncorrectable: pointer[int64](1), CRCErrors: pointer[int64](3), ErrorLogEntries: pointer[int64](12), SelfTestPassed: pointer(false),
		}},
		"nvme-smartctl-7.4.json": {ran: true, want: &DiskSMART{
			Passed: pointer(true), CriticalWarning: pointer[int64](0), AvailableSpare: pointer[int64](100), AvailableSpareThreshold: pointer[int64](10),
			PercentageUsed: pointer[int64](36), MediaErrors: pointer[int64](0), ErrorLogEntries: pointer[int64](0),
		}},
		"nvme-dead.json":    {ran: true, want: nil},
		"sata-standby.json": {ran: true, want: nil},
	}
	for name, test := range tests {
		got, ran := parseSmartctl(readSmartFixture(t, name))
		if ran != test.ran || !reflect.DeepEqual(got, test.want) {
			t.Errorf("parseSmartctl(%s) = %+v, %v; want %+v, %v", name, got, ran, test.want, test.ran)
		}
	}
	if smart, ran := parseSmartctl([]byte("not json")); smart != nil || ran {
		t.Errorf("parseSmartctl(garbage) = %+v, %v", smart, ran)
	}
}

// smartHost is bare metal with smartctl, an NVMe disk, a SATA disk, a
// sleeping SATA disk, a dead NVMe disk and an emulated QEMU disk.
func smartHost(t *testing.T, ran *[]string) *Host {
	return &Host{
		FS: fstest.MapFS{
			"usr/sbin/smartctl":                 &fstest.MapFile{Mode: 0o755},
			"sys/class/nvme/nvme0/state":        sysfsFile("live"),
			"sys/class/nvme/nvme0/nvme0n1/size": sysfsFile("1"),
			"sys/class/nvme/nvme1/state":        sysfsFile("dead"),
			"sys/block/sda/device/state":        sysfsFile("running"),
			"sys/block/sda/device/vendor":       sysfsFile("ATA"),
			"sys/block/sda/device/model":        sysfsFile("ST4000NM0035-1V4"),
			"sys/block/sdb/device/state":        sysfsFile("running"),
			"sys/block/sdb/device/vendor":       sysfsFile("ATA"),
			"sys/block/sdc/device/state":        sysfsFile("running"),
			"sys/block/sdc/device/vendor":       sysfsFile("QEMU"),
			"sys/block/sdc/device/model":        sysfsFile("QEMU HARDDISK"),
		},
		Command: func(_ context.Context, name string, args ...string) (CommandResult, error) {
			device := args[len(args)-1]
			*ran = append(*ran, device)
			fixture := map[string]string{"/dev/nvme0": "nvme-healthy.json", "/dev/sda": "sata-failing.json", "/dev/sdb": "sata-standby.json"}[device]
			return CommandResult{Stdout: readSmartFixture(t, fixture), ExitCode: 4}, nil
		},
	}
}

func TestFetchDiskHealthReadsThePhysicalDisks(t *testing.T) {
	ran := []string{}
	health := smartHost(t, &ran).FetchDiskHealth(context.Background())

	if !reflect.DeepEqual(ran, []string{"/dev/nvme0", "/dev/sda", "/dev/sdb"}) {
		t.Errorf("read %v; the dead controller and the emulated disk must be skipped", ran)
	}
	if health.Status != SMARTRead || len(health.Disks) != 2 || health.Disks["nvme0n1"] == nil || *health.Disks["sda"].PendingSectors != 2 {
		t.Errorf("health = %+v", health)
	}
}

func TestFetchDiskHealthSkipsVirtualMachinesAndHostsWithoutSmartctl(t *testing.T) {
	ran := []string{}
	host := smartHost(t, &ran)
	host.FS.(fstest.MapFS)["sys/class/dmi/id/sys_vendor"] = sysfsFile("QEMU")
	if health := host.FetchDiskHealth(context.Background()); health.Status != SMARTVirtual || len(ran) != 0 {
		t.Errorf("virtual machine: %+v, ran %v", health, ran)
	}

	host = smartHost(t, &ran)
	delete(host.FS.(fstest.MapFS), "usr/sbin/smartctl")
	if health := host.FetchDiskHealth(context.Background()); health.Status != SMARTNotInstalled || len(ran) != 0 {
		t.Errorf("without smartctl: %+v, ran %v", health, ran)
	}

	if health := (&Host{FS: fstest.MapFS{"usr/sbin/smartctl": &fstest.MapFile{Mode: 0o755}}}).FetchDiskHealth(context.Background()); health.Status != SMARTNoDisks {
		t.Errorf("without disks: %+v", health)
	}
}

type fakeDiskHealthRunner struct {
	calls  int
	health DiskHealth
	err    error
}

func (runner *fakeDiskHealthRunner) Do(_ context.Context, action string, _ map[string]string) (helper.Result, error) {
	runner.calls++
	if action != helper.ActionDiskHealth {
		return helper.Result{}, errors.New("unexpected action " + action)
	}
	if runner.err != nil {
		return helper.Result{}, runner.err
	}
	data, _ := json.Marshal(runner.health)
	return helper.Result{Data: data}, nil
}

func TestSmartCacheReadsHourlyAndKeepsSleepingDisks(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	runner := &fakeDiskHealthRunner{health: DiskHealth{Status: SMARTRead, Disks: map[string]*DiskSMART{"sda": {PendingSectors: pointer[int64](0)}, "sdb": {PendingSectors: pointer[int64](1)}}}}
	host := &Host{FS: fstest.MapFS{}, Privileged: runner, Now: func() time.Time { return now }}
	cache := &smartCache{}

	cache.get(context.Background(), host)
	now = now.Add(30 * time.Minute)
	cache.get(context.Background(), host)
	if runner.calls != 1 {
		t.Fatalf("read %d times within the hour", runner.calls)
	}

	// sdb is asleep an hour later: it keeps its last data
	now = now.Add(time.Hour)
	runner.health = DiskHealth{Status: SMARTRead, Disks: map[string]*DiskSMART{"sda": {PendingSectors: pointer[int64](3)}}}
	health := cache.get(context.Background(), host)
	if runner.calls != 2 || *health.Disks["sda"].PendingSectors != 3 || health.Disks["sdb"] == nil || *health.Disks["sdb"].PendingSectors != 1 {
		t.Errorf("health = %+v after %d reads", health, runner.calls)
	}

	// A helper that stops answering keeps the previous read
	now = now.Add(2 * time.Hour)
	runner.err = errors.New("helper did not answer")
	if health := cache.get(context.Background(), host); health.Status != SMARTRead || health.Disks["sda"] == nil {
		t.Errorf("health after a helper failure = %+v", health)
	}
	if health := (&smartCache{}).get(context.Background(), host); health.Status != SMARTUnavailable {
		t.Errorf("first read without a helper = %+v", health)
	}
}
