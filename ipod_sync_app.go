package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/afkarxyz/SpotiFLAC/backend"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// IpodSyncSettings is the persisted configuration for library -> iPod sync.
// JSON tags match the shape the frontend (IpodSyncPage) reads/writes.
type IpodSyncSettings struct {
	AutoSyncOnLaunch    bool     `json:"autoSyncOnLaunch"`
	IncludeLikedSongs   bool     `json:"includeLikedSongs"`
	SelectedPlaylistIDs []string `json:"selectedPlaylistIds"`
}

// IpodSyncResult is the summary returned when a sync run finishes.
type IpodSyncResult struct {
	Synced  int    `json:"synced"`
	Skipped int    `json:"skipped"`
	Failed  int    `json:"failed"`
	Total   int    `json:"total"`
	Message string `json:"message"`
}

var (
	ipodSyncRunning atomic.Bool
	ipodSyncCancel  atomic.Bool
)

func ipodSyncSettingsPath() (string, error) {
	dir, err := backend.EnsureAppDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "ipod_sync_settings.json"), nil
}

// GetIpodSyncSettings returns persisted sync settings (with safe defaults).
func (a *App) GetIpodSyncSettings() (IpodSyncSettings, error) {
	settings := IpodSyncSettings{SelectedPlaylistIDs: []string{}}

	path, err := ipodSyncSettingsPath()
	if err != nil {
		return settings, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return settings, nil
		}
		return settings, err
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return IpodSyncSettings{SelectedPlaylistIDs: []string{}}, err
	}
	if settings.SelectedPlaylistIDs == nil {
		settings.SelectedPlaylistIDs = []string{}
	}
	return settings, nil
}

// SaveIpodSyncSettings persists sync settings to disk.
func (a *App) SaveIpodSyncSettings(settings IpodSyncSettings) error {
	if settings.SelectedPlaylistIDs == nil {
		settings.SelectedPlaylistIDs = []string{}
	}
	path, err := ipodSyncSettingsPath()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// --- Spotify OAuth bindings ---

// SaveSpotifyCredentials stores the user's Spotify app Client ID/Secret.
func (a *App) SaveSpotifyCredentials(clientID, clientSecret string) error {
	return backend.SaveSpotifyCredentials(clientID, clientSecret)
}

// GetSpotifyCredentials returns saved credentials and connection state.
func (a *App) GetSpotifyCredentials() (map[string]interface{}, error) {
	clientID, clientSecret, connected, err := backend.GetSpotifyCredentials()
	return map[string]interface{}{
		"clientId":         clientID,
		"clientSecret":     clientSecret,
		"connected":        connected,
		"grantedScopes":    backend.SpotifyGrantedScopes(),
		"privatePlaylists": backend.SpotifyHasPrivatePlaylistAccess(),
	}, err
}

// ConnectSpotify starts the OAuth loopback flow and opens the auth URL in the
// user's browser. Returns the auth URL so the frontend can also surface it.
func (a *App) ConnectSpotify() (string, error) {
	authURL, err := backend.BeginSpotifyAuth()
	if err != nil {
		return "", err
	}
	if a.ctx != nil {
		runtime.BrowserOpenURL(a.ctx, authURL)
	}
	return authURL, nil
}

// SpotifyConnectionStatus reports whether we hold a usable refresh token.
func (a *App) SpotifyConnectionStatus() bool {
	return backend.SpotifyIsConnected()
}

// DisconnectSpotify clears stored tokens (keeps client credentials).
func (a *App) DisconnectSpotify() error {
	return backend.DisconnectSpotify()
}

// ListSpotifyPlaylists returns the connected user's playlists.
func (a *App) ListSpotifyPlaylists() ([]backend.LibraryPlaylist, error) {
	return backend.FetchUserPlaylists()
}

// --- iPod bindings ---

// DetectIpod returns the best-guess connected (Rockbox) iPod volume.
func (a *App) DetectIpod() (backend.IpodDevice, error) {
	return backend.DetectIpod()
}

// CancelIpodSync requests that an in-progress sync stop after the current track.
func (a *App) CancelIpodSync() error {
	ipodSyncCancel.Store(true)
	return nil
}

// IpodReorganizeResult summarizes a one-time reorganize pass.
type IpodReorganizeResult struct {
	Moved   int    `json:"moved"`
	InPlace int    `json:"in_place"`
	Failed  int    `json:"failed"`
	Total   int    `json:"total"`
	Message string `json:"message"`
}

// ReorganizeIpodLibrary moves every audio file already on the iPod into the
// sorted <Artist>/<Album>/<NN Title> layout, then removes emptied folders. This
// is a one-time cleanup for files placed by older syncs or added manually.
func (a *App) ReorganizeIpodLibrary() (IpodReorganizeResult, error) {
	result := IpodReorganizeResult{}

	if !ipodSyncRunning.CompareAndSwap(false, true) {
		return result, fmt.Errorf("a sync or reorganize is already in progress")
	}
	defer ipodSyncRunning.Store(false)
	ipodSyncCancel.Store(false)

	dev, err := backend.DetectIpod()
	if err != nil {
		return result, fmt.Errorf("iPod not detected: %w", err)
	}

	a.emitSyncStatus("Scanning iPod files…")
	files, err := backend.ListIpodAudioFiles(dev)
	if err != nil {
		return result, fmt.Errorf("failed to list iPod files: %w", err)
	}
	result.Total = len(files)
	if result.Total == 0 {
		result.Message = "No audio files on the iPod to reorganize."
		a.emitSyncStatus(result.Message)
		return result, nil
	}
	a.emitSyncLog(fmt.Sprintf("Reorganizing %d files into Artist/Album/Track…", result.Total))

	for i, file := range files {
		if ipodSyncCancel.Load() {
			result.Message = fmt.Sprintf("Cancelled: %d moved, %d already sorted, %d failed of %d", result.Moved, result.InPlace, result.Failed, result.Total)
			a.emitSyncStatus(result.Message)
			backend.RemoveEmptyDirsUnder(dev.MusicPath)
			return result, nil
		}
		a.emitSyncProgress(int(float64(i) / float64(result.Total) * 100))

		meta, merr := backend.ExtractFullMetadataFromFile(file)
		if merr != nil {
			result.Failed++
			a.emitSyncLog("✗ " + filepath.Base(file) + ": couldn't read tags")
			continue
		}

		track := backend.LibraryTrack{
			Artists:     meta.Artist,
			AlbumArtist: meta.AlbumArtist,
			AlbumName:   meta.Album,
			Name:        meta.Title,
			TrackNumber: meta.TrackNumber,
			DiscNumber:  meta.DiscNumber,
			ISRC:        meta.ISRC,
		}
		relDir, destName := a.ipodDestination(track, file)

		moved, dest, mErr := backend.MoveTrackOnIpod(dev, file, relDir, destName)
		if mErr != nil {
			result.Failed++
			a.emitSyncLog("✗ " + filepath.Base(file) + ": " + mErr.Error())
			continue
		}
		if moved {
			result.Moved++
			a.emitSyncStatus(fmt.Sprintf("(%d/%d) %s", i+1, result.Total, filepath.Base(dest)))
		} else {
			result.InPlace++
		}
	}

	backend.RemoveEmptyDirsUnder(dev.MusicPath)
	a.emitSyncProgress(100)
	result.Message = fmt.Sprintf("Done: %d moved, %d already sorted, %d failed of %d", result.Moved, result.InPlace, result.Failed, result.Total)
	a.emitSyncStatus(result.Message)
	a.emitSyncLog(result.Message)
	return result, nil
}

// --- Orchestrator ---

func (a *App) emitSyncStatus(status string) {
	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, "ipod-sync:status", status)
	}
}

func (a *App) emitSyncLog(line string) {
	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, "ipod-sync:log", line)
	}
}

func (a *App) emitSyncProgress(percent int) {
	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, "ipod-sync:progress", percent)
	}
}

// SyncLibraryToIpod downloads the selected Spotify library as FLAC (reusing the
// existing download pipeline with provider fallback) and copies new tracks onto
// the mounted Rockbox iPod. Progress is streamed via ipod-sync:* events.
func (a *App) SyncLibraryToIpod() (IpodSyncResult, error) {
	result := IpodSyncResult{}

	if !ipodSyncRunning.CompareAndSwap(false, true) {
		return result, fmt.Errorf("a sync is already in progress")
	}
	defer ipodSyncRunning.Store(false)
	ipodSyncCancel.Store(false)

	if !backend.SpotifyIsConnected() {
		return result, fmt.Errorf("spotify is not connected")
	}

	dev, err := backend.DetectIpod()
	if err != nil {
		return result, fmt.Errorf("iPod not detected: %w", err)
	}
	a.emitSyncLog(fmt.Sprintf("iPod detected: %s (%s, Rockbox: %v)", dev.Name, dev.MountPath, dev.IsRockbox))

	// Index the songs already on the iPod so we can skip ones physically present
	// (by ISRC, or artist+title), even if our manifest doesn't know about them.
	a.emitSyncStatus("Scanning songs already on iPod…")
	ipodIndex, scanErr := backend.ScanIpodLibrary(dev)
	if scanErr != nil {
		a.emitSyncLog("Warning: iPod library scan failed: " + scanErr.Error())
	}
	a.emitSyncLog(fmt.Sprintf("iPod library: %d existing songs indexed", ipodIndex.FileCount()))

	syncSettings, _ := a.GetIpodSyncSettings()

	// Build the full track list from Liked Songs + selected playlists.
	a.emitSyncStatus("Fetching your Spotify library…")
	var tracks []backend.LibraryTrack

	if syncSettings.IncludeLikedSongs {
		liked, err := backend.FetchLikedTracks()
		if err != nil {
			a.emitSyncLog("Failed to fetch Liked Songs: " + err.Error())
		} else {
			tracks = append(tracks, liked...)
			a.emitSyncLog(fmt.Sprintf("Liked Songs: %d tracks", len(liked)))
		}
	}

	if len(syncSettings.SelectedPlaylistIDs) > 0 {
		all, err := backend.FetchUserPlaylists()
		if err != nil {
			a.emitSyncLog("Failed to list playlists: " + err.Error())
		} else {
			byID := make(map[string]backend.LibraryPlaylist, len(all))
			for _, p := range all {
				byID[p.ID] = p
			}
			for _, id := range syncSettings.SelectedPlaylistIDs {
				if ipodSyncCancel.Load() {
					break
				}
				p, ok := byID[id]
				if !ok {
					continue
				}
				pt, err := backend.FetchPlaylistTracks(p.ID, p.Name)
				if err != nil {
					a.emitSyncLog(fmt.Sprintf("Failed playlist %q: %s", p.Name, err.Error()))
					continue
				}
				tracks = append(tracks, pt...)
				a.emitSyncLog(fmt.Sprintf("Playlist %q: %d tracks", p.Name, len(pt)))
			}
		}
	}

	result.Total = len(tracks)
	if result.Total == 0 {
		return result, fmt.Errorf("nothing selected to sync (enable Liked Songs or pick playlists)")
	}

	manifest, err := backend.LoadSyncManifest()
	if err != nil {
		return result, fmt.Errorf("failed to load sync manifest: %w", err)
	}

	appSettings, _ := a.LoadSettings()
	services := resolveServiceOrder(appSettings)
	stagingBase := ipodStagingDir(appSettings)
	tidalAPI, qobuzAPI := ipodCustomAPIs(appSettings)
	is24Bit := true
	if appSettings != nil {
		if q, ok := appSettings["autoQuality"].(string); ok && q != "" {
			is24Bit = q == "24"
		}
	}
	a.emitSyncLog(fmt.Sprintf("Providers: %s | staging: %s", strings.Join(services, " → "), stagingBase))

	for i, t := range tracks {
		if ipodSyncCancel.Load() {
			_ = manifest.Save()
			result.Message = fmt.Sprintf("Cancelled: %d synced, %d skipped, %d failed of %d", result.Synced, result.Skipped, result.Failed, result.Total)
			a.emitSyncStatus(result.Message)
			return result, nil
		}

		label := strings.TrimSpace(fmt.Sprintf("%s - %s", t.Name, t.Artists))
		a.emitSyncStatus(fmt.Sprintf("(%d/%d) %s", i+1, result.Total, label))
		a.emitSyncProgress(int(float64(i) / float64(result.Total) * 100))

		// Skip songs already physically on the iPod (matched by ISRC, else by
		// artist+title), and record them so the manifest stays in sync.
		if existingPath, found := ipodIndex.Match(t.ISRC, t.Artists, t.Name); found {
			result.Skipped++
			if t.SpotifyID != "" {
				var size int64
				if info, e := os.Stat(existingPath); e == nil {
					size = info.Size()
				}
				manifest.Add(t.SpotifyID, existingPath, size)
			}
			continue
		}

		if t.SpotifyID != "" {
			// Skip only when the track is genuinely present on the iPod, not
			// merely recorded in our manifest. If it was deleted from the device
			// (or the manifest came from a different iPod), drop the stale entry
			// and re-sync it.
			if manifest.ExistsOnDevice(t.SpotifyID, dev.MusicPath) {
				result.Skipped++
				continue
			}
			if manifest.Has(t.SpotifyID) {
				manifest.Remove(t.SpotifyID)
			}
		}

		flacPath, derr := a.downloadLibraryTrackWithCooldown(t, services, stagingBase, tidalAPI, qobuzAPI, is24Bit)
		if derr != nil || flacPath == "" {
			if ipodSyncCancel.Load() {
				_ = manifest.Save()
				result.Message = fmt.Sprintf("Cancelled: %d synced, %d skipped, %d failed of %d", result.Synced, result.Skipped, result.Failed, result.Total)
				a.emitSyncStatus(result.Message)
				return result, nil
			}
			result.Failed++
			a.emitSyncLog("✗ " + label + ": " + errText(derr))
			continue
		}

		relDir, destName := a.ipodDestination(t, flacPath)
		copied, dest, cerr := backend.CopyTrackToIpod(dev, flacPath, relDir, destName)
		if cerr != nil {
			result.Failed++
			a.emitSyncLog("✗ copy " + label + ": " + cerr.Error())
			continue
		}

		var size int64
		if info, e := os.Stat(dest); e == nil {
			size = info.Size()
		}
		if t.SpotifyID != "" {
			manifest.Add(t.SpotifyID, dest, size)
		}
		ipodIndex.Add(t.ISRC, t.Artists, t.Name, dest)
		if copied {
			a.emitSyncLog("✓ " + label)
		} else {
			a.emitSyncLog("• already on iPod: " + label)
		}
		result.Synced++

		if i%25 == 0 {
			_ = manifest.Save()
		}
	}

	if err := manifest.Save(); err != nil {
		a.emitSyncLog("Warning: failed to persist sync manifest: " + err.Error())
	}
	a.emitSyncProgress(100)
	result.Message = fmt.Sprintf("Done: %d synced, %d skipped, %d failed of %d", result.Synced, result.Skipped, result.Failed, result.Total)
	a.emitSyncStatus(result.Message)
	a.emitSyncLog(result.Message)
	return result, nil
}

// maxCooldownWaitsPerTrack caps how many times a single track will wait out a
// community "scheduled break" before giving up on it.
const maxCooldownWaitsPerTrack = 6

// waitOutCommunityCooldown blocks until the community API cooldown has elapsed,
// emitting a countdown status. Returns false if the sync was cancelled while
// waiting.
func (a *App) waitOutCommunityCooldown() bool {
	announced := false
	for {
		if ipodSyncCancel.Load() {
			return false
		}
		remaining := backend.CommunityCooldownRemaining()
		if remaining <= 0 {
			return true
		}
		if !announced {
			a.emitSyncLog(fmt.Sprintf("Server on a scheduled break — waiting ~%ds before continuing…", int(remaining.Seconds())+1))
			announced = true
		}
		a.emitSyncStatus(fmt.Sprintf("Server on a break — resuming in ~%ds…", int(remaining.Seconds())+1))

		step := time.Second
		if remaining < step {
			step = remaining
		}
		time.Sleep(step)
	}
}

// downloadLibraryTrackWithCooldown downloads a track, transparently waiting out
// any community "scheduled break" cooldown and retrying so a cooldown doesn't
// turn every remaining track into a failure.
func (a *App) downloadLibraryTrackWithCooldown(t backend.LibraryTrack, services []string, stagingBase, tidalAPI, qobuzAPI string, is24Bit bool) (string, error) {
	var lastErr error
	for waits := 0; waits <= maxCooldownWaitsPerTrack; waits++ {
		if !a.waitOutCommunityCooldown() {
			return "", fmt.Errorf("cancelled")
		}

		flacPath, err := a.downloadLibraryTrack(t, services, stagingBase, tidalAPI, qobuzAPI, is24Bit)
		if err == nil && flacPath != "" {
			return flacPath, nil
		}
		lastErr = err

		// If the failure triggered a fresh cooldown, loop to wait it out and
		// retry this same track instead of failing it.
		if backend.CommunityCooldownRemaining() > 0 {
			continue
		}
		break
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("download failed")
	}
	return "", lastErr
}

// downloadLibraryTrack mirrors the app's playlist-download flow to avoid pegging
// the community API: it resolves the track's streaming URLs once via Songlink,
// then only attempts providers that actually have the track (passing the
// resolved URL) with no internal fallback. This keeps requests-per-track low.
func (a *App) downloadLibraryTrack(t backend.LibraryTrack, services []string, stagingBase, tidalAPI, qobuzAPI string, is24Bit bool) (string, error) {
	// Resolve availability once, like the app does, instead of blindly hitting
	// every provider. Only needed when Tidal/Amazon are in the order.
	var urls *backend.SongLinkURLs
	if t.SpotifyID != "" && needsStreamingURLs(services) {
		client := backend.NewSongLinkClient()
		if resolved, err := client.GetAllURLsFromSpotify(t.SpotifyID, ""); err == nil {
			urls = resolved
		}
	}

	var lastErr error
	tried := false
	for _, svc := range services {
		if ipodSyncCancel.Load() {
			return "", fmt.Errorf("cancelled")
		}

		req := DownloadRequest{
			SpotifyID:     t.SpotifyID,
			TrackName:     t.Name,
			ArtistName:    t.Artists,
			AlbumName:     t.AlbumName,
			AlbumArtist:   t.AlbumArtist,
			ReleaseDate:   t.ReleaseDate,
			CoverURL:      t.CoverURL,
			ISRC:          t.ISRC,
			PlaylistName:  t.PlaylistName,
			OutputDir:     stagingBase,
			AllowFallback: false,
			TidalAPIURL:   tidalAPI,
			QobuzAPIURL:   qobuzAPI,
		}

		switch svc {
		case "tidal":
			if urls == nil || urls.TidalURL == "" {
				continue // Tidal doesn't have it; skip without a request.
			}
			req.Service = "tidal"
			req.ServiceURL = urls.TidalURL
			req.AudioFormat = "LOSSLESS"
		case "amazon":
			if urls == nil || urls.AmazonURL == "" {
				continue
			}
			req.Service = "amazon"
			req.ServiceURL = urls.AmazonURL
			req.AudioFormat = amazonQuality(is24Bit)
		case "qobuz":
			req.Service = "qobuz"
			req.AudioFormat = qobuzQuality(is24Bit)
		default:
			continue
		}

		tried = true
		resp, err := a.DownloadTrack(req)
		if err == nil && resp.Success {
			return resp.File, nil
		}
		switch {
		case err != nil:
			lastErr = err
		case resp.Error != "":
			lastErr = fmt.Errorf("%s", resp.Error)
		default:
			lastErr = fmt.Errorf("provider %s failed", svc)
		}

		// If the server put us on a cooldown, stop hammering other providers;
		// the caller waits it out and retries.
		if backend.CommunityCooldownRemaining() > 0 {
			break
		}
	}

	if !tried {
		return "", fmt.Errorf("no provider had this track")
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("all providers failed")
	}
	return "", lastErr
}

// ipodDestination builds the sorted on-iPod location for a track:
// <Artist>/<Album>/<NN Title>.<ext>, so browsing the iPod is ordered by artist,
// then album, then track number.
func (a *App) ipodDestination(t backend.LibraryTrack, srcPath string) (relDir, destName string) {
	artist := a.getFirstArtist(strings.TrimSpace(t.AlbumArtist))
	if artist == "" {
		artist = a.getFirstArtist(strings.TrimSpace(t.Artists))
	}
	if artist == "" {
		artist = "Unknown Artist"
	}
	album := strings.TrimSpace(t.AlbumName)
	if album == "" {
		album = "Unknown Album"
	}
	relDir = artist + "/" + album

	title := strings.TrimSpace(t.Name)
	if title == "" {
		title = strings.TrimSuffix(filepath.Base(srcPath), filepath.Ext(srcPath))
	}
	prefix := ""
	if t.TrackNumber > 0 {
		if t.DiscNumber > 1 {
			prefix = fmt.Sprintf("%d-%02d ", t.DiscNumber, t.TrackNumber)
		} else {
			prefix = fmt.Sprintf("%02d ", t.TrackNumber)
		}
	}
	ext := filepath.Ext(srcPath)
	if ext == "" {
		ext = ".flac"
	}
	destName = prefix + title + ext
	return relDir, destName
}

func needsStreamingURLs(services []string) bool {
	for _, s := range services {
		if s == "tidal" || s == "amazon" {
			return true
		}
	}
	return false
}

func amazonQuality(is24Bit bool) string {
	if is24Bit {
		return "24"
	}
	return "16"
}

func qobuzQuality(is24Bit bool) string {
	if is24Bit {
		return "27"
	}
	return "6"
}

// maybeAutoSyncIpod runs at startup and triggers a sync when the user has opted
// in and both Spotify and an iPod are available.
func (a *App) maybeAutoSyncIpod() {
	time.Sleep(3 * time.Second)

	settings, err := a.GetIpodSyncSettings()
	if err != nil || !settings.AutoSyncOnLaunch {
		return
	}
	if !backend.SpotifyIsConnected() {
		return
	}
	if _, err := backend.DetectIpod(); err != nil {
		return
	}
	a.emitSyncLog("Auto-sync on launch: starting…")
	if _, err := a.SyncLibraryToIpod(); err != nil {
		a.emitSyncLog("Auto-sync failed: " + err.Error())
	}
}

// resolveServiceOrder maps the app's downloader setting to an ordered provider
// list, mirroring the frontend's auto-fallback behaviour.
func resolveServiceOrder(settings map[string]interface{}) []string {
	downloader := "auto"
	if settings != nil {
		if d, ok := settings["downloader"].(string); ok && d != "" {
			downloader = d
		}
	}
	switch downloader {
	case "tidal", "qobuz", "amazon":
		return []string{downloader}
	}

	order := "tidal-qobuz-amazon"
	if settings != nil {
		if o, ok := settings["autoOrder"].(string); ok && strings.TrimSpace(o) != "" {
			order = o
		}
	}
	var out []string
	for _, part := range strings.Split(order, "-") {
		part = strings.TrimSpace(part)
		if part == "tidal" || part == "qobuz" || part == "amazon" {
			out = append(out, part)
		}
	}
	if len(out) == 0 {
		return []string{"tidal", "qobuz", "amazon"}
	}
	return out
}

func ipodStagingDir(settings map[string]interface{}) string {
	if settings != nil {
		if p, ok := settings["downloadPath"].(string); ok && strings.TrimSpace(p) != "" {
			return p
		}
	}
	return backend.GetDefaultMusicPath()
}

func ipodCustomAPIs(settings map[string]interface{}) (tidal string, qobuz string) {
	if settings == nil {
		return "", ""
	}
	if v, ok := settings["customTidalApi"].(string); ok {
		v = strings.TrimRight(strings.TrimSpace(v), "/")
		if strings.HasPrefix(v, "https://") {
			tidal = v
		}
	}
	if v, ok := settings["customQobuzApi"].(string); ok {
		v = strings.TrimRight(strings.TrimSpace(v), "/")
		if strings.HasPrefix(v, "https://") {
			qobuz = v
		}
	}
	return tidal, qobuz
}

func errText(err error) string {
	if err == nil {
		return "unknown error"
	}
	return err.Error()
}
