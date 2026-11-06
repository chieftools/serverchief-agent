package collector

import (
	"context"
	"reflect"
	"testing"
	"testing/fstest"
)

func sysfsFile(value string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(value + "\n")} }

// degradedStorageHost is a Hetzner box with four NVMe disks in RAID 1 and
// RAID 10 arrays, after nvme0 died: its controller is dead, its namespace
// reports no size and its partitions are faulty members.
func degradedStorageHost() *Host {
	files := fstest.MapFS{
		"sys/block/md2/size":                   sysfsFile("3865014272"),
		"sys/block/md2/md/level":               sysfsFile("raid10"),
		"sys/block/md2/md/array_state":         sysfsFile("clean"),
		"sys/block/md2/md/raid_disks":          sysfsFile("4"),
		"sys/block/md2/md/degraded":            sysfsFile("1"),
		"sys/block/md2/md/sync_action":         sysfsFile("idle"),
		"sys/block/md2/md/metadata_version":    sysfsFile("1.2"),
		"sys/block/md2/md/dev-nvme0n1p4/state": sysfsFile("faulty,write_error"),
		"sys/block/md2/md/dev-nvme0n1p4/slot":  sysfsFile("none"),
		"sys/block/md2/md/dev-nvme1n1p4/state": sysfsFile("in_sync"),
		"sys/block/md2/md/dev-nvme1n1p4/slot":  sysfsFile("1"),
		"sys/block/md0/size":                   sysfsFile("64976896"),
		"sys/block/md0/md/level":               sysfsFile("raid1"),
		"sys/block/md0/md/array_state":         sysfsFile("active-idle"),
		"sys/block/md0/md/raid_disks":          sysfsFile("2"),
		"sys/block/md0/md/degraded":            sysfsFile("0"),
		"sys/block/md0/md/sync_action":         sysfsFile("check"),
		"sys/block/md0/md/dev-nvme1n1p2/state": sysfsFile("in_sync"),
		"sys/block/md0/md/dev-nvme1n1p2/slot":  sysfsFile("0"),
		"sys/block/md127/md/metadata_version":  sysfsFile("external:imsm"),
		"sys/block/md127/md/array_state":       sysfsFile("inactive"),
		"sys/block/nvme0n1/size":               sysfsFile("0"),
		"sys/block/nvme1n1/size":               sysfsFile("2000409264"),
		"sys/block/sda/size":                   sysfsFile("7814037168"),
		"sys/block/sda/removable":              sysfsFile("0"),
		"sys/block/sda/device/state":           sysfsFile("offline"),
		"sys/block/sda/device/model":           sysfsFile("ST4000NM0035-1V4"),
		"sys/block/sdb/removable":              sysfsFile("1"),
		"sys/block/sdb/device/state":           sysfsFile("running"),
		"sys/block/vda/size":                   sysfsFile("41943040"),
		"sys/class/nvme/nvme0/state":           sysfsFile("dead"),
		"sys/class/nvme/nvme0/model":           sysfsFile("SAMSUNG MZVL21T0HCLR-00B00              "),
		"sys/class/nvme/nvme0/serial":          sysfsFile("S676NL0W376092      "),
		"sys/class/nvme/nvme0/nvme0n1/size":    sysfsFile("0"),
		"sys/class/nvme/nvme1/state":           sysfsFile("live"),
		"sys/class/nvme/nvme1/model":           sysfsFile("SAMSUNG MZVL21T0HCLR-00B00"),
		"sys/class/nvme/nvme1/serial":          sysfsFile("S676NX0W218643"),
		"sys/class/nvme/nvme1/nvme1c1n1/size":  sysfsFile("2000409264"),
		"sys/class/nvme/nvme2/state":           sysfsFile("dead"),
		"sys/class/nvme/nvme2/model":           sysfsFile("SAMSUNG MZVL21T0HCLR-00B00"),
		"sys/class/nvme/nvme2/serial":          sysfsFile("S676NU0W639503"),
	}
	return &Host{FS: files}
}

func TestStorageCollect(t *testing.T) {
	payload, err := NewStorage(degradedStorageHost()).Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	info := payload.(*StorageInfo)
	zero, one := 0, 1
	want := &StorageInfo{
		Arrays: []RAIDArray{
			{
				Name: "md0", Level: "raid1", State: "active", RAIDDisks: 2, Degraded: 0, SyncAction: "check", SizeBytes: 64976896 * 512,
				Members: []RAIDMember{{Device: "nvme1n1p2", Slot: &zero, State: []string{"in_sync"}}},
			},
			{
				Name: "md2", Level: "raid10", State: "active", RAIDDisks: 4, Degraded: 1, SyncAction: "idle", SizeBytes: 3865014272 * 512,
				Members: []RAIDMember{
					{Device: "nvme0n1p4", Slot: nil, State: []string{"faulty", "write_error"}},
					{Device: "nvme1n1p4", Slot: &one, State: []string{"in_sync"}},
				},
			},
		},
		Disks: []StorageDisk{
			{Name: "nvme0n1", Type: "nvme", Model: "SAMSUNG MZVL21T0HCLR-00B00", Serial: "S676NL0W376092", State: "dead", SizeBytes: 0},
			{Name: "nvme1n1", Type: "nvme", Model: "SAMSUNG MZVL21T0HCLR-00B00", Serial: "S676NX0W218643", State: "live", SizeBytes: 2000409264 * 512},
			{Name: "nvme2", Type: "nvme", Model: "SAMSUNG MZVL21T0HCLR-00B00", Serial: "S676NU0W639503", State: "dead"},
			{Name: "sda", Type: "scsi", Model: "ST4000NM0035-1V4", State: "offline", SizeBytes: 7814037168 * 512},
		},
	}
	if !reflect.DeepEqual(info, want) {
		t.Errorf("Collect() =\n%+v\nwant\n%+v", info, want)
	}
}

func TestStorageCollectWithoutRAIDOrDisks(t *testing.T) {
	payload, err := NewStorage(&Host{FS: fstest.MapFS{"sys/block/vda/size": sysfsFile("41943040")}}).Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	info := payload.(*StorageInfo)
	if info.Arrays == nil || info.Disks == nil || len(info.Arrays) != 0 || len(info.Disks) != 0 {
		t.Errorf("Collect() = %#v, want empty lists", info)
	}
}

func TestRAIDStateFoldsWriteActivity(t *testing.T) {
	for state, want := range map[string]string{
		"clean": "active", "active": "active", "active-idle": "active", "write-pending": "active", "read-auto": "active",
		"inactive": "inactive", "readonly": "readonly", "broken": "broken", "": "",
	} {
		if got := raidState(state); got != want {
			t.Errorf("raidState(%q) = %q, want %q", state, got, want)
		}
	}
}
