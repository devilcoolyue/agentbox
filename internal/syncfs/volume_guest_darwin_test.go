package syncfs

import (
	"context"
	"errors"
	"testing"

	"golang.org/x/sys/unix"
)

func darwinGuestFixture() (unix.Statfs_t, unix.Statfs_t, map[string]diskInfo) {
	selected, disks := darwinDiskFixture()
	selected.Fsid.Val[0] = 11
	for _, disk := range disks {
		delete(disk, "BusProtocol")
	}
	root := selected
	root.Fsid.Val[0] = 22 // System snapshot and Data have different mount IDs.
	clear(root.Mntfromname[:])
	copy(root.Mntfromname[:], "/dev/disk3s1s1")
	disks["disk3s1s1"] = cloneDarwinDisk(disks["disk3s5"], "disk3s1s1")
	return selected, root, disks
}

func cloneDarwinDisk(source diskInfo, device string) diskInfo {
	copy := make(diskInfo)
	for key, value := range source {
		copy[key] = value
	}
	copy["DeviceIdentifier"] = plistText(device)
	copy["DeviceNode"] = plistText("/dev/" + device)
	return copy
}

func TestDarwinGuestOnlyVerifiedFixedSystemDisk(t *testing.T) {
	tests := []struct {
		name string
		edit func(*unix.Statfs_t, map[string]diskInfo, *string)
		want bool
	}{
		{"observed VirtualMac system disk without bus metadata", nil, true},
		{"empty typed bus metadata", func(_ *unix.Statfs_t, disks map[string]diskInfo, _ *string) {
			disks["disk0"]["BusProtocol"] = plistText("")
		}, true},
		{"physical Mac missing bus is still unknown", func(_ *unix.Statfs_t, _ map[string]diskInfo, model *string) { *model = "MacBookPro18,3" }, false},
		{"unknown virtual model", func(_ *unix.Statfs_t, _ map[string]diskInfo, model *string) { *model = "VirtualMac99,1" }, false},
		{"model prefix cannot grant permission", func(_ *unix.Statfs_t, _ map[string]diskInfo, model *string) { *model = "VirtualMac2,1-extra" }, false},
		{"explicit unknown bus", func(_ *unix.Statfs_t, disks map[string]diskInfo, _ *string) {
			disks["disk0"]["BusProtocol"] = plistText("Unknown")
		}, false},
		{"USB bus", func(_ *unix.Statfs_t, disks map[string]diskInfo, _ *string) {
			disks["disk0"]["BusProtocol"] = plistText("USB")
		}, false},
		{"disk image bus", func(_ *unix.Statfs_t, disks map[string]diskInfo, _ *string) {
			disks["disk0"]["BusProtocol"] = plistText("Disk Image")
		}, false},
		{"malformed bus property", func(_ *unix.Statfs_t, disks map[string]diskInfo, _ *string) {
			disks["disk0"]["BusProtocol"] = plistBool(false)
		}, false},
		{"explicit virtual whole disk", func(_ *unix.Statfs_t, disks map[string]diskInfo, _ *string) {
			disks["disk0"]["VirtualOrPhysical"] = plistText("Virtual")
		}, false},
		{"missing whole classification", func(_ *unix.Statfs_t, disks map[string]diskInfo, _ *string) {
			delete(disks["disk0"], "VirtualOrPhysical")
		}, false},
		{"missing device tree", func(_ *unix.Statfs_t, disks map[string]diskInfo, _ *string) { delete(disks["disk0"], "DeviceTreePath") }, false},
		{"external disk", func(_ *unix.Statfs_t, disks map[string]diskInfo, _ *string) {
			disks["disk0"]["Internal"] = plistBool(false)
		}, false},
		{"removable disk", func(_ *unix.Statfs_t, disks map[string]diskInfo, _ *string) {
			disks["disk0"]["Removable"] = plistBool(true)
		}, false},
		{"ejectable disk", func(_ *unix.Statfs_t, disks map[string]diskInfo, _ *string) {
			disks["disk0"]["Ejectable"] = plistBool(true)
		}, false},
		{"nonlocal system root", func(root *unix.Statfs_t, _ map[string]diskInfo, _ *string) { root.Flags = 0 }, false},
		{"unknown system filesystem", func(root *unix.Statfs_t, _ map[string]diskInfo, _ *string) {
			clear(root.Fstypename[:])
			copy(root.Fstypename[:], "macfuse")
		}, false},
		{"root filesystem metadata mismatch", func(_ *unix.Statfs_t, disks map[string]diskInfo, _ *string) {
			disks["disk3s1s1"]["FilesystemType"] = plistText("hfs")
		}, false},
		{"system root metadata unavailable", func(_ *unix.Statfs_t, disks map[string]diskInfo, _ *string) { delete(disks, "disk3s1s1") }, false},
		{"system root external flag", func(_ *unix.Statfs_t, disks map[string]diskInfo, _ *string) {
			disks["disk3s1s1"]["Internal"] = plistBool(false)
		}, false},
		{"system root missing backing chain", func(_ *unix.Statfs_t, disks map[string]diskInfo, _ *string) {
			delete(disks["disk3s1s1"], "APFSPhysicalStores")
		}, false},
		{"system root multi-store is not established evidence", func(_ *unix.Statfs_t, disks map[string]diskInfo, _ *string) {
			stores := disks["disk3s1s1"]["APFSPhysicalStores"]
			stores.Children = append(stores.Children, physicalStore("disk0s9"))
			disks["disk3s1s1"]["APFSPhysicalStores"] = stores
		}, false},
		{"another fixed internal guest disk", func(_ *unix.Statfs_t, disks map[string]diskInfo, _ *string) {
			disks["disk9s2"] = cloneDarwinDisk(disks["disk0s2"], "disk9s2")
			disks["disk9s2"]["ParentWholeDisk"] = plistText("disk9")
			stores := disks["disk3s1s1"]["APFSPhysicalStores"]
			stores.Children = []diskPlistValue{physicalStore("disk9s2")}
			disks["disk3s1s1"]["APFSPhysicalStores"] = stores
		}, false},
		{"cannot query root physical store", func(_ *unix.Statfs_t, disks map[string]diskInfo, _ *string) {
			stores := disks["disk3s1s1"]["APFSPhysicalStores"]
			stores.Children = []diskPlistValue{physicalStore("disk0s99")}
			disks["disk3s1s1"]["APFSPhysicalStores"] = stores
		}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			selected, root, disks := darwinGuestFixture()
			model := "VirtualMac2,1"
			if test.edit != nil {
				test.edit(&root, disks, &model)
			}
			reads := map[string]int{}
			err := checkDarwinVolumeWithSystem(t.Context(), selected, func(_ context.Context, device string) (diskInfo, error) { reads[device]++; return disks[device], nil }, func(context.Context) (string, unix.Statfs_t, error) { return model, root, nil })
			if (err == nil) != test.want {
				t.Fatalf("accepted=%v want=%v err=%v", err == nil, test.want, err)
			}
			for _, count := range reads {
				if count > 1 {
					t.Fatal("one approval unnecessarily repeated a device metadata query", reads)
				}
			}
		})
	}
}

func TestDarwinGuestSystemProofMustRemainCurrentAndCancelable(t *testing.T) {
	for _, kind := range []string{"unavailable", "remounted", "model_changed", "canceled"} {
		t.Run(kind, func(t *testing.T) {
			selected, root, disks := darwinGuestFixture()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			err := checkDarwinVolumeWithSystem(ctx, selected, func(query context.Context, device string) (diskInfo, error) {
				if query != ctx {
					t.Fatal("metadata query lost request context")
				}
				return disks[device], nil
			}, func(query context.Context) (string, unix.Statfs_t, error) {
				if query != ctx {
					t.Fatal("system-root probe lost request context")
				}
				calls++
				if kind == "unavailable" {
					return "", unix.Statfs_t{}, errors.New("no root evidence")
				}
				if kind == "canceled" {
					cancel()
				}
				if calls > 1 {
					if kind == "remounted" {
						root.Fsid.Val[0]++
					}
					if kind == "model_changed" {
						return "MacBookPro18,3", root, nil
					}
				}
				return "VirtualMac2,1", root, nil
			})
			if err == nil {
				t.Fatal("lost or changed system-root proof granted approval")
			}
			if kind == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatal("cancellation was hidden", err)
			}
		})
	}
}

func TestDarwinPhysicalInternalDisksDoNotNeedGuestProbe(t *testing.T) {
	selected, disks := darwinDiskFixture()
	if err := checkDarwinVolumeWithSystem(t.Context(), selected, func(_ context.Context, device string) (diskInfo, error) { return disks[device], nil }, func(context.Context) (string, unix.Statfs_t, error) {
		t.Fatal("normal physical internal disk depended on a VM exception")
		return "", unix.Statfs_t{}, nil
	}); err != nil {
		t.Fatal(err)
	}
}
