package collector

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// Synthetic /proc and /sys contents in the kernel's formats.
func usageFS() fstest.MapFS {
	file := func(data string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(data)} }
	return fstest.MapFS{
		"proc/stat": file(`cpu  1000 50 300 8000 120 10 20 5 0 0
cpu0 500 25 150 4000 60 5 10 3 0 0
cpu1 500 25 150 4000 60 5 10 2 0 0
intr 12345
ctxt 67890
btime 1792000000
processes 4242
procs_running 3
procs_blocked 1
`),
		"proc/loadavg": file("0.52 0.61 0.58 2/213 4242\n"),
		"proc/meminfo": file(`MemTotal:        8388608 kB
MemFree:         1048576 kB
MemAvailable:    5242880 kB
Buffers:          131072 kB
Cached:          1835008 kB
SwapCached:            0 kB
SReclaimable:     131072 kB
SwapTotal:       2097152 kB
SwapFree:        2097152 kB
`),
		"proc/mounts": file(`/dev/vda1 / ext4 rw,relatime 0 0
proc /proc proc rw 0 0
tmpfs /run tmpfs rw 0 0
/dev/vda1 /srv/www ext4 rw,relatime 0 0
/dev/vdb1 /var/lib/mysql xfs rw 0 0
fileserver.example.invalid:/export /mnt/share nfs4 rw 0 0
`),
		"srv/www":       &fstest.MapFile{Mode: 0o755 | 1<<31},
		"var/lib/mysql": &fstest.MapFile{Mode: 0o755 | 1<<31},
		"mnt/share":     &fstest.MapFile{Mode: 0o755 | 1<<31},
		"proc/diskstats": file(`   7       0 loop0 10 0 80 0 0 0 0 0 0 4 0 0 0 0 0
 252       0 vda 2000 100 40000 900 5000 300 120000 4000 0 3000 4900 0 0 0 0
 252       1 vda1 1990 100 39900 890 5000 300 120000 4000 0 2990 4890 0 0 0 0
 253       0 dm-0 50 0 400 10 60 0 480 20 0 30 30 0 0 0 0
  11       0 sr0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0
`),
		"sys/block/vda/size":         file("167772160\n"),
		"sys/block/loop0/size":       file("0\n"),
		"sys/block/sr0/size":         file("0\n"),
		"sys/block/dm-0/size":        file("2097152\n"),
		"sys/block/dm-0/dm/name":     file("vg-data\n"),
		"sys/block/dm-0/slaves/vda1": file(""),
		"proc/net/dev": file(`Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo:  123456     100    0    0    0     0          0         0   123456     100    0    0    0     0       0          0
  eth0: 9000000   12000    1    2    0     0          0         0  4000000    9000    3    4    0     0       0          0
  eth1: 7000000   10000    0    0    0     0          0         0  3000000    8000    0    0    0     0       0          0
 bond0: 7000000   10000    0    0    0     0          0         0  3000000    8000    0    0    0     0       0          0
eth0.100: 1000     10    0    0    0     0          0         0     1000      10    0    0    0     0       0          0
veth1a2b: 500      5    0    0    0     0          0         0      500       5    0    0    0     0       0          0
docker0:  700      7    0    0    0     0          0         0      700       7    0    0    0     0       0          0
`),
		"sys/class/net/eth0/device":         file(""),
		"sys/class/net/eth1/device":         file(""),
		"sys/class/net/eth1/master":         file(""),
		"sys/class/net/bond0/lower_eth1":    file(""),
		"sys/class/net/eth0.100/lower_eth0": file(""),
		"proc/net/snmp": file(`Ip: Forwarding DefaultTTL
Ip: 1 64
Tcp: RtoAlgorithm RtoMin RtoMax MaxConn ActiveOpens PassiveOpens AttemptFails EstabResets CurrEstab InSegs
Tcp: 1 200 120000 -1 100 200 3 4 42 99999
Udp: InDatagrams NoPorts
Udp: 10 2
`),
		"proc/net/sockstat": file(`sockets: used 300
TCP: inuse 50 orphan 0 tw 17 alloc 60 mem 3
UDP: inuse 4 mem 1
`),
		"proc/uptime": file("864000.25 3400000.00\n"),
	}
}

func usageHost() *Host {
	return &Host{
		FS: usageFS(),
		Statfs: func(path string) (FSStats, error) {
			switch path {
			case "/":
				return FSStats{SizeBytes: 80 << 30, FreeBytes: 50 << 30, AvailableBytes: 46 << 30, Files: 5000000, FilesFree: 4640000}, nil
			case "/var/lib/mysql":
				return FSStats{SizeBytes: 200 << 30, FreeBytes: 150 << 30, AvailableBytes: 150 << 30}, nil
			}
			return FSStats{}, fmt.Errorf("unexpected statfs of %s", path)
		},
		Now: func() time.Time { return time.Unix(1793001600, 0) },
	}
}

func TestReadUsage(t *testing.T) {
	usage := usageHost().ReadUsage()

	if usage.CPU != (CPUTimes{User: 1000, Nice: 50, System: 300, Idle: 8000, IOWait: 120, IRQ: 10, SoftIRQ: 20, Steal: 5}) || usage.CPU.Total() != 9505 {
		t.Errorf("cpu = %+v", usage.CPU)
	}
	if usage.Cores != 2 || usage.ProcsRunning != 3 || usage.ProcsBlocked != 1 {
		t.Errorf("cores %d, running %d, blocked %d", usage.Cores, usage.ProcsRunning, usage.ProcsBlocked)
	}
	if usage.Load != [3]float64{0.52, 0.61, 0.58} || usage.Threads != 213 {
		t.Errorf("load %v threads %d", usage.Load, usage.Threads)
	}
	if want := (MemoryUsage{Total: 8 << 30, Available: 5 << 30, Cached: 2 << 30, SwapTotal: 2 << 30, SwapFree: 2 << 30}); usage.Memory != want {
		t.Errorf("memory = %+v, want %+v", usage.Memory, want)
	}

	mounts := []string{}
	for _, filesystem := range usage.Filesystems {
		mounts = append(mounts, filesystem.MountPoint)
	}
	if !reflect.DeepEqual(mounts, []string{"/", "/var/lib/mysql"}) {
		t.Errorf("file systems = %v (pseudo, bind-mounted and network file systems must be skipped)", mounts)
	}

	wantDisks := []DiskCounters{
		{Name: "vda", Reads: 2000, ReadSectors: 40000, Writes: 5000, WriteSectors: 120000, IOMillis: 3000, Underlying: true},
		// LVM on vda1: its I/O shows on vda too
		{Name: "vg-data", Reads: 50, ReadSectors: 400, Writes: 60, WriteSectors: 480, IOMillis: 30},
	}
	if !reflect.DeepEqual(usage.Disks, wantDisks) {
		t.Errorf("disks = %+v (loop devices, empty optical drives and partitions must be skipped)", usage.Disks)
	}

	underlying := map[string]bool{}
	for _, counters := range usage.Interfaces {
		underlying[counters.Name] = counters.Underlying
	}
	// The bond and the VLAN carry traffic of the NICs below them
	if !reflect.DeepEqual(underlying, map[string]bool{"bond0": false, "eth0": true, "eth0.100": false, "eth1": true}) {
		t.Errorf("interfaces = %v (lo, veth and docker must be skipped)", underlying)
	}
	if eth0 := usage.Interfaces[1]; eth0 != (InterfaceCounters{Name: "eth0", RxBytes: 9000000, TxBytes: 4000000, RxPackets: 12000, TxPackets: 9000, Errors: 4, Drops: 6, Underlying: true}) {
		t.Errorf("eth0 = %+v", eth0)
	}

	if !usage.HasTCP || usage.TCPEstablished != 42 || usage.TCPTimeWait != 17 || usage.Uptime != 864000.25 {
		t.Errorf("tcp %v %d %d uptime %v", usage.HasTCP, usage.TCPEstablished, usage.TCPTimeWait, usage.Uptime)
	}
	if !usage.At.Equal(time.Unix(1793001600, 0)) {
		t.Errorf("at = %v", usage.At)
	}
}

func TestReadUsageCountsInterfacesNotStackedOnOthersInAContainer(t *testing.T) {
	host := usageHost()
	fs := host.FS.(fstest.MapFS)
	for _, name := range []string{"sys/class/net/eth0/device", "sys/class/net/eth1/device"} {
		delete(fs, name)
	}

	underlying := map[string]bool{}
	for _, counters := range host.ReadUsage().Interfaces {
		underlying[counters.Name] = counters.Underlying
	}
	if !reflect.DeepEqual(underlying, map[string]bool{"bond0": false, "eth0": true, "eth0.100": false, "eth1": true}) {
		t.Fatalf("interfaces = %v", underlying)
	}
}

func TestReadUsageEstimatesAvailableMemoryOnOldKernels(t *testing.T) {
	host := usageHost()
	fs := host.FS.(fstest.MapFS)
	fs["proc/meminfo"] = &fstest.MapFile{Data: []byte("MemTotal: 4096 kB\nMemFree: 1024 kB\nBuffers: 512 kB\nCached: 1024 kB\n")}

	if got := host.ReadUsage().Memory.Available; got != 2560*1024 {
		t.Fatalf("available = %d, want free + buffers + cached", got)
	}
}

func TestReadUsageCapsInstances(t *testing.T) {
	var netdev strings.Builder
	netdev.WriteString("header\nheader\n")
	for index := range MaximumInstances + 10 {
		fmt.Fprintf(&netdev, "eth%03d: 1 1 0 0 0 0 0 0 1 1 0 0 0 0 0 0\n", index)
	}
	host := usageHost()
	host.FS.(fstest.MapFS)["proc/net/dev"] = &fstest.MapFile{Data: []byte(netdev.String())}

	if got := len(host.ReadUsage().Interfaces); got != MaximumInstances {
		t.Fatalf("%d interfaces, want the cap of %d", got, MaximumInstances)
	}
}

func TestReadUsageWithoutSources(t *testing.T) {
	usage := (&Host{FS: fstest.MapFS{}}).ReadUsage()
	if usage.Cores != 0 || usage.HasTCP || usage.Filesystems != nil || usage.Disks != nil || usage.Interfaces != nil {
		t.Fatalf("usage = %+v", usage)
	}
}
