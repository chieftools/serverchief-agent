package collector

import (
	"context"
	"io/fs"
	"os"
	"reflect"
	"testing"
	"testing/fstest"
	"time"
)

func readZpoolFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/zfs/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestParseZpoolStatusHealthyMirrors(t *testing.T) {
	pools := parseZpoolStatus(readZpoolFixture(t, "healthy.txt"), time.UTC)
	endedAt := "2026-10-11T01:41:19Z"
	want := ZFSPool{
		Name:  "rpool",
		State: "ONLINE",
		Scan:  ZFSScan{Function: "scrub", State: "finished", EndedAt: &endedAt},
		Vdevs: []ZFSVdev{
			{Name: "mirror-0", Type: "mirror", Class: "data", State: "ONLINE", Devices: []ZFSDevice{{Device: "nvme0n1p3", State: "ONLINE"}, {Device: "nvme2n1p3", State: "ONLINE"}}},
			{Name: "mirror-1", Type: "mirror", Class: "data", State: "ONLINE", Devices: []ZFSDevice{{Device: "nvme1n1p3", State: "ONLINE"}, {Device: "nvme3n1p3", State: "ONLINE"}}},
		},
		Volumes: []ZFSVolume{},
	}
	if len(pools) != 2 || !reflect.DeepEqual(pools["rpool"], want) {
		t.Errorf("rpool = %+v\nwant %+v", pools["rpool"], want)
	}
}

func TestParseZpoolStatusDegradedPool(t *testing.T) {
	pools := parseZpoolStatus(readZpoolFixture(t, "degraded.txt"), time.UTC)
	tank := pools["tank"]
	if tank.State != "DEGRADED" || tank.DataErrors != 2 || tank.Scan != (ZFSScan{Function: "resilver", State: "in_progress"}) {
		t.Fatalf("tank = %+v", tank)
	}
	want := []ZFSVdev{
		{Name: "mirror-0", Type: "mirror", Class: "data", State: "DEGRADED", Devices: []ZFSDevice{
			{Device: "sda1", State: "ONLINE"},
			{Device: "1234567890123456789", State: "UNAVAIL", Note: "was /dev/sdb1", Parent: "replacing-1"},
			{Device: "sdc1", State: "ONLINE", Note: "(resilvering)", Parent: "replacing-1"},
		}},
		{Name: "raidz1-1", Type: "raidz1", Class: "data", State: "ONLINE", Devices: []ZFSDevice{
			{Device: "sdd", State: "ONLINE", Checksum: 12},
			{Device: "sde", State: "ONLINE", Read: 3},
			{Device: "sdf", State: "ONLINE"},
		}},
		{Name: "nvme0n1p1", Type: "disk", Class: "log", State: "ONLINE", Devices: []ZFSDevice{{Device: "nvme0n1p1", State: "ONLINE"}}},
		{Name: "nvme0n1p2", Type: "disk", Class: "cache", State: "ONLINE", Devices: []ZFSDevice{{Device: "nvme0n1p2", State: "ONLINE"}}},
		{Name: "sdg", Type: "disk", Class: "spare", State: "AVAIL", Devices: []ZFSDevice{{Device: "sdg", State: "AVAIL"}}},
	}
	if !reflect.DeepEqual(tank.Vdevs, want) {
		t.Errorf("tank vdevs =\n%+v\nwant\n%+v", tank.Vdevs, want)
	}
	backup := pools["backup"]
	if backup.Scan.State != "none" || len(backup.Vdevs) != 1 || backup.Vdevs[0].Type != "disk" || backup.Vdevs[0].Name != "sdh" {
		t.Errorf("backup = %+v", backup)
	}
}

func TestParseZpoolScan(t *testing.T) {
	amsterdam, _ := time.LoadLocation("Europe/Amsterdam")
	ended := func(value string) *string { return &value }
	for line, want := range map[string]ZFSScan{
		"none requested": {State: "none"},
		"scrub repaired 0B in 01:17:18 with 0 errors on Sun Oct 11 01:41:19 2026": {Function: "scrub", State: "finished", EndedAt: ended("2026-10-10T23:41:19Z")},
		"resilvered 300G in 02:00:00 with 4 errors on Fri Nov  6 12:00:00 2026":   {Function: "resilver", State: "finished", Errors: 4, EndedAt: ended("2026-11-06T11:00:00Z")},
		"scrub canceled on Mon Nov  2 09:00:00 2026":                              {Function: "scrub", State: "canceled", EndedAt: ended("2026-11-02T08:00:00Z")},
		"scrub in progress since Fri Nov  6 10:00:00 2026":                        {Function: "scrub", State: "in_progress"},
		"scrub paused since Fri Nov  6 10:00:00 2026":                             {Function: "scrub", State: "paused"},
	} {
		if got := parseZpoolScan(line, amsterdam); !reflect.DeepEqual(got, want) {
			t.Errorf("parseZpoolScan(%q) = %+v, want %+v", line, got, want)
		}
	}
}

func TestReadZFSPoolsJoinsKstatZpoolAndVolumes(t *testing.T) {
	host := &Host{
		FS: fstest.MapFS{
			"proc/spl/kstat/zfs/rpool/state":          sysfsFile("DEGRADED"),
			"proc/spl/kstat/zfs/rpool2/state":         sysfsFile("ONLINE"),
			"proc/spl/kstat/zfs/arcstats":             sysfsFile("x"),
			"usr/sbin/zpool":                          &fstest.MapFile{Mode: 0o755},
			"dev/zvol/rpool/data/vm-100-disk-0":       &fstest.MapFile{Data: []byte("../../../zd96"), Mode: fs.ModeSymlink},
			"dev/zvol/rpool/data/vm-100-disk-0-part1": &fstest.MapFile{Data: []byte("../../../zd96p1"), Mode: fs.ModeSymlink},
			"sys/block/zd96/size":                     sysfsFile("67108864"),
		},
		Command: func(_ context.Context, name string, args ...string) (CommandResult, error) {
			if name != "/usr/sbin/zpool" || !reflect.DeepEqual(args, []string{"status", "-L", "-P", "-p"}) {
				t.Errorf("ran %s %v", name, args)
			}
			return CommandResult{Stdout: readZpoolFixture(t, "healthy.txt")}, nil
		},
		Now: func() time.Time { return time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC) },
	}
	pools := host.readZFSPools(context.Background())
	if len(pools) != 2 || pools[0].Name != "rpool" || pools[1].Name != "rpool2" {
		t.Fatalf("pools = %+v", pools)
	}
	if pools[0].State != "DEGRADED" || len(pools[0].Vdevs) != 2 {
		t.Errorf("rpool keeps the kstat state and the zpool layout: %+v", pools[0])
	}
	if want := []ZFSVolume{{Name: "rpool/data/vm-100-disk-0", Device: "zd96", SizeBytes: 67108864 * 512}}; !reflect.DeepEqual(pools[0].Volumes, want) {
		t.Errorf("volumes = %+v, want %+v", pools[0].Volumes, want)
	}
}

func TestReadZFSPoolsWithoutZpoolKeepsTheState(t *testing.T) {
	host := &Host{FS: fstest.MapFS{"proc/spl/kstat/zfs/tank/state": sysfsFile("FAULTED")}}
	pools := host.readZFSPools(context.Background())
	if len(pools) != 1 || pools[0].State != "FAULTED" || pools[0].Scan.State != "none" || pools[0].Vdevs == nil || pools[0].Volumes == nil {
		t.Errorf("pools = %+v", pools)
	}
}

func TestReadZFSPoolsWithoutZFS(t *testing.T) {
	host := &Host{FS: fstest.MapFS{}, Command: func(context.Context, string, ...string) (CommandResult, error) {
		t.Error("zpool must not run without ZFS")
		return CommandResult{}, nil
	}}
	if pools := host.readZFSPools(context.Background()); pools == nil || len(pools) != 0 {
		t.Errorf("pools = %#v", pools)
	}
}
