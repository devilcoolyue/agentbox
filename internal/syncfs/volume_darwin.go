package syncfs

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const diskInfoLimit = 1 << 20

var diskDeviceName = regexp.MustCompile(`^disk[0-9]+(s[0-9]+)*$`)

// diskutil uses Disk Arbitration / IOKit metadata. Use its fixed, system-owned
// executable and machine-readable output so the CGO-free sidecar works on both
// Mac architectures. Never search PATH, invoke a shell, or pass user paths.
func readDiskInfo(ctx context.Context, device string) (diskInfo, error) {
	if !diskDeviceName.MatchString(device) {
		return nil, ErrUnsupportedVolume
	}
	cmd := exec.CommandContext(ctx, "/usr/sbin/diskutil", "info", "-plist", "/dev/"+device)
	cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LC_ALL=C"}
	return runDiskInfoCommand(ctx, cmd)
}

func runDiskInfoCommand(ctx context.Context, cmd *exec.Cmd) (diskInfo, error) {
	cmd.WaitDelay = time.Second
	var output diskInfoBuffer
	cmd.Stdout = &output
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrUnsupportedVolume
	}
	return decodeDiskInfo(output.Bytes())
}

func checkVolume(ctx context.Context, name string) (volumeGuard, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var before, after unix.Statfs_t
	if err := unix.Statfs(name, &before); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := checkDarwinVolume(ctx, before, readDiskInfo); err != nil {
		return nil, err
	}
	// Device names can be reused after removal. New roots query metadata
	// afresh; an existing root keeps only a guard for this exact pinned mount.
	// Reject a mount change while obtaining metadata, too.
	if err := unix.Statfs(name, &after); err != nil {
		return nil, err
	}
	if before.Fsid != after.Fsid || before.Mntfromname != after.Mntfromname || before.Fstypename != after.Fstypename || after.Flags&unix.MNT_LOCAL == 0 {
		return nil, ErrUnsupportedVolume
	}
	return darwinVolumeGuard(before), nil
}

// The approved mount must also match the pinned root and every descendant
// directory. This prevents an external mount inside an internal project from
// bypassing the root policy, and closes replacement between statfs and OpenRoot.
func darwinVolumeGuard(approved unix.Statfs_t) volumeGuard {
	return func(root *os.Root) error {
		file, err := root.Open(".")
		if err != nil {
			return err
		}
		defer file.Close()
		var actual unix.Statfs_t
		if err := unix.Fstatfs(int(file.Fd()), &actual); err != nil {
			return err
		}
		if approved.Fsid != actual.Fsid || approved.Mntfromname != actual.Mntfromname || approved.Fstypename != actual.Fstypename || actual.Flags&unix.MNT_LOCAL == 0 {
			return ErrUnsupportedVolume
		}
		return nil
	}
}

func checkDarwinVolume(ctx context.Context, stat unix.Statfs_t, read func(context.Context, string) (diskInfo, error)) error {
	filesystem := unix.ByteSliceToString(stat.Fstypename[:])
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if stat.Flags&unix.MNT_LOCAL == 0 || (filesystem != "apfs" && filesystem != "hfs") {
		return ErrUnsupportedVolume
	}
	device := strings.TrimPrefix(unix.ByteSliceToString(stat.Mntfromname[:]), "/dev/")
	if !diskDeviceName.MatchString(device) {
		return ErrUnsupportedVolume
	}
	get := func(device string) (diskInfo, error) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !diskDeviceName.MatchString(device) {
			return nil, ErrUnsupportedVolume
		}
		info, err := read(ctx, device)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil || info.text("DeviceIdentifier") != device || info.text("DeviceNode") != "/dev/"+device || !info.internalFixed() {
			return nil, ErrUnsupportedVolume
		}
		return info, nil
	}
	volume, err := get(device)
	if err != nil {
		return err
	}
	if volume.text("FilesystemType") != filesystem {
		return ErrUnsupportedVolume
	}
	stores := []diskInfo{volume}
	if unix.ByteSliceToString(stat.Fstypename[:]) == "apfs" {
		// Synthesized APFS volumes are virtual even on internal SSDs. Validate
		// every physical store (including both disks of a Fusion container).
		backing, ok := volume["APFSPhysicalStores"]
		if !ok || backing.XMLName.Local != "array" || len(backing.Children) == 0 || len(backing.Children) > 32 {
			return ErrUnsupportedVolume
		}
		stores = nil
		seen := make(map[string]bool)
		for _, entry := range backing.Children {
			fields, err := entry.dict()
			if err != nil {
				return ErrUnsupportedVolume
			}
			name := fields.text("APFSPhysicalStore")
			if seen[name] {
				return ErrUnsupportedVolume
			}
			seen[name] = true
			store, err := get(name)
			if err != nil {
				return err
			}
			stores = append(stores, store)
		}
	}
	for _, store := range stores {
		if store.text("VirtualOrPhysical") == "Virtual" {
			return ErrUnsupportedVolume
		}
		whole := store
		if !store.isBool("WholeDisk", true) {
			whole, err = get(store.text("ParentWholeDisk"))
			if err != nil {
				return err
			}
		}
		if !whole.isBool("WholeDisk", true) || !whole.physicalInternalBus() {
			return ErrUnsupportedVolume
		}
	}
	return nil
}

func (d diskInfo) internalFixed() bool {
	if !d.isBool("Internal", true) || !d.isBool("Removable", false) || !d.isBool("Ejectable", false) {
		return false
	}
	// Supplemental flags are not emitted by every supported macOS version.
	// If present they must explicitly agree with the required core flags.
	for _, key := range []string{"RemovableMedia", "RemovableMediaOrExternalDevice", "EjectableOnly", "SystemImage"} {
		if _, ok := d[key]; ok && !d.isBool(key, false) {
			return false
		}
	}
	return true
}

func (d diskInfo) physicalInternalBus() bool {
	// VirtualOrPhysical is "Unknown" even for current Apple Silicon internal
	// SSDs. Require positive native device-tree and internal bus evidence;
	// do not treat an absent/unknown classification as proof of a physical disk.
	if d.text("VirtualOrPhysical") == "Virtual" || !strings.HasPrefix(d.text("DeviceTreePath"), "IODeviceTree:/") {
		return false
	}
	switch d.text("BusProtocol") {
	case "Apple Fabric", "PCI", "PCI-Express", "SATA", "ATA", "ATAPI", "SCSI", "SAS", "NVMe":
		return true
	default:
		return false
	}
}

type diskInfoBuffer struct{ buffer bytes.Buffer }

func (b *diskInfoBuffer) Bytes() []byte { return b.buffer.Bytes() }
func (b *diskInfoBuffer) Len() int      { return b.buffer.Len() }

func (b *diskInfoBuffer) Write(p []byte) (int, error) {
	if len(p) > diskInfoLimit-b.Len() {
		return 0, ErrUnsupportedVolume
	}
	return b.buffer.Write(p)
}

type diskPlistValue struct {
	XMLName  xml.Name
	Text     string           `xml:",chardata"`
	Children []diskPlistValue `xml:",any"`
}

type diskInfo map[string]diskPlistValue

func decodeDiskInfo(data []byte) (diskInfo, error) {
	if len(data) == 0 || len(data) > diskInfoLimit {
		return nil, ErrUnsupportedVolume
	}
	// Bound nesting before decoding the tree. No external DTD/entities are
	// resolved by encoding/xml; metadata is never evaluated as shell text.
	decoder := xml.NewDecoder(bytes.NewReader(data))
	depth, tokens, roots := 0, 0, 0
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, ErrUnsupportedVolume
		}
		tokens++
		if tokens > 32768 {
			return nil, ErrUnsupportedVolume
		}
		switch token.(type) {
		case xml.StartElement:
			if depth == 0 {
				roots++
				if roots > 1 {
					return nil, ErrUnsupportedVolume
				}
			}
			depth++
			if depth > 16 {
				return nil, ErrUnsupportedVolume
			}
		case xml.EndElement:
			depth--
		case xml.CharData:
			if depth == 0 && strings.TrimSpace(string(token.(xml.CharData))) != "" {
				return nil, ErrUnsupportedVolume
			}
		}
	}
	if depth != 0 || roots != 1 {
		return nil, ErrUnsupportedVolume
	}
	var value diskPlistValue
	if xml.Unmarshal(data, &value) != nil || value.XMLName.Local != "plist" || len(value.Children) != 1 {
		return nil, ErrUnsupportedVolume
	}
	return value.Children[0].dict()
}

func (v diskPlistValue) dict() (diskInfo, error) {
	if v.XMLName.Local != "dict" || len(v.Children)%2 != 0 {
		return nil, ErrUnsupportedVolume
	}
	out := make(diskInfo)
	for i := 0; i < len(v.Children); i += 2 {
		key := v.Children[i]
		if key.XMLName.Local != "key" || key.Text == "" || len(key.Children) != 0 {
			return nil, ErrUnsupportedVolume
		}
		if _, found := out[key.Text]; found {
			return nil, ErrUnsupportedVolume
		}
		out[key.Text] = v.Children[i+1]
	}
	return out, nil
}

func (d diskInfo) text(key string) string {
	v := d[key]
	if v.XMLName.Local != "string" || len(v.Children) != 0 {
		return ""
	}
	return v.Text
}

func (d diskInfo) isBool(key string, want bool) bool {
	v := d[key]
	expected := "false"
	if want {
		expected = "true"
	}
	return v.XMLName.Local == expected && len(v.Children) == 0 && strings.TrimSpace(v.Text) == ""
}
