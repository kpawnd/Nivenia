package restore

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	RestoreModeRsync    = "rsync"
	RestoreModeSnapshot = "snapshot"
)

func canonical(p string) string {
	cleaned := filepath.Clean(p)
	if cleaned == "." {
		return "/"
	}
	return cleaned
}

func ensureDir(path string) error {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", path, err)
	}
	return nil
}

func rsyncExcludeArgs(excludes []string) []string {
	args := make([]string, 0, len(excludes)*2)
	for _, p := range excludes {
		trimmed := strings.TrimSpace(p)
		if trimmed == "" {
			continue
		}
		args = append(args, "--exclude", strings.TrimPrefix(canonical(trimmed), "/"))
	}
	return args
}

// SpeedFormatter converts bytes to human-readable speed
func formatSpeed(bytes int64) string {
	const (
		kb = 1024
		mb = kb * 1024
		gb = mb * 1024
	)
	switch {
	case bytes >= gb:
		return fmt.Sprintf("%.2f GB/s", float64(bytes)/float64(gb))
	case bytes >= mb:
		return fmt.Sprintf("%.2f MB/s", float64(bytes)/float64(mb))
	case bytes >= kb:
		return fmt.Sprintf("%.2f KB/s", float64(bytes)/float64(kb))
	default:
		return fmt.Sprintf("%d B/s", bytes)
	}
}

var (
	rsyncHelpOnce sync.Once
	rsyncHelpText string
)

func rsyncSupportsFlag(flag string) bool {
	rsyncHelpOnce.Do(func() {
		cmd := exec.Command("rsync", "--help")
		out, err := cmd.CombinedOutput()
		if err != nil {
			rsyncHelpText = ""
			return
		}
		rsyncHelpText = string(out)
	})
	if rsyncHelpText == "" {
		return false
	}
	return strings.Contains(rsyncHelpText, flag)
}

func runRsync(src, dst string, excludes []string, delete bool) error {
	args := []string{"-aH", "--numeric-ids"}
	if delete {
		args = append(args, "--delete")
	}
	// Optimizations for initial capture:
	// --whole-file: disable delta algorithm (faster for local copies)
	args = append(args, "--whole-file")

	if rsyncSupportsFlag("--info=progress2") {
		args = append(args, "--info=progress2")
	} else {
		// Compatible with older Apple rsync/openrsync builds.
		args = append(args, "--progress")
	}

	// Avoid metadata failures on protected paths during restore/capture.
	if rsyncSupportsFlag("--no-xattrs") {
		args = append(args, "--no-xattrs")
	}
	if rsyncSupportsFlag("--no-acls") {
		args = append(args, "--no-acls")
	}

	args = append(args, rsyncExcludeArgs(excludes)...)
	args = append(args, src, dst)

	cmd := exec.Command("rsync", args...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("rsync start: %w", err)
	}

	scanner := bufio.NewScanner(stderr)
	spinner := []string{"|", "/", "-", "\\"}
	spinIdx := 0
	speedRegex := regexp.MustCompile(`(\d+\.\d+)([KMG])B/s`)
	lastSpeed := ""
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	var errorLines []string

	go func() {
		for range ticker.C {
			fmt.Fprintf(os.Stderr, "\r\033[2K[CAP] Capturing baseline %s %s", spinner[spinIdx%len(spinner)], lastSpeed)
			spinIdx++
		}
	}()

	for scanner.Scan() {
		line := scanner.Text()
		if matches := speedRegex.FindStringSubmatch(line); len(matches) > 0 {
			lastSpeed = matches[1] + matches[2] + "B/s"
		} else if strings.TrimSpace(line) != "" && !strings.Contains(line, "to-check") && !strings.Contains(line, "sent") && !strings.Contains(line, "total") {
			// Capture non-progress error lines
			errorLines = append(errorLines, line)
		}
	}

	ticker.Stop()
	fmt.Fprintf(os.Stderr, "\r\033[2K")

	if err := cmd.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			if exitErr.ExitCode() == 24 {
				fmt.Fprintln(os.Stderr, "[CAP] rsync reported vanished files (exit 24) on live filesystem; continuing")
				return nil
			}
		}

		// Build detailed error message with rsync arguments for debugging
		errMsg := fmt.Sprintf("rsync failed: %v\ncommand: rsync %s", err, strings.Join(args, " "))
		if len(errorLines) > 0 {
			errMsg += "\nrsync output:\n" + strings.Join(errorLines, "\n")
		}
		return fmt.Errorf(errMsg)
	}

	return nil
}

func effectiveRestoreMode(mode string) string {
	if env := strings.TrimSpace(os.Getenv("NIVENIA_RESTORE_MODE")); env != "" {
		mode = env
	}
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		return RestoreModeRsync
	}
	return mode
}

func snapshotName() string {
	if env := strings.TrimSpace(os.Getenv("NIVENIA_SNAPSHOT_NAME")); env != "" {
		return env
	}
	return "nivenia-baseline"
}

func snapshotVolume(managedRoot string) string {
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
	_, err := runDiskutil("apfs", "revertToSnapshot", volume, "-name", name)
	return err
}

func CaptureBaseline(managedRoot, baselineRoot string, excludes []string) error {
	src := canonical(managedRoot)
	dst := canonical(baselineRoot)

	if src == dst {
		return fmt.Errorf("managed_root and baseline_root must be different")
	}
	if err := ensureDir(dst); err != nil {
		return err
	}

	return runRsync(src+"/", dst+"/", excludes, true)
}

func CaptureBaselineWithMode(managedRoot, baselineRoot string, excludes []string, mode string) error {
	mode = effectiveRestoreMode(mode)
	if mode == RestoreModeSnapshot {
		volume := snapshotVolume(managedRoot)
		return createAPFSSnapshot(volume, snapshotName())
	}
	return CaptureBaseline(managedRoot, baselineRoot, excludes)
}

func RestoreFromBaseline(baselineRoot, managedRoot string, excludes []string) error {
	src := canonical(baselineRoot)
	dst := canonical(managedRoot)

	st, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("baseline missing at %s: %w", src, err)
	}
	if !st.IsDir() {
		return fmt.Errorf("baseline path %s is not a directory", src)
	}

	// Strict default: restore with delete pass so new files are removed on reboot.
	deletePass := true
	if strings.EqualFold(os.Getenv("NIVENIA_SAFE_SYNC_ONLY"), "1") {
		deletePass = false
	}

	return runRsync(src+"/", dst+"/", excludes, deletePass)
}

func RestoreFromBaselineWithMode(baselineRoot, managedRoot string, excludes []string, mode string) error {
	mode = effectiveRestoreMode(mode)
	if mode == RestoreModeSnapshot {
		volume := snapshotVolume(managedRoot)
		return revertAPFSSnapshot(volume, snapshotName())
	}
	return RestoreFromBaseline(baselineRoot, managedRoot, excludes)
}
