package backend

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const spotifyAPIBase = "https://api.spotify.com/v1"

var spotifyLibraryClient = &http.Client{Timeout: 30 * time.Second}

// LibraryPlaylist describes a playlist in the user's library.
type LibraryPlaylist struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	URL        string `json:"url"`
	TrackCount int    `json:"track_count"`
	Owner      string `json:"owner"`
}

// LibraryTrack describes a single track resolved from the user's library.
type LibraryTrack struct {
	SpotifyID    string `json:"spotify_id"`
	Name         string `json:"name"`
	Artists      string `json:"artists"`
	AlbumName    string `json:"album_name"`
	AlbumArtist  string `json:"album_artist"`
	ExternalURL  string `json:"external_urls"`
	CoverURL     string `json:"cover_url"`
	ISRC         string `json:"isrc"`
	DurationMS   int    `json:"duration_ms"`
	TrackNumber  int    `json:"track_number"`
	DiscNumber   int    `json:"disc_number"`
	ReleaseDate  string `json:"release_date"`
	PlaylistName string `json:"playlist_name"`
}

// --- raw Spotify API response shapes ---

type spotifyImage struct {
	URL    string `json:"url"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

type spotifyArtist struct {
	Name string `json:"name"`
}

type spotifyAlbum struct {
	Name        string          `json:"name"`
	Artists     []spotifyArtist `json:"artists"`
	Images      []spotifyImage  `json:"images"`
	ReleaseDate string          `json:"release_date"`
}

type spotifyExternalIDs struct {
	ISRC string `json:"isrc"`
}

type spotifyExternalURLs struct {
	Spotify string `json:"spotify"`
}

type spotifyTrack struct {
	ID           string              `json:"id"`
	Name         string              `json:"name"`
	Artists      []spotifyArtist     `json:"artists"`
	Album        spotifyAlbum        `json:"album"`
	ExternalIDs  spotifyExternalIDs  `json:"external_ids"`
	ExternalURLs spotifyExternalURLs `json:"external_urls"`
	DurationMS   int                 `json:"duration_ms"`
	TrackNumber  int                 `json:"track_number"`
	DiscNumber   int                 `json:"disc_number"`
	IsLocal      bool                `json:"is_local"`
	Type         string              `json:"type"`
}

type spotifyPlaylistPage struct {
	Items []struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Owner struct {
			DisplayName string `json:"display_name"`
			ID          string `json:"id"`
		} `json:"owner"`
		Tracks struct {
			Total int `json:"total"`
		} `json:"tracks"`
		ExternalURLs spotifyExternalURLs `json:"external_urls"`
	} `json:"items"`
	Next string `json:"next"`
}

type spotifySavedTracksPage struct {
	Items []struct {
		Track *spotifyTrack `json:"track"`
	} `json:"items"`
	Next string `json:"next"`
}

type spotifyPlaylistTracksPage struct {
	Items []struct {
		Track   *spotifyTrack `json:"track"`
		IsLocal bool          `json:"is_local"`
	} `json:"items"`
	Next string `json:"next"`
}

// spotifyAPIGet performs an authenticated GET and decodes the JSON body.
func spotifyAPIGet(rawURL string, out interface{}) error {
	token, err := SpotifyAccessToken()
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := spotifyLibraryClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("spotify API request failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	return json.Unmarshal(body, out)
}

func joinArtistNames(artists []spotifyArtist) string {
	names := make([]string, 0, len(artists))
	for _, artist := range artists {
		if name := strings.TrimSpace(artist.Name); name != "" {
			names = append(names, name)
		}
	}
	return strings.Join(names, ", ")
}

func firstImageURL(images []spotifyImage) string {
	if len(images) > 0 {
		return images[0].URL
	}
	return ""
}

func trackToLibraryTrack(track *spotifyTrack, playlistName string) LibraryTrack {
	return LibraryTrack{
		SpotifyID:    track.ID,
		Name:         track.Name,
		Artists:      joinArtistNames(track.Artists),
		AlbumName:    track.Album.Name,
		AlbumArtist:  joinArtistNames(track.Album.Artists),
		ExternalURL:  track.ExternalURLs.Spotify,
		CoverURL:     firstImageURL(track.Album.Images),
		ISRC:         track.ExternalIDs.ISRC,
		DurationMS:   track.DurationMS,
		TrackNumber:  track.TrackNumber,
		DiscNumber:   track.DiscNumber,
		ReleaseDate:  track.Album.ReleaseDate,
		PlaylistName: playlistName,
	}
}

// FetchUserPlaylists returns all playlists in the user's library.
func FetchUserPlaylists() ([]LibraryPlaylist, error) {
	playlists := make([]LibraryPlaylist, 0)
	next := spotifyAPIBase + "/me/playlists?limit=50"

	for next != "" {
		var page spotifyPlaylistPage
		if err := spotifyAPIGet(next, &page); err != nil {
			return nil, err
		}

		for _, item := range page.Items {
			if item.ID == "" {
				continue
			}
			owner := item.Owner.DisplayName
			if owner == "" {
				owner = item.Owner.ID
			}
			url := item.ExternalURLs.Spotify
			if url == "" {
				url = "https://open.spotify.com/playlist/" + item.ID
			}
			playlists = append(playlists, LibraryPlaylist{
				ID:         item.ID,
				Name:       item.Name,
				URL:        url,
				TrackCount: item.Tracks.Total,
				Owner:      owner,
			})
		}

		next = page.Next
	}

	return playlists, nil
}

// FetchLikedTracks returns the user's Liked Songs.
func FetchLikedTracks() ([]LibraryTrack, error) {
	tracks := make([]LibraryTrack, 0)
	next := spotifyAPIBase + "/me/tracks?limit=50"

	for next != "" {
		var page spotifySavedTracksPage
		if err := spotifyAPIGet(next, &page); err != nil {
			return nil, err
		}

		for _, item := range page.Items {
			if item.Track == nil || item.Track.ID == "" || item.Track.IsLocal {
				continue
			}
			tracks = append(tracks, trackToLibraryTrack(item.Track, "Liked Songs"))
		}

		next = page.Next
	}

	return tracks, nil
}

// FetchPlaylistTracks returns all non-local tracks in a playlist.
func FetchPlaylistTracks(playlistID, playlistName string) ([]LibraryTrack, error) {
	tracks := make([]LibraryTrack, 0)
	next := spotifyAPIBase + "/playlists/" + playlistID + "/tracks?limit=100"

	for next != "" {
		var page spotifyPlaylistTracksPage
		if err := spotifyAPIGet(next, &page); err != nil {
			return nil, err
		}

		for _, item := range page.Items {
			if item.Track == nil || item.Track.ID == "" || item.IsLocal || item.Track.IsLocal {
				continue
			}
			tracks = append(tracks, trackToLibraryTrack(item.Track, playlistName))
		}

		next = page.Next
	}

	return tracks, nil
}
