package backend

import "testing"

func TestFirstArtistName(t *testing.T) {
	cases := map[string]string{
		"Max":                            "Max", // must NOT split on the 'x'
		"Max, Other":                     "Max",
		"A & B":                          "A",
		"Drake feat. 21":                 "Drake",
		"imnotvrycreative, raftheartist": "imnotvrycreative",
		"Solo":                           "Solo",
	}
	for in, want := range cases {
		if got := firstArtistName(in); got != want {
			t.Errorf("firstArtistName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIndexMatch(t *testing.T) {
	idx := &IpodLibraryIndex{byISRC: map[string]string{}, byName: map[string]string{}}
	idx.Add("USLD91780787", "donavns", "Cherry Wine", "/Volumes/IPOD/Music/x/Cherry Wine - donavns.flac")

	// ISRC match (case-insensitive)
	if _, ok := idx.Match("usld91780787", "", ""); !ok {
		t.Error("expected ISRC match")
	}
	// name match ignoring punctuation/case, different ISRC
	if _, ok := idx.Match("XX0000000000", "Donavns", "cherry  wine!!"); !ok {
		t.Error("expected artist+title match")
	}
	// no match
	if _, ok := idx.Match("", "Someone", "Different Song"); ok {
		t.Error("did not expect a match")
	}
}
