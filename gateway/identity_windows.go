package main

import (
	"fmt"
	"syscall"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	registryPath        = `SOFTWARE\camOS\Gateway`
	registrySiteIDValue = "SiteID"
)

var procRegFlushKey = windows.NewLazySystemDLL("advapi32.dll").NewProc("RegFlushKey")

func saveSiteID(siteID int64) error {
	key, _, err := registry.CreateKey(
		registry.LOCAL_MACHINE,
		registryPath,
		registry.SET_VALUE|registry.QUERY_VALUE|registry.WOW64_64KEY,
	)
	if err != nil {
		return fmt.Errorf("SiteID write failed: %w", err)
	}
	defer key.Close()

	if err := key.SetQWordValue(registrySiteIDValue, uint64(siteID)); err != nil {
		return fmt.Errorf("SiteID write failed: %w", err)
	}
	status, _, _ := procRegFlushKey.Call(uintptr(key))
	if status != 0 {
		return fmt.Errorf("SiteID flush failed: %w", syscall.Errno(status))
	}
	return nil
}

func loadSiteID() (int64, error) {
	key, err := registry.OpenKey(
		registry.LOCAL_MACHINE,
		registryPath,
		registry.QUERY_VALUE|registry.WOW64_64KEY,
	)
	if err != nil {
		return 0, fmt.Errorf("SiteID unavailable; run commission <site_id> as administrator: %w", err)
	}
	defer key.Close()

	value, _, err := key.GetIntegerValue(registrySiteIDValue)
	if err != nil {
		return 0, fmt.Errorf("SiteID unavailable; run commission <site_id> as administrator: %w", err)
	}
	return int64(value), nil
}
