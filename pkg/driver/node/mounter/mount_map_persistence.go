// Package mounter provides mount implementations for the CSI driver.
package mounter

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"k8s.io/klog/v2"
)

// MetaFileName returns the path to the .meta.json file for a given volume.
// The file lives alongside the FUSE source mount directory.
func MetaFileName(kubeletPath, volumeID string) string {
	return filepath.Join(kubeletPath, "plugins", "s3.csi.aws.com", "mnt", volumeID+".meta.json")
}

// MountMeta is the JSON-serializable structure persisted alongside each source mount.
// It records the parameters used to create the mount — enabling validation on recovery
// and for subsequent share requests after driver restart.
type MountMeta struct {
	VolumeID                 string   `json:"volumeID"`
	MountOptions             []string `json:"mountOptions"`
	AuthenticationSource     string   `json:"authenticationSource"`
	ServiceAccountName       string   `json:"serviceAccountName"`
	ServiceAccountEKSRoleARN string   `json:"serviceAccountEKSRoleARN,omitempty"`
	PodNamespace             string   `json:"podNamespace"`
	FSGroup                  string   `json:"fsGroup"`
}

// WriteMeta atomically writes the .meta.json file for the given volume.
// Uses temp file + os.Rename for atomicity (rename is atomic on Linux).
func WriteMeta(kubeletPath string, entry *MountEntry) error {
	meta := MountMeta{
		VolumeID:                 entry.VolumeID,
		MountOptions:             entry.Params.MountOptions,
		AuthenticationSource:     entry.Params.AuthenticationSource,
		ServiceAccountName:       entry.Params.ServiceAccountName,
		ServiceAccountEKSRoleARN: entry.Params.ServiceAccountEKSRoleARN,
		PodNamespace:             entry.Params.PodNamespace,
		FSGroup:                  entry.Params.FSGroup,
	}

	data, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("failed to marshal mount meta for volume %s: %w", entry.VolumeID, err)
	}

	metaPath := MetaFileName(kubeletPath, entry.VolumeID)
	if err := os.MkdirAll(filepath.Dir(metaPath), 0750); err != nil {
		return fmt.Errorf("failed to create meta directory: %w", err)
	}

	tmpPath := metaPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0600); err != nil {
		return fmt.Errorf("failed to write temp meta file: %w", err)
	}

	if err := os.Rename(tmpPath, metaPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("failed to rename meta file: %w", err)
	}

	klog.V(4).Infof("MountMap: wrote meta for volume %s at %s", entry.VolumeID, metaPath)
	return nil
}

// RemoveMeta removes the .meta.json file for the given volume.
// Called when the last consumer disconnects and the source mount is torn down.
func RemoveMeta(kubeletPath, volumeID string) {
	metaPath := MetaFileName(kubeletPath, volumeID)
	if err := os.Remove(metaPath); err != nil && !os.IsNotExist(err) {
		klog.Warningf("MountMap: failed to remove meta file %s: %v", metaPath, err)
	}
}

// readMeta reads and parses a .meta.json file.
func readMeta(path string) (*MountMeta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var meta MountMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("failed to unmarshal %s: %w", path, err)
	}
	return &meta, nil
}

// mountInfoEntry represents a single line from /proc/self/mountinfo.
type mountInfoEntry struct {
	MountPoint string // the mount point path
	DeviceID   string // major:minor device ID
}

// parseMountInfoFromProc reads /proc/self/mountinfo and returns all entries.
// This is the default implementation used in production (Linux).
func parseMountInfoFromProc() ([]mountInfoEntry, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var entries []mountInfoEntry
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		// mountinfo format: mountID parentID major:minor root mountPoint ...
		if len(fields) < 5 {
			continue
		}
		entries = append(entries, mountInfoEntry{
			DeviceID:   fields[2], // major:minor
			MountPoint: fields[4], // mount point
		})
	}
	return entries, scanner.Err()
}

// findMountByPath finds the mountinfo entry for a given mount path.
func findMountByPath(entries []mountInfoEntry, path string) *mountInfoEntry {
	for i := range entries {
		if entries[i].MountPoint == path {
			return &entries[i]
		}
	}
	return nil
}

// findBindMountTargets finds all mount points sharing the same device ID as the source,
// excluding the source path itself. These are the bind mount targets.
func findBindMountTargets(entries []mountInfoEntry, deviceID, sourcePath string) []string {
	var targets []string
	for _, e := range entries {
		if e.DeviceID == deviceID && e.MountPoint != sourcePath {
			targets = append(targets, e.MountPoint)
		}
	}
	return targets
}
