package restore

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

const defaultSnapshotName = "nivenia-baseline"

func SnapshotName() string {
	if env := strings.TrimSpace(os.Getenv("NIVENIA_SNAPSHOT_NAME")); env != "" {
		return env
	}
	return defaultSnapshotName
}

func SnapshotVolume(managedRoot string) string {
	if env := strings.TrimSpace(os.Getenv("NIVENIA_SNAPSHOT_VOLUME")); env != "" {
		return env
	}
	return managedRoot
}

func runDiskutil(args ...string) (string, error) {
	cmd := exec.Command("diskutil", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("diskutil %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func diskutilAvailable() error {
	if _, err := exec.LookPath("diskutil"); err != nil {
		return fmt.Errorf("diskutil not found: %w", err)
	}
	return nil
}

func isAPFSInfo(info string) bool {
	upper := strings.ToUpper(info)
	if strings.Contains(upper, "FILE SYSTEM PERSONALITY: APFS") {
		return true
	}
	if strings.Contains(upper, "TYPE (BUNDLE): APFS") {
		return true
	}
	if strings.Contains(upper, "APFS VOLUME") {
		return true
	}
	return false
}

func snapshotPreflight(volume, name string, requireSnapshot bool) error {
	if err := diskutilAvailable(); err != nil {
		return err
	}
	if strings.TrimSpace(volume) == "" {
		return fmt.Errorf("snapshot volume is empty")
	}
	if _, err := os.Stat(volume); err != nil {
		return fmt.Errorf("snapshot volume not found: %s: %w", volume, err)
	}
	info, err := runDiskutil("info", volume)
	if err != nil {
		return err
	}
	if !isAPFSInfo(info) {
		return fmt.Errorf("snapshot volume is not APFS: %s", strings.TrimSpace(info))
	}
	if volume == "/System/Volumes/Data" {
		fmt.Fprintln(os.Stderr, "[WARN] snapshot scope is the full Data volume; system/user changes will be reverted")
		fmt.Fprintln(os.Stderr, "[WARN] for DeepFreeze-like isolation, prefer a dedicated APFS volume and set NIVENIA_SNAPSHOT_VOLUME")
	}

	names, err := listAPFSSnapshotNames(volume)
	if err != nil {
		return err
	}
	if requireSnapshot {
		found := false
		for _, existing := range names {
			if existing == name {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("snapshot %q not found on %s; available=%v", name, volume, names)
		}
	}
	return nil
}

func listAPFSSnapshotNames(volume string) ([]string, error) {
	out, err := runDiskutil("apfs", "listSnapshots", volume)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(out, "\n")
	var names []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Name:") {
			name := strings.TrimSpace(strings.TrimPrefix(line, "Name:"))
			if name != "" {
				names = append(names, name)
			}
		}
	}
	return names, nil
}

func deleteAPFSSnapshot(volume, name string) error {
	_, err := runDiskutil("apfs", "deleteSnapshot", volume, "-name", name)
	return err
}

func createAPFSSnapshot(volume, name string) error {
	if name == "" {
		return fmt.Errorf("snapshot name is empty")
	}
	if err := snapshotPreflight(volume, name, false); err != nil {
		return err
	}
	// Remove any existing snapshot with the same name to keep a single baseline.
	if names, err := listAPFSSnapshotNames(volume); err == nil {
		for _, existing := range names {
			if existing == name {
				_ = deleteAPFSSnapshot(volume, name)
				break
			}
		}
	}
	_, err := runDiskutil("apfs", "snapshot", volume, "-name", name)
	return err
}

func revertAPFSSnapshot(volume, name string) error {
	if name == "" {
		return fmt.Errorf("snapshot name is empty")
	}
	if err := snapshotPreflight(volume, name, true); err != nil {
		return err
	}
	_, err := runDiskutil("apfs", "revertToSnapshot", volume, "-name", name)
	return err
}

func CaptureBaseline(managedRoot string) error {
	volume := SnapshotVolume(managedRoot)
	return createAPFSSnapshot(volume, SnapshotName())
}

func RestoreFromBaseline(managedRoot string) error {
	volume := SnapshotVolume(managedRoot)
	return revertAPFSSnapshot(volume, SnapshotName())
}
