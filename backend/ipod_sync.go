package backend

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

type IpodDevice struct {
	Name       string `json:"name"`
	MountPath  string `json:"mount_path"` // e.g. /Volumes/IPOD
	MusicPath  string `json:"music_path"` // <MountPath>/Music
	Connected  bool   `json:"connected"`
	IsRockbox  bool   `json:"is_rockbox"` // true if a .rockbox dir exists at volume root
	FreeBytes  uint64 `json:"free_bytes"`
	TotalBytes uint64 `json:"total_bytes"`
}

const ipodSyncManifestFile = "ipod_sync_manifest.json"

// systemVolumeNames are volume names that are never an iPod.
var systemVolumeNames = map[string]struct{}{
	"Macintosh HD":          {},
	"Macintosh HD - Data":   {},
	"com.apple.TimeMachine": {},
	"Recovery":              {},
	"Preboot":               {},
	"VM":                    {},
	"Update":                {},
	"xarts":                 {},
	"iSCPreboot":            {},
	"Hardware":              {},
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

// CopyTrackToIpod copies srcFlacPath into <MusicPath>/<relDir>/<destName>,
// creating dirs. When destName is empty the source file's name is used. The
// source file's extension is always preserved. Skips (copied=false) if a
// destination file already exists with the same size. Returns final dest path.
func CopyTrackToIpod(dev IpodDevice, srcFlacPath, relDir, destName string) (copied bool, dest string, err error) {
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

	srcExt := filepath.Ext(srcFlacPath)
	base := sanitizeComponent(destName)
	if base == "" {
		base = sanitizeComponent(filepath.Base(srcFlacPath))
	}
	if base == "" {
		return false, "", errors.New("source file has no valid name")
	}
	// Always keep the source extension so the name is well-formed.
	if srcExt != "" && !strings.EqualFold(filepath.Ext(base), srcExt) {
		base = strings.TrimSuffix(base, filepath.Ext(base)) + srcExt
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

// ExistsOnDevice reports whether the track is recorded AND its file is actually
// present on the given iPod music path. This verifies against the iPod's real
// contents instead of trusting the manifest blindly, so tracks deleted from the
// device (or a manifest carried over to a different iPod) are re-synced.
func (m *SyncManifest) ExistsOnDevice(spotifyID, musicPath string) bool {
	m.mu.Lock()
	entry, ok := m.Entries[spotifyID]
	m.mu.Unlock()
	if !ok {
		return false
	}
	return manifestEntryFileExists(entry, musicPath)
}

func manifestEntryFileExists(entry SyncManifestEntry, musicPath string) bool {
	if entry.DestPath != "" {
		if info, err := os.Stat(entry.DestPath); err == nil && !info.IsDir() {
			return true
		}
	}
	// Same iPod remounted under a different volume name: look for the same
	// <relDir>/<filename> path under the current music path.
	if entry.DestPath != "" && musicPath != "" {
		rel := filepath.Join(filepath.Base(filepath.Dir(entry.DestPath)), filepath.Base(entry.DestPath))
		if info, err := os.Stat(filepath.Join(musicPath, rel)); err == nil && !info.IsDir() {
			return true
		}
	}
	return false
}

// Remove deletes a track's manifest entry (e.g. when its file is no longer on
// the device and needs to be re-synced).
func (m *SyncManifest) Remove(spotifyID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.Entries, spotifyID)
}

// ListIpodAudioFiles returns every audio file currently on the iPod.
func ListIpodAudioFiles(dev IpodDevice) ([]string, error) {
	var files []string
	root := dev.MusicPath
	if root == "" {
		return files, nil
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return files, nil
	}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if ipodAudioExts[strings.ToLower(filepath.Ext(path))] {
			files = append(files, path)
		}
		return nil
	})
	return files, err
}

// MoveTrackOnIpod moves srcPath to <MusicPath>/<relDir>/<destName> within the
// same iPod. Returns moved=false when the file is already in place or when an
// identical duplicate already exists at the target (in which case the source is
// removed). A differing file at the target name is disambiguated with a suffix.
func MoveTrackOnIpod(dev IpodDevice, srcPath, relDir, destName string) (moved bool, dest string, err error) {
	if dev.MusicPath == "" {
		return false, "", errors.New("device music path is empty")
	}
	srcInfo, err := os.Stat(srcPath)
	if err != nil {
		return false, "", fmt.Errorf("source file not accessible: %w", err)
	}

	destDir := dev.MusicPath
	if cleaned := sanitizeRelDir(relDir); cleaned != "" {
		destDir = filepath.Join(destDir, cleaned)
	}

	srcExt := filepath.Ext(srcPath)
	base := sanitizeComponent(destName)
	if base == "" {
		base = sanitizeComponent(filepath.Base(srcPath))
	}
	if base == "" {
		return false, "", errors.New("invalid destination name")
	}
	if srcExt != "" && !strings.EqualFold(filepath.Ext(base), srcExt) {
		base = strings.TrimSuffix(base, filepath.Ext(base)) + srcExt
	}
	dest = filepath.Join(destDir, base)

	if pathsEqual(dest, srcPath) {
		return false, dest, nil // already in the right place
	}

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return false, "", fmt.Errorf("failed to create destination directory: %w", err)
	}

	if destInfo, statErr := os.Stat(dest); statErr == nil {
		if !destInfo.IsDir() && destInfo.Size() == srcInfo.Size() {
			// Identical duplicate already sorted; drop the stray source.
			_ = os.Remove(srcPath)
			return false, dest, nil
		}
		dest = uniqueDestPath(destDir, base)
	}

	if err := os.Rename(srcPath, dest); err != nil {
		return false, dest, fmt.Errorf("failed to move file: %w", err)
	}
	return true, dest, nil
}

// RemoveEmptyDirsUnder deletes empty directories beneath root (but not root
// itself), e.g. leftover per-playlist folders after a reorganize.
func RemoveEmptyDirsUnder(root string) {
	if root == "" {
		return
	}
	var dirs []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() && !pathsEqual(path, root) {
			dirs = append(dirs, path)
		}
		return nil
	})
	// Deepest first so parents become empty after children are removed.
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, dir := range dirs {
		if entries, err := os.ReadDir(dir); err == nil && len(entries) == 0 {
			_ = os.Remove(dir)
		}
	}
}

func pathsEqual(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

func uniqueDestPath(dir, base string) string {
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	for i := 2; i < 1000; i++ {
		candidate := filepath.Join(dir, fmt.Sprintf("%s (%d)%s", stem, i, ext))
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
	return filepath.Join(dir, base)
}

// ipodAudioExts are the audio file extensions considered when indexing an iPod.
var ipodAudioExts = map[string]bool{
	".flac": true, ".mp3": true, ".m4a": true, ".alac": true, ".ogg": true,
	".opus": true, ".wav": true, ".aac": true, ".aiff": true, ".aif": true,
	".ape": true, ".wv": true, ".wma": true,
}

// IpodLibraryIndex indexes the audio files already present on an iPod so a sync
// can skip songs that are physically there, regardless of what our manifest
// records. Songs are matched by ISRC first, then by normalized artist+title.
type IpodLibraryIndex struct {
	byISRC map[string]string // ISRC (upper) -> file path
	byName map[string]string // "artist|title" (normalized) -> file path
	files  int
}

// FileCount reports how many audio files were scanned on the iPod.
func (idx *IpodLibraryIndex) FileCount() int {
	if idx == nil {
		return 0
	}
	return idx.files
}

// Match reports whether a song is already present on the iPod, returning the
// matching file path. ISRC is authoritative; artist+title is a fallback for
// files lacking an ISRC tag.
func (idx *IpodLibraryIndex) Match(isrc, artists, title string) (string, bool) {
	if idx == nil {
		return "", false
	}
	if code := strings.ToUpper(strings.TrimSpace(isrc)); code != "" {
		if path, ok := idx.byISRC[code]; ok {
			return path, true
		}
	}
	if key := trackMatchKey(artists, title); key != "" {
		if path, ok := idx.byName[key]; ok {
			return path, true
		}
	}
	return "", false
}

// Add records a song in the index (used for songs copied during the current
// sync so later duplicates in the same run are skipped).
func (idx *IpodLibraryIndex) Add(isrc, artists, title, path string) {
	if idx == nil {
		return
	}
	if code := strings.ToUpper(strings.TrimSpace(isrc)); code != "" {
		if _, exists := idx.byISRC[code]; !exists {
			idx.byISRC[code] = path
		}
	}
	if key := trackMatchKey(artists, title); key != "" {
		if _, exists := idx.byName[key]; !exists {
			idx.byName[key] = path
		}
	}
}

// ScanIpodLibrary walks the iPod's music folder and reads tags from every audio
// file to build a content index. Files that can't be read are skipped.
func ScanIpodLibrary(dev IpodDevice) (*IpodLibraryIndex, error) {
	idx := &IpodLibraryIndex{
		byISRC: make(map[string]string),
		byName: make(map[string]string),
	}

	root := dev.MusicPath
	if root == "" {
		return idx, nil
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return idx, nil
	}

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !ipodAudioExts[strings.ToLower(filepath.Ext(path))] {
			return nil
		}
		idx.files++

		meta, merr := extractFullMetadataWithTagLib(path)
		if merr != nil {
			return nil
		}
		if code := strings.ToUpper(strings.TrimSpace(meta.ISRC)); code != "" {
			if _, exists := idx.byISRC[code]; !exists {
				idx.byISRC[code] = path
			}
		}
		if key := trackMatchKey(meta.Artist, meta.Title); key != "" {
			if _, exists := idx.byName[key]; !exists {
				idx.byName[key] = path
			}
		}
		return nil
	})

	return idx, walkErr
}

// trackMatchKey builds a normalized "artist|title" key for fuzzy matching. It
// returns "" when there is no usable title.
func trackMatchKey(artists, title string) string {
	normTitle := normalizeMatchValue(title)
	if normTitle == "" {
		return ""
	}
	return normalizeMatchValue(firstArtistName(artists)) + "|" + normTitle
}

// normalizeMatchValue lowercases a string and strips everything but letters and
// digits so cosmetic differences (spacing, punctuation, feat. tags) don't block
// a match.
func normalizeMatchValue(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func firstArtistName(artists string) string {
	artists = strings.TrimSpace(artists)
	lower := strings.ToLower(artists)
	cut := len(artists)
	for _, sep := range []string{";", ",", " & ", " x ", " feat", " ft.", " ft "} {
		if i := strings.Index(lower, sep); i >= 0 && i < cut {
			cut = i
		}
	}
	return strings.TrimSpace(artists[:cut])
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
