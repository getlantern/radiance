//go:build windows

package vpn

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// deviceClassNet is GUID_DEVCLASS_NET, scoping enumeration to network adapters.
var deviceClassNet = &windows.GUID{
	Data1: 0x4d36e972,
	Data2: 0xe325,
	Data3: 0x11ce,
	Data4: [8]byte{0xbf, 0xc1, 0x08, 0x00, 0x2b, 0xe1, 0x03, 0x18},
}

// wintunHardwareID is the hardware ID wintun gives every adapter it creates.
const wintunHardwareID = "Wintun"

// removeOrphanedTUNAdapters removes Wintun devnodes holding an identity we would
// derive but which are not present, and reports how many it removed.
//
// Three rails must all hold before anything is removed: the devnode is a Wintun
// adapter, it holds a GUID one of names derives, and it is not started. A live
// adapter belongs to something — this process or another app — and is never
// touched. Removal is best effort: a devnode wedged badly enough to reject
// DIF_REMOVE (CR_INVALID_DEVNODE, seen in engineering#3854) leaves the caller to
// fall back to a fresh identity.
func removeOrphanedTUNAdapters(names []string) (int, error) {
	wanted := make(map[string]struct{}, len(names))
	for _, name := range names {
		wanted[strings.ToUpper(wintunGUIDString(name))] = struct{}{}
	}

	// Deliberately no DIGCF_PRESENT: orphans are exactly the devnodes that the
	// present-only enumeration — and sing-box's own name picker — cannot see.
	devInfo, err := windows.SetupDiGetClassDevsEx(deviceClassNet, "", 0, 0, 0, "")
	if err != nil {
		return 0, fmt.Errorf("enumerating network devices: %w", err)
	}
	defer devInfo.Close()

	var (
		removed int
		errs    []error
	)
	for i := 0; ; i++ {
		devInfoData, err := devInfo.EnumDeviceInfo(i)
		if err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_ITEMS) {
				break
			}
			continue
		}
		if !isRemovableWintunOrphan(devInfo, devInfoData, wanted) {
			continue
		}
		if err := removeDevice(devInfo, devInfoData); err != nil {
			errs = append(errs, err)
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}

// wintunGUIDString renders the adapter GUID in the registry's form. sing-tun
// reinterprets the digest as a windows.GUID, which is the little-endian field
// layout spelled out here rather than an unaligned unsafe cast.
func wintunGUIDString(name string) string {
	sum := wintunAdapterGUID(name)
	guid := windows.GUID{
		Data1: binary.LittleEndian.Uint32(sum[0:4]),
		Data2: binary.LittleEndian.Uint16(sum[4:6]),
		Data3: binary.LittleEndian.Uint16(sum[6:8]),
	}
	copy(guid.Data4[:], sum[8:16])
	return guid.String()
}

func isRemovableWintunOrphan(devInfo windows.DevInfo, devInfoData *windows.DevInfoData, wanted map[string]struct{}) bool {
	if !hasWintunHardwareID(devInfo, devInfoData) {
		return false
	}
	if isDeviceStarted(devInfoData) {
		return false
	}
	guid, err := netCfgInstanceID(devInfo, devInfoData)
	if err != nil {
		return false
	}
	_, ok := wanted[strings.ToUpper(guid)]
	return ok
}

// hasWintunHardwareID keeps cleanup off every network device wintun didn't make.
func hasWintunHardwareID(devInfo windows.DevInfo, devInfoData *windows.DevInfoData) bool {
	prop, err := devInfo.DeviceRegistryProperty(devInfoData, windows.SPDRP_HARDWAREID)
	if err != nil {
		return false
	}
	switch ids := prop.(type) {
	case string:
		return strings.EqualFold(ids, wintunHardwareID)
	case []string:
		for _, id := range ids {
			if strings.EqualFold(id, wintunHardwareID) {
				return true
			}
		}
	}
	return false
}

// isDeviceStarted reports whether the devnode is live. An unreadable status
// counts as not started: a fully orphaned devnode is precisely the case where
// CM_Get_DevNode_Status fails with CR_NO_SUCH_DEVINST.
func isDeviceStarted(devInfoData *windows.DevInfoData) bool {
	var status, problem uint32
	if err := windows.CM_Get_DevNode_Status(&status, &problem, devInfoData.DevInst, 0); err != nil {
		return false
	}
	return status&windows.DN_STARTED != 0
}

// netCfgInstanceID reads the adapter GUID wintun assigned, which is what
// CreateAdapter collides against.
func netCfgInstanceID(devInfo windows.DevInfo, devInfoData *windows.DevInfoData) (string, error) {
	handle, err := devInfo.OpenDevRegKey(devInfoData, windows.DICS_FLAG_GLOBAL, 0, windows.DIREG_DRV, registry.QUERY_VALUE)
	if err != nil {
		return "", err
	}
	key := registry.Key(handle)
	defer key.Close()
	value, _, err := key.GetStringValue("NetCfgInstanceId")
	return value, err
}

// removeDevice deletes the devnode from every hardware profile.
func removeDevice(devInfo windows.DevInfo, devInfoData *windows.DevInfoData) error {
	params := windows.RemoveDeviceParams{
		ClassInstallHeader: *windows.MakeClassInstallHeader(windows.DIF_REMOVE),
		Scope:              windows.DI_REMOVEDEVICE_GLOBAL,
	}
	if err := devInfo.SetClassInstallParams(devInfoData, &params.ClassInstallHeader, uint32(unsafe.Sizeof(params))); err != nil {
		return fmt.Errorf("setting remove params: %w", err)
	}
	if err := devInfo.CallClassInstaller(windows.DIF_REMOVE, devInfoData); err != nil {
		return fmt.Errorf("calling class installer: %w", err)
	}
	return nil
}
