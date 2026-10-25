package metrics

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"go.chief.tools/serverchief-agent/collector"
)

func reading(at time.Time) collector.Usage {
	return collector.Usage{
		At:           at,
		CPU:          collector.CPUTimes{User: 1000, Nice: 0, System: 200, Idle: 8000, IOWait: 100, IRQ: 0, SoftIRQ: 0, Steal: 0},
		Cores:        4,
		ProcsRunning: 2,
		Load:         [3]float64{0.5, 0.6, 0.7},
		Threads:      200,
		Memory:       collector.MemoryUsage{Total: 8 << 30, Available: 6 << 30, Cached: 1 << 30, SwapTotal: 2 << 30, SwapFree: 1 << 30},
		Filesystems: []collector.FilesystemUsage{{MountPoint: "/", FSStats: collector.FSStats{
			SizeBytes: 100 << 30, FreeBytes: 60 << 30, AvailableBytes: 55 << 30, Files: 1000, FilesFree: 900,
		}}},
		Disks:          []collector.DiskCounters{{Name: "vda", Reads: 100, Writes: 200, ReadSectors: 1000, WriteSectors: 2000, IOMillis: 500}},
		Interfaces:     []collector.InterfaceCounters{{Name: "eth0", RxBytes: 1000, TxBytes: 2000, RxPackets: 10, TxPackets: 20}},
		HasTCP:         true,
		TCPEstablished: 12,
		TCPTimeWait:    3,
		Uptime:         3600.75,
	}
}

func values(points []Point) map[[2]string]float64 {
	result := make(map[[2]string]float64, len(points))
	for _, point := range points {
		result[[2]string{point.Metric, point.Instance}] = point.Value
	}
	return result
}

func TestDeriveGaugesWithoutAPreviousReading(t *testing.T) {
	got := values(Derive(nil, reading(time.Unix(1793001600, 0))))

	for key, want := range map[[2]string]float64{
		{"cpu.cores", ""}:           4,
		{"load.15", ""}:             0.7,
		{"procs.total", ""}:         200,
		{"memory.used", ""}:         2 << 30,
		{"memory.used_pct", ""}:     25,
		{"memory.cached", ""}:       1 << 30,
		{"swap.used_pct", ""}:       50,
		{"fs.used", "/"}:            40 << 30,
		{"fs.used_pct", "/"}:        42.105, // like df: 40 / (40 + 55)
		{"fs.inodes_used_pct", "/"}: 10,
		{"tcp.established", ""}:     12,
		{"tcp.time_wait", ""}:       3,
		{"system.uptime", ""}:       3600,
		{"procs.running", ""}:       2,
	} {
		if got[key] != want {
			t.Errorf("%v = %v, want %v", key, got[key], want)
		}
	}
	for _, rate := range [][2]string{{"cpu.user", ""}, {"disk.read_bps", "vda"}, {"net.rx_bps", "eth0"}} {
		if _, ok := got[rate]; ok {
			t.Errorf("%v present without a previous reading", rate)
		}
	}
}

func TestDeriveRatesOverTheInterval(t *testing.T) {
	before := reading(time.Unix(1793001600, 0))
	after := reading(time.Unix(1793001660, 0))
	after.CPU = collector.CPUTimes{User: 1100, Nice: 20, System: 220, Idle: 8840, IOWait: 110, IRQ: 5, SoftIRQ: 5, Steal: 0}
	after.Disks[0] = collector.DiskCounters{Name: "vda", Reads: 160, Writes: 320, ReadSectors: 1000 + 120*1024, WriteSectors: 2000 + 240*1024, IOMillis: 500 + 6000}
	after.Interfaces[0] = collector.InterfaceCounters{Name: "eth0", RxBytes: 1000 + 600000, TxBytes: 2000 + 120000, RxPackets: 10 + 600, TxPackets: 20 + 300, Errors: 6, Drops: 0}

	got := values(Derive(&before, after))
	for key, want := range map[[2]string]float64{
		// 1000 ticks passed: user 100 + nice 20, system 20 + irq 5 + softirq 5.
		{"cpu.user", ""}:           12,
		{"cpu.system", ""}:         3,
		{"cpu.iowait", ""}:         1,
		{"cpu.steal", ""}:          0,
		{"cpu.idle", ""}:           84,
		{"disk.read_bps", "vda"}:   1048576,
		{"disk.write_bps", "vda"}:  2097152,
		{"disk.read_iops", "vda"}:  1,
		{"disk.write_iops", "vda"}: 2,
		{"disk.util_pct", "vda"}:   10,
		{"net.rx_bps", "eth0"}:     10000,
		{"net.tx_bps", "eth0"}:     2000,
		{"net.rx_pps", "eth0"}:     10,
		{"net.tx_pps", "eth0"}:     5,
		{"net.errors", "eth0"}:     0.1,
		{"net.drops", "eth0"}:      0,
	} {
		if got[key] != want {
			t.Errorf("%v = %v, want %v", key, got[key], want)
		}
	}
}

func TestDeriveSkipsCountersThatWentBackwards(t *testing.T) {
	before := reading(time.Unix(1793001600, 0))
	after := reading(time.Unix(1793001660, 0))
	after.CPU.Idle = 10 // a reboot between readings
	after.Interfaces[0].RxBytes = 5
	after.Disks = append(after.Disks, collector.DiskCounters{Name: "vdb", Reads: 5})

	got := values(Derive(&before, after))
	for _, key := range [][2]string{{"cpu.idle", ""}, {"net.rx_bps", "eth0"}, {"disk.read_bps", "vdb"}} {
		if _, ok := got[key]; ok {
			t.Errorf("%v derived across a reset or without a previous reading", key)
		}
	}
	if got[[2]string{"disk.read_bps", "vda"}] != 0 {
		t.Error("an unchanged disk should report 0, not be left out")
	}
}

func TestDeriveCapsUtilizationAndLeavesOutUnknownSwap(t *testing.T) {
	before := reading(time.Unix(1793001600, 0))
	after := reading(time.Unix(1793001660, 0))
	after.Disks[0].IOMillis += 61000 // in-flight time overlaps the interval edges
	after.Memory.SwapTotal, after.Memory.SwapFree = 0, 0

	got := values(Derive(&before, after))
	if got[[2]string{"disk.util_pct", "vda"}] != 100 {
		t.Errorf("util = %v, want the 100%% cap", got[[2]string{"disk.util_pct", "vda"}])
	}
	if _, ok := got[[2]string{"swap.used_pct", ""}]; ok {
		t.Error("swap.used_pct without swap")
	}
}

// Every metric the agent derives is in the protocol's catalog, as shown by
// the request fixture, and encodes as the fixture's triples.
func TestDerivedMetricsMatchTheProtocolFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/protocol/samples-request.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture Batch
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	catalog := make(map[string]bool)
	for _, sample := range fixture.Samples {
		for _, point := range sample.Points {
			catalog[point.Metric] = true
		}
	}

	before := reading(time.Unix(1793001600, 0))
	after := reading(time.Unix(1793001660, 0))
	after.CPU.User += 10
	after.CPU.Idle += 90
	derived := Derive(&before, after)
	for _, point := range derived {
		if !catalog[point.Metric] {
			t.Errorf("%s is not in the fixture's catalog", point.Metric)
		}
	}
	names := make(map[string]bool)
	for _, point := range derived {
		names[point.Metric] = true
	}
	for metric := range catalog {
		if !names[metric] {
			t.Errorf("%s is in the catalog but never derived", metric)
		}
	}

	encoded, err := json.Marshal(Batch{ProtocolVersion: 1, BatchID: fixture.BatchID, Samples: fixture.Samples})
	if err != nil {
		t.Fatal(err)
	}
	var left, right any
	_ = json.Unmarshal(encoded, &left)
	_ = json.Unmarshal(data, &right)
	leftJSON, _ := json.Marshal(left)
	rightJSON, _ := json.Marshal(right)
	if string(leftJSON) != string(rightJSON) {
		t.Errorf("re-encoded fixture differs:\n%s\n%s", leftJSON, rightJSON)
	}
}
