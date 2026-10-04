package syncfs

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// This diagnostic is test-only and always uses its own temporary directory.
// Never dump diskutil's raw plist: it contains volume names, UUIDs, serials and
// paths. Production volume policy is exercised unchanged and remains strict.
func TestDarwinVolumeEnvironmentReport(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	var mount unix.Statfs_t
	if err := unix.Statfs(t.TempDir(), &mount); err != nil {
		t.Fatal("could not inspect the isolated temporary directory's mount")
	}
	filesystem := unix.ByteSliceToString(mount.Fstypename[:])
	device := strings.TrimPrefix(unix.ByteSliceToString(mount.Mntfromname[:]), "/dev/")
	model, err := unix.Sysctl("hw.model")
	if err != nil || !regexp.MustCompile(`^[A-Za-z0-9,._-]{1,64}$`).MatchString(model) {
		model = "unavailable"
	}
	cache := map[string]diskInfo{}
	failures := map[string]error{}
	roles := map[string]string{}
	var disks []map[string]any
	read := func(device string) (diskInfo, error) {
		if info, ok := cache[device]; ok {
			return info, failures[device]
		}
		info, err := readDiskInfo(ctx, device)
		cache[device], failures[device] = info, err
		return info, err
	}
	collect := func(role, device string) diskInfo {
		if roles[device] == "" {
			roles[device] = role
		}
		info, err := read(device)
		row := darwinDiagnosticFields(info)
		row["role"] = role
		row["read_ok"] = err == nil
		row["device_identifier_valid"] = diskDeviceName.MatchString(device)
		row["device_identity_matches_query"] = info.text("DeviceIdentifier") == device && info.text("DeviceNode") == "/dev/"+device
		disks = append(disks, row)
		return info
	}
	volume := collect("volume", device)
	var stores []diskInfo
	storeNames := []string{}
	backing := volume["APFSPhysicalStores"]
	storeListValid := filesystem != "apfs"
	if filesystem == "apfs" && backing.XMLName.Local == "array" && len(backing.Children) > 0 && len(backing.Children) <= 32 {
		storeListValid = true
		seen := map[string]bool{}
		for index, child := range backing.Children {
			fields, err := child.dict()
			name := fields.text("APFSPhysicalStore")
			if err != nil || !diskDeviceName.MatchString(name) || seen[name] {
				storeListValid = false
			}
			seen[name] = true
			storeNames = append(storeNames, name)
			stores = append(stores, collect(fmt.Sprintf("apfs_store_%d", index), name))
		}
	} else if filesystem != "apfs" {
		storeNames, stores = []string{device}, []diskInfo{volume}
	}
	wholes := make([]diskInfo, len(stores))
	for index, store := range stores {
		name := storeNames[index]
		if !store.isBool("WholeDisk", true) {
			name = store.text("ParentWholeDisk")
		}
		wholes[index] = collect(fmt.Sprintf("backing_whole_%d", index), name)
	}
	trace := []string{}
	rejectedAt := ""
	policyErr := checkDarwinVolume(ctx, mount, func(_ context.Context, name string) (diskInfo, error) {
		role := roles[name]
		if role == "" {
			role = "unclassified_device"
		}
		trace = append(trace, role)
		info, err := read(name)
		switch {
		case err != nil:
			rejectedAt = role + ":metadata_query"
		case info.text("DeviceIdentifier") != name || info.text("DeviceNode") != "/dev/"+name:
			rejectedAt = role + ":device_identity"
		case !info.internalFixed():
			rejectedAt = role + ":internal_fixed_policy"
		}
		return info, err
	})
	if policyErr == nil {
		rejectedAt = "accepted"
	} else if rejectedAt == "" {
		switch {
		case ctx.Err() != nil:
			rejectedAt = "diagnostic_deadline"
		case mount.Flags&unix.MNT_LOCAL == 0:
			rejectedAt = "kernel:not_local"
		case filesystem != "apfs" && filesystem != "hfs":
			rejectedAt = "kernel:unsupported_filesystem"
		case !diskDeviceName.MatchString(device):
			rejectedAt = "kernel:non_disk_source"
		case volume.text("FilesystemType") != filesystem:
			rejectedAt = "volume:filesystem_mismatch"
		case !storeListValid:
			rejectedAt = "volume:invalid_apfs_store_list"
		default:
			for index, store := range stores {
				if store.text("VirtualOrPhysical") == "Virtual" {
					rejectedAt = fmt.Sprintf("backing_store_%d:virtual", index)
					break
				}
				if !store.isBool("WholeDisk", true) && !diskDeviceName.MatchString(store.text("ParentWholeDisk")) {
					rejectedAt = fmt.Sprintf("backing_store_%d:invalid_parent", index)
					break
				}
				if !wholes[index].isBool("WholeDisk", true) || !wholes[index].physicalInternalBus() {
					rejectedAt = fmt.Sprintf("backing_whole_%d:physical_internal_bus_policy", index)
					break
				}
			}
			if rejectedAt == "" {
				rejectedAt = "unclassified_rejection"
			}
		}
	}
	report := map[string]any{
		"version": 1, "platform": runtime.GOOS + "/" + runtime.GOARCH, "hw_model": model,
		"kernel_filesystem": darwinDiagnosticText(filesystem), "kernel_local": mount.Flags&unix.MNT_LOCAL != 0,
		"apfs_store_count": len(backing.Children), "apfs_store_list_valid": storeListValid,
		"disks": disks, "policy_query_roles": trace, "policy_accepted": policyErr == nil, "rejected_at": rejectedAt,
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal("could not encode whitelisted volume diagnostic")
	}
	t.Log("AGENTBOX_MAC_VOLUME_REPORT=" + string(encoded))
}

func darwinDiagnosticText(value string) string {
	if value == "" {
		return "missing"
	}
	if !regexp.MustCompile(`^[A-Za-z0-9 ._-]{1,80}$`).MatchString(value) {
		return "unrecognized_value"
	}
	return value
}

func darwinDiagnosticFields(info diskInfo) map[string]any {
	row := map[string]any{}
	for _, key := range []string{"Internal", "Removable", "Ejectable", "RemovableMedia", "RemovableMediaOrExternalDevice", "EjectableOnly", "SystemImage", "WholeDisk"} {
		switch {
		case info.isBool(key, true):
			row[key] = true
		case info.isBool(key, false):
			row[key] = false
		default:
			row[key] = "missing_or_invalid"
		}
	}
	for _, key := range []string{"FilesystemType", "VirtualOrPhysical", "BusProtocol"} {
		row[key] = darwinDiagnosticText(info.text(key))
	}
	path := info.text("DeviceTreePath")
	_, exists := info["DeviceTreePath"]
	row["device_tree_present"] = exists
	switch {
	case strings.HasPrefix(path, "IODeviceTree:/"):
		row["device_tree_prefix"] = "IODeviceTree:/"
	case strings.HasPrefix(path, "IOService:/"):
		row["device_tree_prefix"] = "IOService:/"
	case path == "":
		row["device_tree_prefix"] = "missing_or_invalid"
	default:
		row["device_tree_prefix"] = "other"
	}
	row["internal_fixed_pass"] = info.internalFixed()
	row["physical_internal_bus_pass"] = info.physicalInternalBus()
	return row
}

func TestDarwinVolumeReportOmitsPrivateMetadata(t *testing.T) {
	_, disks := darwinDiskFixture()
	info := disks["disk0"]
	for _, key := range []string{"VolumeUUID", "DiskUUID", "SerialNumber", "VolumeName", "MountPoint", "DeviceTreePath"} {
		info[key] = plistText("IODeviceTree:/PRIVATE-SERIAL-AND-PATH")
	}
	encoded, err := json.Marshal(darwinDiagnosticFields(info))
	if err != nil || strings.Contains(string(encoded), "PRIVATE") || strings.Contains(string(encoded), "UUID") || strings.Contains(string(encoded), "SerialNumber") || strings.Contains(string(encoded), "MountPoint") {
		t.Fatal("volume report leaked metadata outside its whitelist")
	}
}
