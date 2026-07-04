package main

import (
	"testing"

	"github.com/afkarxyz/SpotiFLAC/backend"
)

func TestIpodDestination(t *testing.T) {
	a := &App{}
	cases := []struct {
		name         string
		track        backend.LibraryTrack
		src          string
		wantRelDir   string
		wantDestName string
	}{
		{
			name:         "standard",
			track:        backend.LibraryTrack{AlbumArtist: "Kendrick Lamar", Artists: "Kendrick Lamar, SZA", AlbumName: "good kid, m.A.A.d city", Name: "Sherane", TrackNumber: 1, DiscNumber: 1},
			src:          "/tmp/x/Sherane - Kendrick.flac",
			wantRelDir:   "Kendrick Lamar/good kid, m.A.A.d city",
			wantDestName: "01 Sherane.flac",
		},
		{
			name:         "dotted title keeps extension",
			track:        backend.LibraryTrack{AlbumArtist: "Kendrick Lamar", AlbumName: "GKMC", Name: "m.A.A.d city", TrackNumber: 5, DiscNumber: 1},
			src:          "/tmp/x/song.flac",
			wantRelDir:   "Kendrick Lamar/GKMC",
			wantDestName: "05 m.A.A.d city.flac",
		},
		{
			name:         "multi-disc prefix",
			track:        backend.LibraryTrack{AlbumArtist: "Artist", AlbumName: "Album", Name: "Track", TrackNumber: 3, DiscNumber: 2},
			src:          "/tmp/x/song.flac",
			wantRelDir:   "Artist/Album",
			wantDestName: "2-03 Track.flac",
		},
		{
			name:         "no album artist falls back to first artist",
			track:        backend.LibraryTrack{Artists: "Drake, 21 Savage", AlbumName: "Her Loss", Name: "Rich Flex", TrackNumber: 1},
			src:          "/tmp/x/song.flac",
			wantRelDir:   "Drake/Her Loss",
			wantDestName: "01 Rich Flex.flac",
		},
		{
			name:         "missing metadata uses fallbacks",
			track:        backend.LibraryTrack{Name: "Loose Song"},
			src:          "/tmp/x/song.flac",
			wantRelDir:   "Unknown Artist/Unknown Album",
			wantDestName: "Loose Song.flac",
		},
	}
	for _, c := range cases {
		relDir, destName := a.ipodDestination(c.track, c.src)
		if relDir != c.wantRelDir || destName != c.wantDestName {
			t.Errorf("%s: got (%q, %q), want (%q, %q)", c.name, relDir, destName, c.wantRelDir, c.wantDestName)
		}
	}
}
