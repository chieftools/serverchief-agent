package metrics

import (
	"maps"
	"math"
	"slices"

	"go.chief.tools/serverchief-agent/collector"
)

const sectorBytes = 512

// Derive turns a reading into points (the protocol's metric catalog). Rates
// and CPU percentages need the previous reading; they are left out without
// one, when no time passed, and for a group whose counters went backwards
// (a reboot, a driver reload or a recreated interface).
func Derive(previous *collector.Usage, current collector.Usage) []Point {
	var points []Point
	add := func(metric, instance string, value float64) {
		points = append(points, Point{Metric: metric, Instance: instance, Value: value})
	}

	elapsed := 0.0
	if previous != nil {
		elapsed = current.At.Sub(previous.At).Seconds()
	}
	rates := elapsed > 0

	if rates {
		before, after := previous.CPU, current.CPU
		total := float64(after.Total()) - float64(before.Total())
		if total > 0 && grew(cpuCounters(before), cpuCounters(after)) {
			share := func(from, to uint64) float64 { return percent(float64(to-from), total) }
			add("cpu.user", "", share(before.User+before.Nice, after.User+after.Nice))
			add("cpu.system", "", share(before.System+before.IRQ+before.SoftIRQ, after.System+after.IRQ+after.SoftIRQ))
			add("cpu.iowait", "", share(before.IOWait, after.IOWait))
			add("cpu.steal", "", share(before.Steal, after.Steal))
			add("cpu.idle", "", share(before.Idle, after.Idle))
		}
	}
	if current.Cores > 0 {
		add("cpu.cores", "", float64(current.Cores))
		add("load.1", "", current.Load[0])
		add("load.5", "", current.Load[1])
		add("load.15", "", current.Load[2])
		add("procs.running", "", float64(current.ProcsRunning))
		add("procs.blocked", "", float64(current.ProcsBlocked))
	}
	if current.Threads > 0 {
		add("procs.total", "", float64(current.Threads))
	}

	if memory := current.Memory; memory.Total > 0 {
		used := memory.Total - min(memory.Available, memory.Total)
		add("memory.total", "", float64(memory.Total))
		add("memory.available", "", float64(memory.Available))
		add("memory.used", "", float64(used))
		add("memory.cached", "", float64(memory.Cached))
		add("memory.used_pct", "", percent(float64(used), float64(memory.Total)))
		swapUsed := memory.SwapTotal - min(memory.SwapFree, memory.SwapTotal)
		add("swap.total", "", float64(memory.SwapTotal))
		add("swap.used", "", float64(swapUsed))
		if memory.SwapTotal > 0 {
			add("swap.used_pct", "", percent(float64(swapUsed), float64(memory.SwapTotal)))
		}
	}

	for _, filesystem := range current.Filesystems {
		used := filesystem.SizeBytes - min(filesystem.FreeBytes, filesystem.SizeBytes)
		add("fs.size", filesystem.MountPoint, float64(filesystem.SizeBytes))
		add("fs.used", filesystem.MountPoint, float64(used))
		// df: used against what users can fill, so the root reserve counts as full.
		if writable := float64(used) + float64(filesystem.AvailableBytes); writable > 0 {
			add("fs.used_pct", filesystem.MountPoint, percent(float64(used), writable))
		}
		if filesystem.Files > 0 {
			inodes := filesystem.Files - min(filesystem.FilesFree, filesystem.Files)
			add("fs.inodes_used_pct", filesystem.MountPoint, percent(float64(inodes), float64(filesystem.Files)))
		}
	}

	if rates {
		// Host totals (instance "") add up the underlying devices only, so a
		// disk below LVM or RAID, or a NIC below a bond, bridge or VLAN, is
		// not counted twice; utilisation is that of the busiest disk.
		diskTotals := map[string]float64{}
		netTotals := map[string]float64{}
		total := func(totals map[string]float64, metric string, value float64) {
			totals[metric] += value
		}

		disks := make(map[string]collector.DiskCounters, len(previous.Disks))
		for _, disk := range previous.Disks {
			disks[disk.Name] = disk
		}
		for _, disk := range current.Disks {
			before, ok := disks[disk.Name]
			if !ok || !grew(diskCounters(before), diskCounters(disk)) {
				continue
			}
			values := []measurement{
				{"disk.read_bps", rate(float64((disk.ReadSectors-before.ReadSectors)*sectorBytes), elapsed)},
				{"disk.write_bps", rate(float64((disk.WriteSectors-before.WriteSectors)*sectorBytes), elapsed)},
				{"disk.read_iops", rate(float64(disk.Reads-before.Reads), elapsed)},
				{"disk.write_iops", rate(float64(disk.Writes-before.Writes), elapsed)},
			}
			utilization := math.Min(100, percent(float64(disk.IOMillis-before.IOMillis), elapsed*1000))
			for _, value := range values {
				add(value.metric, disk.Name, value.value)
				if disk.Underlying {
					total(diskTotals, value.metric, value.value)
				}
			}
			add("disk.util_pct", disk.Name, utilization)
			if disk.Underlying {
				diskTotals["disk.util_pct"] = math.Max(diskTotals["disk.util_pct"], utilization)
			}
		}

		interfaces := make(map[string]collector.InterfaceCounters, len(previous.Interfaces))
		for _, counters := range previous.Interfaces {
			interfaces[counters.Name] = counters
		}
		for _, counters := range current.Interfaces {
			before, ok := interfaces[counters.Name]
			if !ok || !grew(interfaceCounters(before), interfaceCounters(counters)) {
				continue
			}
			values := []measurement{
				{"net.rx_bps", rate(float64(counters.RxBytes-before.RxBytes), elapsed)},
				{"net.tx_bps", rate(float64(counters.TxBytes-before.TxBytes), elapsed)},
				{"net.rx_pps", rate(float64(counters.RxPackets-before.RxPackets), elapsed)},
				{"net.tx_pps", rate(float64(counters.TxPackets-before.TxPackets), elapsed)},
				{"net.errors", rate(float64(counters.Errors-before.Errors), elapsed)},
				{"net.drops", rate(float64(counters.Drops-before.Drops), elapsed)},
			}
			for _, value := range values {
				add(value.metric, counters.Name, value.value)
				if counters.Underlying {
					total(netTotals, value.metric, value.value)
				}
			}
		}

		for _, totals := range []map[string]float64{diskTotals, netTotals} {
			for _, metric := range slices.Sorted(maps.Keys(totals)) {
				add(metric, "", round(totals[metric]))
			}
		}
	}

	if current.HasTCP {
		add("tcp.established", "", float64(current.TCPEstablished))
		add("tcp.time_wait", "", float64(current.TCPTimeWait))
	}
	if current.Uptime > 0 {
		add("system.uptime", "", math.Floor(current.Uptime))
	}

	return points
}

type measurement struct {
	metric string
	value  float64
}

// grew reports whether no counter of a group went backwards.
func grew(before, after []uint64) bool {
	for index, value := range before {
		if after[index] < value {
			return false
		}
	}
	return true
}

func cpuCounters(times collector.CPUTimes) []uint64 {
	return []uint64{times.User, times.Nice, times.System, times.Idle, times.IOWait, times.IRQ, times.SoftIRQ, times.Steal}
}

func diskCounters(disk collector.DiskCounters) []uint64 {
	return []uint64{disk.Reads, disk.Writes, disk.ReadSectors, disk.WriteSectors, disk.IOMillis}
}

func interfaceCounters(counters collector.InterfaceCounters) []uint64 {
	return []uint64{counters.RxBytes, counters.TxBytes, counters.RxPackets, counters.TxPackets, counters.Errors, counters.Drops}
}

// percent rounds to three decimals; values are about whole percentages and
// shorter numbers compress better.
func percent(part, whole float64) float64 {
	if whole <= 0 {
		return 0
	}
	return round(part / whole * 100)
}

func rate(delta, seconds float64) float64 {
	return round(delta / seconds)
}

func round(value float64) float64 {
	return math.Round(value*1000) / 1000
}
