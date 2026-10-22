package collector

import (
	"context"
	"errors"
	"os"
	"reflect"
	"slices"
	"testing"
	"testing/fstest"
)

func TestParseCPUInfoX86(t *testing.T) {
	data, err := os.ReadFile("testdata/x86-kvm/proc/cpuinfo")
	if err != nil {
		t.Fatal(err)
	}
	info := parseCPUInfo(data)
	if info.Vendor != "GenuineIntel" || info.Model != "Intel(R) Xeon(R) Gold 6230 CPU @ 2.10GHz" {
		t.Errorf("vendor/model = %q / %q", info.Vendor, info.Model)
	}
	if info.Sockets != 2 || info.Cores != 4 || info.Threads != 8 {
		t.Errorf("topology = %d sockets, %d cores, %d threads; want 2/4/8", info.Sockets, info.Cores, info.Threads)
	}
	if !slices.Contains(info.Flags, "hypervisor") || !slices.Contains(info.Flags, "avx512f") || !slices.IsSorted(info.Flags) {
		t.Errorf("flags = %v", info.Flags)
	}
}

func TestParseCPUInfoARM64(t *testing.T) {
	host := fixtureHost(t, "arm64-neoverse")
	info := host.readCPUInfo()
	want := CPUInfo{Vendor: "ARM", Model: "Neoverse-N1", Sockets: 1, Cores: 4, Threads: 4}
	if info.Vendor != want.Vendor || info.Model != want.Model || info.Sockets != want.Sockets || info.Cores != want.Cores || info.Threads != want.Threads {
		t.Errorf("readCPUInfo() = %#v, want %#v", info, want)
	}
	if !slices.Contains(info.Flags, "asimd") || !slices.Contains(info.Flags, "atomics") {
		t.Errorf("features = %v", info.Flags)
	}
}

func TestParseCPUInfoAppleVirtualization(t *testing.T) {
	info := fixtureHost(t, "arm64-apple").readCPUInfo()
	if info.Vendor != "Apple" || info.Model != "Apple part 0x000" || info.Threads != 2 || info.Cores != 2 || info.Sockets != 1 {
		t.Errorf("readCPUInfo() = %#v", info)
	}
}

func TestParseCPUInfoLegacyARM(t *testing.T) {
	info := fixtureHost(t, "arm-legacy").readCPUInfo()
	if info.Vendor != "Qualcomm" || info.Model != "AArch64 Processor rev 4 (aarch64)" || info.Threads != 2 {
		t.Errorf("readCPUInfo() = %#v", info)
	}
}

func TestParseCPUInfoEmpty(t *testing.T) {
	info := parseCPUInfo(nil)
	if info.Threads != 0 || info.Sockets != 0 || info.Flags == nil {
		t.Errorf("parseCPUInfo(nil) = %#v", info)
	}
}

func TestParseMeminfo(t *testing.T) {
	data, err := os.ReadFile("testdata/x86-kvm/proc/meminfo")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := parseMeminfo(data), (MemoryInfo{TotalBytes: 8148292 * 1024, SwapTotalBytes: 2097148 * 1024}); got != want {
		t.Fatalf("parseMeminfo() = %#v, want %#v", got, want)
	}
}

func TestHardwareCollect(t *testing.T) {
	host := fixtureHost(t, "x86-kvm")
	statfsCalls := []string{}
	host.Statfs = func(path string) (FSStats, error) {
		statfsCalls = append(statfsCalls, path)
		if path == "/boot/efi" {
			return FSStats{}, errors.New("permission denied")
		}
		return FSStats{SizeBytes: 42 << 30, TotalInodes: 1000}, nil
	}
	payload, err := NewHardware(host).Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	info := payload.(*HardwareInfo)

	names := []string{}
	for _, device := range info.BlockDevices {
		names = append(names, device.Name)
	}
	if !reflect.DeepEqual(names, []string{"nvme0n1", "sda", "vda"}) {
		t.Fatalf("block devices = %v (loop0 and empty sr0 must be skipped)", names)
	}
	sda := info.BlockDevices[1]
	wantSDA := BlockDevice{
		Name: "sda", Type: "scsi", SizeBytes: 83886080 * 512, Rotational: true,
		Vendor: "QEMU", Model: "QEMU HARDDISK",
		Partitions: []Partition{{Name: "sda1", SizeBytes: 83666911 * 512}, {Name: "sda15", SizeBytes: 217088 * 512}},
	}
	if !reflect.DeepEqual(sda, wantSDA) {
		t.Errorf("sda = %#v\nwant %#v", sda, wantSDA)
	}
	if nvme := info.BlockDevices[0]; nvme.Type != "nvme" || nvme.Rotational || len(nvme.Partitions) != 0 || nvme.Partitions == nil {
		t.Errorf("nvme0n1 = %#v", nvme)
	}

	wantFilesystems := []Filesystem{
		{Device: "/dev/sda1", MountPoint: "/", Type: "ext4", SizeBytes: 42 << 30, TotalInodes: 1000},
		{Device: "/dev/sda15", MountPoint: "/boot/efi", Type: "vfat"},
		{Device: "/dev/vda", MountPoint: "/mnt/My Backups", Type: "xfs", ReadOnly: true, SizeBytes: 42 << 30, TotalInodes: 1000},
		{Device: "nas.example.net:/export/backup", MountPoint: "/mnt/nfs", Type: "nfs4", Network: true},
		{Device: "/dev/vda", MountPoint: "/var/lib/mysql", Type: "xfs", SizeBytes: 42 << 30, TotalInodes: 1000},
	}
	if !reflect.DeepEqual(info.Filesystems, wantFilesystems) {
		t.Errorf("filesystems =\n%#v\nwant\n%#v", info.Filesystems, wantFilesystems)
	}
	if slices.Contains(statfsCalls, "/mnt/nfs") {
		t.Errorf("statfs called on a network file system: %v", statfsCalls)
	}
	if info.CPU.Threads != 8 || info.Memory.TotalBytes == 0 {
		t.Errorf("cpu/memory = %#v / %#v", info.CPU, info.Memory)
	}
}

func TestParseMountsOverlayAndShadowing(t *testing.T) {
	data := []byte("overlay / overlay rw,relatime 0 0\n" +
		"/dev/sda1 /data ext4 rw 0 0\n" +
		"/dev/sdb1 /data xfs rw 0 0\n" +
		"/dev/sda1 /var/lib/docker/overlay2 ext4 rw 0 0\n" +
		"shm /dev/shm tmpfs rw 0 0\n" +
		"fuse.lxcfs /proc/cpuinfo fuse.lxcfs rw 0 0\n" +
		"short line\n")
	got := parseMounts(data)
	want := []Filesystem{
		{Device: "overlay", MountPoint: "/", Type: "overlay"},
		{Device: "/dev/sdb1", MountPoint: "/data", Type: "xfs"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseMounts() = %#v\nwant %#v", got, want)
	}
}

func TestReadFilesystemsSkipsFileBindMounts(t *testing.T) {
	host := &Host{FS: fstest.MapFS{
		"proc/mounts": &fstest.MapFile{Data: []byte("/dev/vdb1 /etc/hosts btrfs rw 0 0\n/dev/vdb1 /srv btrfs rw 0 0\n")},
		"etc/hosts":   &fstest.MapFile{Data: []byte("127.0.0.1 localhost\n")},
	}}
	got := host.readFilesystems()
	if len(got) != 1 || got[0].MountPoint != "/srv" {
		t.Fatalf("readFilesystems() = %#v", got)
	}
}

func TestBlockVendor(t *testing.T) {
	for input, want := range map[string]string{"0x1af4": "", "0x0000": "", "ATA": "ATA", "0xZZ": "0xZZ", "": ""} {
		if got := blockVendor(input); got != want {
			t.Errorf("blockVendor(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestUnescapeMount(t *testing.T) {
	for input, want := range map[string]string{
		`/mnt/My\040Disk`: "/mnt/My Disk",
		`/a\011b\134c`:    "/a\tb\\c",
		`/trailing\04`:    `/trailing\04`,
		"/plain":          "/plain",
	} {
		if got := unescapeMount(input); got != want {
			t.Errorf("unescapeMount(%q) = %q, want %q", input, got, want)
		}
	}
}
