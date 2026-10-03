package syncfs

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func plistText(value string) diskPlistValue {
	return diskPlistValue{XMLName: xml.Name{Local: "string"}, Text: value}
}

func plistBool(value bool) diskPlistValue {
	name := "false"
	if value {
		name = "true"
	}
	return diskPlistValue{XMLName: xml.Name{Local: name}}
}

func physicalStore(name string) diskPlistValue {
	return diskPlistValue{XMLName: xml.Name{Local: "dict"}, Children: []diskPlistValue{
		{XMLName: xml.Name{Local: "key"}, Text: "APFSPhysicalStore"}, plistText(name),
	}}
}

func darwinDiskFixture() (unix.Statfs_t, map[string]diskInfo) {
	var stat unix.Statfs_t
	stat.Flags = unix.MNT_LOCAL
	copy(stat.Mntfromname[:], "/dev/disk3s5")
	copy(stat.Fstypename[:], "apfs")
	devices := map[string]diskInfo{}
	for _, id := range []string{"disk3s5", "disk0s2", "disk0"} {
		devices[id] = diskInfo{
			"FilesystemType": plistText("apfs"), "DeviceIdentifier": plistText(id), "DeviceNode": plistText("/dev/" + id),
			"Internal": plistBool(true), "Removable": plistBool(false), "Ejectable": plistBool(false),
			"WholeDisk": plistBool(id == "disk0"), "ParentWholeDisk": plistText("disk0"),
			"BusProtocol": plistText("Apple Fabric"), "DeviceTreePath": plistText("IODeviceTree:/arm-io/internal-nvme"),
			"VirtualOrPhysical": plistText("Unknown"),
		}
	}
	devices["disk3s5"]["APFSPhysicalStores"] = diskPlistValue{XMLName: xml.Name{Local: "array"}, Children: []diskPlistValue{physicalStore("disk0s2")}}
	return stat, devices
}

func TestDarwinVolumePolicy(t *testing.T) {
	tests := []struct {
		name string
		edit func(*unix.Statfs_t, map[string]diskInfo)
		want bool
	}{
		{"internal Apple Silicon APFS", nil, true},
		{"APFS synthesized volume is allowed with physical stores", func(_ *unix.Statfs_t, d map[string]diskInfo) {
			d["disk3s5"]["VirtualOrPhysical"] = plistText("Virtual")
		}, true},
		{"internal Intel SATA HFS", func(s *unix.Statfs_t, d map[string]diskInfo) {
			clear(s.Fstypename[:])
			copy(s.Fstypename[:], "hfs")
			d["disk3s5"]["FilesystemType"] = plistText("hfs")
			d["disk0"]["BusProtocol"] = plistText("SATA")
			d["disk0"]["VirtualOrPhysical"] = plistText("Physical")
		}, true},
		{"internal Intel NVMe", func(_ *unix.Statfs_t, d map[string]diskInfo) { d["disk0"]["BusProtocol"] = plistText("PCI-Express") }, true},
		{"unknown local filesystem", func(s *unix.Statfs_t, _ map[string]diskInfo) {
			clear(s.Fstypename[:])
			copy(s.Fstypename[:], "macfuse")
		}, false},
		{"filesystem metadata mismatch", func(_ *unix.Statfs_t, d map[string]diskInfo) { d["disk3s5"]["FilesystemType"] = plistText("hfs") }, false},
		{"missing filesystem metadata", func(_ *unix.Statfs_t, d map[string]diskInfo) { delete(d["disk3s5"], "FilesystemType") }, false},
		{"USB fixed external SSD", func(_ *unix.Statfs_t, d map[string]diskInfo) { d["disk3s5"]["Internal"] = plistBool(false) }, false},
		{"USB removable media", func(_ *unix.Statfs_t, d map[string]diskInfo) { d["disk3s5"]["Removable"] = plistBool(true) }, false},
		{"ejectable optical media", func(_ *unix.Statfs_t, d map[string]diskInfo) { d["disk3s5"]["Ejectable"] = plistBool(true) }, false},
		{"APFS backed by external store", func(_ *unix.Statfs_t, d map[string]diskInfo) { d["disk0s2"]["Internal"] = plistBool(false) }, false},
		{"APFS backed by removable whole disk", func(_ *unix.Statfs_t, d map[string]diskInfo) { d["disk0"]["Removable"] = plistBool(true) }, false},
		{"internal-looking USB bridge", func(_ *unix.Statfs_t, d map[string]diskInfo) { d["disk0"]["BusProtocol"] = plistText("USB") }, false},
		{"disk image", func(_ *unix.Statfs_t, d map[string]diskInfo) { d["disk0"]["BusProtocol"] = plistText("Disk Image") }, false},
		{"virtual whole disk", func(_ *unix.Statfs_t, d map[string]diskInfo) { d["disk0"]["VirtualOrPhysical"] = plistText("Virtual") }, false},
		{"virtual APFS backing store", func(_ *unix.Statfs_t, d map[string]diskInfo) {
			d["disk0s2"]["VirtualOrPhysical"] = plistText("Virtual")
		}, false},
		{"system image", func(_ *unix.Statfs_t, d map[string]diskInfo) { d["disk0"]["SystemImage"] = plistBool(true) }, false},
		{"contradictory external flag", func(_ *unix.Statfs_t, d map[string]diskInfo) {
			d["disk0"]["RemovableMediaOrExternalDevice"] = plistBool(true)
		}, false},
		{"missing internal", func(_ *unix.Statfs_t, d map[string]diskInfo) { delete(d["disk3s5"], "Internal") }, false},
		{"missing removable", func(_ *unix.Statfs_t, d map[string]diskInfo) { delete(d["disk3s5"], "Removable") }, false},
		{"missing ejectable", func(_ *unix.Statfs_t, d map[string]diskInfo) { delete(d["disk3s5"], "Ejectable") }, false},
		{"wrong boolean type", func(_ *unix.Statfs_t, d map[string]diskInfo) { d["disk3s5"]["Internal"] = plistText("true") }, false},
		{"unknown protocol", func(_ *unix.Statfs_t, d map[string]diskInfo) { d["disk0"]["BusProtocol"] = plistText("Unknown") }, false},
		{"missing native device tree", func(_ *unix.Statfs_t, d map[string]diskInfo) { delete(d["disk0"], "DeviceTreePath") }, false},
		{"missing physical stores", func(_ *unix.Statfs_t, d map[string]diskInfo) { delete(d["disk3s5"], "APFSPhysicalStores") }, false},
		{"duplicate store", func(_ *unix.Statfs_t, d map[string]diskInfo) {
			d["disk3s5"]["APFSPhysicalStores"] = diskPlistValue{XMLName: xml.Name{Local: "array"}, Children: []diskPlistValue{physicalStore("disk0s2"), physicalStore("disk0s2")}}
		}, false},
		{"device removed", func(_ *unix.Statfs_t, d map[string]diskInfo) { delete(d, "disk0") }, false},
		{"wrong device metadata", func(_ *unix.Statfs_t, d map[string]diskInfo) { d["disk0"]["DeviceNode"] = plistText("/dev/disk1") }, false},
		{"network mount", func(s *unix.Statfs_t, _ map[string]diskInfo) { s.Flags = 0 }, false},
		{"non-device mount", func(s *unix.Statfs_t, _ map[string]diskInfo) {
			clear(s.Mntfromname[:])
			copy(s.Mntfromname[:], "server:/share")
		}, false},
		{"device name cannot become argument", func(_ *unix.Statfs_t, d map[string]diskInfo) {
			d["disk0s2"]["ParentWholeDisk"] = plistText("../../tmp/fake")
		}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stat, devices := darwinDiskFixture()
			if test.edit != nil {
				test.edit(&stat, devices)
			}
			err := checkDarwinVolume(t.Context(), stat, func(_ context.Context, id string) (diskInfo, error) { return devices[id], nil })
			if (err == nil) != test.want {
				t.Fatalf("accepted=%v, want %v; err=%v", err == nil, test.want, err)
			}
		})
	}
}

func TestDarwinVolumeChecksEveryFusionStore(t *testing.T) {
	stat, devices := darwinDiskFixture()
	devices["disk3s5"]["APFSPhysicalStores"] = diskPlistValue{XMLName: xml.Name{Local: "array"}, Children: []diskPlistValue{physicalStore("disk0s2"), physicalStore("disk1s2")}}
	for _, id := range []string{"disk1s2", "disk1"} {
		devices[id] = diskInfo{}
		for key, value := range devices["disk0"] {
			devices[id][key] = value
		}
		devices[id]["DeviceIdentifier"] = plistText(id)
		devices[id]["DeviceNode"] = plistText("/dev/" + id)
		devices[id]["WholeDisk"] = plistBool(id == "disk1")
		devices[id]["ParentWholeDisk"] = plistText("disk1")
	}
	read := func(_ context.Context, id string) (diskInfo, error) { return devices[id], nil }
	if err := checkDarwinVolume(t.Context(), stat, read); err != nil {
		t.Fatal("internal Fusion rejected", err)
	}
	devices["disk1"]["Internal"] = plistBool(false)
	if err := checkDarwinVolume(t.Context(), stat, read); err == nil {
		t.Fatal("external second Fusion disk accepted")
	}
}

func TestDarwinVolumeFailureDoesNotFallback(t *testing.T) {
	stat, _ := darwinDiskFixture()
	if err := checkDarwinVolume(t.Context(), stat, func(context.Context, string) (diskInfo, error) {
		return nil, errors.New("disk arbitration unavailable")
	}); err == nil {
		t.Fatal("metadata failure accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := checkDarwinVolume(ctx, stat, func(context.Context, string) (diskInfo, error) {
		t.Fatal("queried after cancellation")
		return nil, nil
	}); err == nil {
		t.Fatal("cancellation accepted")
	}
}

func TestDiskInfoParserBoundsAndTypes(t *testing.T) {
	good := `<?xml version="1.0"?><!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd"><plist version="1.0"><dict><key>Internal</key><true/><key>BusProtocol</key><string>Apple Fabric</string><key>APFSPhysicalStores</key><array><dict><key>APFSPhysicalStore</key><string>disk0s2</string></dict></array></dict></plist>`
	info, err := decodeDiskInfo([]byte(good))
	if err != nil || !info.isBool("Internal", true) || info.text("BusProtocol") != "Apple Fabric" {
		t.Fatal(info, err)
	}
	for _, bad := range []string{
		"", "garbage", `<plist><dict><key>Internal</key></dict></plist>`,
		`<plist><dict><key>Internal</key><true/><key>Internal</key><false/></dict></plist>`,
		`<plist><dict/><dict/></plist>`, `<plist><dict/></plist><plist><dict/></plist>`, strings.Repeat("<array>", 17) + strings.Repeat("</array>", 17),
		strings.Repeat(" ", diskInfoLimit+1),
	} {
		if _, err := decodeDiskInfo([]byte(bad)); err == nil {
			t.Fatalf("accepted malformed metadata %.80q", bad)
		}
	}
	var copied diskInfoBuffer
	if _, err := io.Copy(&copied, strings.NewReader(strings.Repeat("x", diskInfoLimit+1))); err == nil || copied.Len() > diskInfoLimit {
		t.Fatal("io.Copy bypassed output cap")
	}
	var output diskInfoBuffer
	if _, err := output.Write(make([]byte, diskInfoLimit)); err != nil {
		t.Fatal(err)
	}
	if _, err := output.Write([]byte{1}); err == nil || output.Len() != diskInfoLimit {
		t.Fatal("output was not bounded")
	}
}

// Read-only native acceptance: ordinary tests exercise the actual mount's
// metadata too. An external test checkout must fail, rather than bypass policy.
func TestDarwinNativeInternalVolume(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := checkVolume(t.Context(), dir); err != nil {
		t.Fatalf("native volume check: %v", err)
	}
	after, err := os.ReadDir(dir)
	if err != nil || len(before) != len(after) {
		t.Fatal("volume metadata query changed files", err)
	}
}

func TestDarwinPinnedVolumeGuard(t *testing.T) {
	r, dir := fixtureRoot(t)
	if err := os.Mkdir(filepath.Join(dir, "child"), 0700); err != nil {
		t.Fatal(err)
	}
	var mount unix.Statfs_t
	if err := unix.Statfs(dir, &mount); err != nil {
		t.Fatal(err)
	}
	if err := darwinVolumeGuard(mount)(r.root); err != nil {
		t.Fatal(err)
	}
	changed := mount
	changed.Fsid.Val[0]++
	if err := darwinVolumeGuard(changed)(r.root); err == nil {
		t.Fatal("different mount accepted")
	}
	changed = mount
	copy(changed.Mntfromname[:], "/dev/disk999999")
	if err := darwinVolumeGuard(changed)(r.root); err == nil {
		t.Fatal("different mounted device accepted")
	}

	// Every identity check reuses the captured mount guard. It cannot silently
	// open the path with fresh approval after the original mount has changed.
	original := r.volume
	checks := 0
	r.volume = func(root *os.Root) error { checks++; return original(root) }
	for i := 0; i < 100; i++ {
		if err := r.CheckIdentity(); err != nil {
			t.Fatal(err)
		}
	}
	if checks != 200 {
		t.Fatalf("identity checks bypassed pinned mount: %d", checks)
	}
	child, err := r.sub("child")
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	grandchild, err := child.sub(".")
	if err != nil {
		t.Fatal(err)
	}
	grandchild.Close()
	if checks != 202 {
		t.Fatal("descendants did not inherit volume guard", checks)
	}

	r.volume = darwinVolumeGuard(changed)
	if err := r.CheckIdentity(); !errors.Is(err, ErrRootChanged) {
		t.Fatal("changed volume retained identity", err)
	}
	if child, err := r.sub("child"); err == nil {
		child.Close()
		t.Fatal("nested changed mount accepted")
	}
	r.volume = original
	r.Close()
	if err := r.CheckIdentity(); !errors.Is(err, ErrRootChanged) {
		t.Fatal("closed original pin retained approval", err)
	}
}

func BenchmarkDarwinPinnedIdentity(b *testing.B) {
	dir, err := filepath.EvalSymlinks(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	r, err := Open(dir)
	if err != nil {
		b.Fatal(err)
	}
	defer r.Close()
	b.ResetTimer()
	for b.Loop() {
		if err := r.CheckIdentity(); err != nil {
			b.Fatal(err)
		}
	}
}
