package backend

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

type IpodDevice struct {
	Name       string `json:"name"`
	MountPath  string `json:"mount_path"`  // e.g. /Volumes/IPOD
	MusicPath  string `json:"music_path"`  // <MountPath>/Music
	Connected  bool   `json:"connected"`
	IsRockbox  bool   `json:"is_rockbox"` // true if a .rockbox dir exists at volume root
	FreeBytes  uint64 `json:"free_bytes"`
	TotalBytes uint64 `json:"total_bytes"`
}

const ipodSyncManifestFile = "ipod_sync_manifest.json"

// systemVolumeNames are volume names that are never an iPod.
var systemVolumeNames = map[string]struct{}{
	"Macintosh HD":            {},
	"Macintosh HD - Data":     {},
	"com.apple.TimeMachine":   {},
	"Recovery":                {},
	"Preboot":                 {},
	"VM":                      {},
	"Update":                  {},
	"xarts":                   {},
	"iSCPreboot":              {},
	"Hardware":                {},
}

// DetectIpod returns the best-guess connected iPod volume.
func DetectIpod() (IpodDevice, error) {
	candidates, err := ListRemovableVolumes()
	if err != nil {
		return IpodDevice{}, err
	}
	if len(candidates) == 0 {
		return IpodDevice{}, errors.New("no removable volume found")
	}

	// Prefer a Rockbox volume.
	for _, dev := range candidates {
		if dev.IsRockbox {
			return dev, nil
		}
	}

	// Otherwise fall back to the first candidate.
	return candidates[0], nil
}

// ListRemovableVolumes returns candidate volumes so the UI can let the user pick
// if auto-detect is wrong.
func ListRemovableVolumes() ([]IpodDevice, error) {
	mountPaths := listMountCandidates()

	devices := make([]IpodDevice, 0, len(mountPaths))
	for _, mountPath := range mountPaths {
		dev, err := IpodDeviceFromPath(mountPath)
		if err != nil {
			continue
		}
		devices = append(devices, dev)
	}

	return devices, nil
}

// listMountCandidates returns mount paths that could be an external/removable
// volume, per platform. It never returns system volumes.
func listMountCandidates() []string {
	switch runtime.GOOS {
	case "darwin":
		return listMountCandidatesDarwin()
	case "windows":
		return listMountCandidatesWindows()
	default:
		return listMountCandidatesLinux()
	}
}

func listMountCandidatesDarwin() []string {
	entries, err := os.ReadDir("/Volumes")
	if err != nil {
		return nil
	}

	var paths []string
	for _, entry := range entries {
		name := entry.Name()
		if _, skip := systemVolumeNames[name]; skip {
			continue
		}

		mountPath := filepath.Join("/Volumes", name)

		// Skip a symlink that points at the boot volume ("/").
		info, err := os.Lstat(mountPath)
		if err != nil {
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := filepath.EvalSymlinks(mountPath)
			if err != nil || target == "/" {
				continue
			}
		}

		if !isDir(mountPath) {
			continue
		}
		paths = append(paths, mountPath)
	}
	return paths
}

func listMountCandidatesWindows() []string {
	var paths []string
	for letter := 'D'; letter <= 'Z'; letter++ {
		drive := string(letter) + ":\\"
		if isDir(drive) {
			paths = append(paths, drive)
		}
	}
	return paths
}

func listMountCandidatesLinux() []string {
	var roots []string
	if user := currentUsername(); user != "" {
		roots = append(roots,
			filepath.Join("/media", user),
			filepath.Join("/run/media", user),
		)
	}
	roots = append(roots, "/media", "/mnt")

	var paths []string
	seen := make(map[string]struct{})
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			mountPath := filepath.Join(root, entry.Name())
			if _, ok := seen[mountPath]; ok {
				continue
			}
			if !isDir(mountPath) {
				continue
			}
			seen[mountPath] = struct{}{}
			paths = append(paths, mountPath)
		}
	}
	return paths
}

func currentUsername() string {
	for _, key := range []string{"USER", "USERNAME", "LOGNAME"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	return ""
}

// IpodDeviceFromPath builds an IpodDevice from a user-chosen mount path.
func IpodDeviceFromPath(mountPath string) (IpodDevice, error) {
	mountPath = strings.TrimRight(mountPath, "/\\")
	if mountPath == "" {
		return IpodDevice{}, errors.New("mount path is empty")
	}

	if !isDir(mountPath) {
		return IpodDevice{}, fmt.Errorf("mount path does not exist: %s", mountPath)
	}

	free, total := volumeStats(mountPath)

	dev := IpodDevice{
		Name:       filepath.Base(mountPath),
		MountPath:  mountPath,
		MusicPath:  filepath.Join(mountPath, "Music"),
		Connected:  true,
		IsRockbox:  isDir(filepath.Join(mountPath, ".rockbox")),
		FreeBytes:  free,
		TotalBytes: total,
	}
	return dev, nil
}

// CopyTrackToIpod copies srcFlacPath into <MusicPath>/<relDir>/<basename>,
// creating dirs. Skips (copied=false) if a destination file already exists with
// the same size. Returns final dest path.
func CopyTrackToIpod(dev IpodDevice, srcFlacPath, relDir string) (copied bool, dest string, err error) {
	if dev.MusicPath == "" {
		return false, "", errors.New("device music path is empty")
	}

	srcInfo, err := os.Stat(srcFlacPath)
	if err != nil {
		return false, "", fmt.Errorf("source file not accessible: %w", err)
	}
	if srcInfo.IsDir() {
		return false, "", fmt.Errorf("source is a directory: %s", srcFlacPath)
	}

	destDir := dev.MusicPath
	if cleaned := sanitizeRelDir(relDir); cleaned != "" {
		destDir = filepath.Join(destDir, cleaned)
	}

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return false, "", fmt.Errorf("failed to create destination directory: %w", err)
	}

	base := sanitizeComponent(filepath.Base(srcFlacPath))
	if base == "" {
		return false, "", errors.New("source file has no valid name")
	}
	dest = filepath.Join(destDir, base)

	// Skip if an identically-sized file is already present.
	if destInfo, statErr := os.Stat(dest); statErr == nil {
		if !destInfo.IsDir() && destInfo.Size() == srcInfo.Size() {
			return false, dest, nil
		}
	}

	if err := copyFileBuffered(srcFlacPath, dest); err != nil {
		return false, dest, err
	}

	return true, dest, nil
}

func copyFileBuffered(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("failed to open source: %w", err)
	}
	defer in.Close()

	tmp := dest + ".tmp"
	out, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("failed to create destination: %w", err)
	}

	buf := make([]byte, 1<<20) // 1 MiB
	if _, err := io.CopyBuffer(out, in, buf); err != nil {
		out.Close()
		os.Remove(tmp)
		return fmt.Errorf("failed to copy data: %w", err)
	}

	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(tmp)
		return fmt.Errorf("failed to flush destination: %w", err)
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("failed to close destination: %w", err)
	}

	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("failed to finalize destination: %w", err)
	}
	return nil
}

// sanitizeRelDir cleans a relative directory so it can never escape MusicPath.
func sanitizeRelDir(relDir string) string {
	relDir = strings.ReplaceAll(relDir, "\\", "/")
	parts := strings.Split(relDir, "/")

	var clean []string
	for _, part := range parts {
		component := sanitizeComponent(part)
		if component == "" {
			continue
		}
		clean = append(clean, component)
	}
	return filepath.Join(clean...)
}

// sanitizeComponent strips path separators, parent references, and characters
// that are invalid on common filesystems from a single path component.
func sanitizeComponent(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return ""
	}

	invalid := []string{"<", ">", ":", "\"", "/", "\\", "|", "?", "*"}
	for _, char := range invalid {
		name = strings.ReplaceAll(name, char, "")
	}
	return strings.TrimSpace(name)
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// volumeStats reports the free and total bytes for the filesystem containing
// path. It shells out to `df` on darwin/linux (compiles on every platform) and
// returns (0, 0) where that is unavailable, e.g. Windows.
func volumeStats(path string) (free, total uint64) {
	if runtime.GOOS == "windows" {
		return 0, 0
	}

	// -k = 1024-byte blocks, -P = POSIX single-line output.
	out, err := exec.Command("df", "-k", "-P", path).Output()
	if err != nil {
		return 0, 0
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return 0, 0
	}

	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) < 4 {
		return 0, 0
	}

	const blockSize = 1024
	if blocks, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
		total = blocks * blockSize
	}
	if avail, err := strconv.ParseUint(fields[3], 10, 64); err == nil {
		free = avail * blockSize
	}
	return free, total
}

// SyncManifest tracks which tracks have already been copied to an iPod so that
// re-runs can skip them.
type SyncManifest struct {
	mu      sync.Mutex
	path    string
	Entries map[string]SyncManifestEntry `json:"entries"`
}

type SyncManifestEntry struct {
	DestPath string    `json:"dest_path"`
	Size     int64     `json:"size"`
	SyncedAt time.Time `json:"synced_at"`
}

// LoadSyncManifest reads the persisted manifest, returning an empty one if none
// exists yet.
func LoadSyncManifest() (*SyncManifest, error) {
	appDir, err := EnsureAppDir()
	if err != nil {
		return nil, err
	}

	manifestPath := filepath.Join(appDir, ipodSyncManifestFile)
	manifest := &SyncManifest{
		path:    manifestPath,
		Entries: make(map[string]SyncManifestEntry),
	}

	data, err := os.ReadFile(manifestPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return manifest, nil
		}
		return nil, err
	}

	if len(data) == 0 {
		return manifest, nil
	}

	if err := json.Unmarshal(data, manifest); err != nil {
		return nil, err
	}
	if manifest.Entries == nil {
		manifest.Entries = make(map[string]SyncManifestEntry)
	}
	manifest.path = manifestPath
	return manifest, nil
}

// Has reports whether a track with the given Spotify ID has been synced.
func (m *SyncManifest) Has(spotifyID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	_, ok := m.Entries[spotifyID]
	return ok
}

// Add records a synced track.
func (m *SyncManifest) Add(spotifyID, destPath string, size int64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Entries == nil {
		m.Entries = make(map[string]SyncManifestEntry)
	}
	m.Entries[spotifyID] = SyncManifestEntry{
		DestPath: destPath,
		Size:     size,
		SyncedAt: time.Now(),
	}
}

// Save persists the manifest to disk atomically.
func (m *SyncManifest) Save() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.path == "" {
		appDir, err := EnsureAppDir()
		if err != nil {
			return err
		}
		m.path = filepath.Join(appDir, ipodSyncManifestFile)
	}

	payload, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}

	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, payload, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, m.path)
}
