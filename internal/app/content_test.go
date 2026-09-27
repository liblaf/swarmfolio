package app

import (
	"strings"
	"testing"

	"github.com/liblaf/swarmfolio/internal/qbittorrent"
)

func TestVerifyContentIsolation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		torrents []qbittorrent.Torrent
		hashes   []string
		wantErr  string
	}{
		{
			name: "separate unix content trees",
			torrents: []qbittorrent.Torrent{
				{Hash: "new", ContentPath: "/downloads/swarmfolio/new"},
				{Hash: "old", ContentPath: "/downloads/swarmfolio/old"},
			},
			hashes: []string{"new"},
		},
		{
			name: "equal paths collide",
			torrents: []qbittorrent.Torrent{
				{Hash: "new", ContentPath: "/downloads/shared.iso"},
				{Hash: "old", ContentPath: "/downloads/shared.iso"},
			},
			hashes:  []string{"new"},
			wantErr: "overlaps torrent \"old\"",
		},
		{
			name: "selected ancestor collides",
			torrents: []qbittorrent.Torrent{
				{Hash: "new", ContentPath: "/downloads/release"},
				{Hash: "old", ContentPath: "/downloads/release/movie.mkv"},
			},
			hashes:  []string{"new"},
			wantErr: "overlaps torrent \"old\"",
		},
		{
			name: "selected descendant collides",
			torrents: []qbittorrent.Torrent{
				{Hash: "new", ContentPath: "/downloads/release/movie.mkv"},
				{Hash: "old", ContentPath: "/downloads/release"},
			},
			hashes:  []string{"new"},
			wantErr: "overlaps torrent \"old\"",
		},
		{
			name: "windows paths are case insensitive",
			torrents: []qbittorrent.Torrent{
				{Hash: "new", ContentPath: `C:\\Downloads\\Release`},
				{Hash: "old", ContentPath: `c:/downloads/release/movie.mkv`},
			},
			hashes:  []string{"new"},
			wantErr: "overlaps torrent \"old\"",
		},
		{
			name: "selected torrents must also be disjoint",
			torrents: []qbittorrent.Torrent{
				{Hash: "first", ContentPath: "/downloads/shared"},
				{Hash: "second", ContentPath: "/downloads/shared/file"},
				{Hash: "old", ContentPath: "/downloads/old"},
			},
			hashes:  []string{"first", "second"},
			wantErr: "overlaps torrent \"second\"",
		},
		{
			name:     "missing selected torrent",
			torrents: []qbittorrent.Torrent{{Hash: "old", ContentPath: "/downloads/old"}},
			hashes:   []string{"new"},
			wantErr:  "disappeared",
		},
		{
			name:     "invalid selected content path",
			torrents: []qbittorrent.Torrent{{Hash: "new", ContentPath: "relative"}},
			hashes:   []string{"new"},
			wantErr:  "selected torrent \"new\" has no valid absolute content path",
		},
		{
			name: "unknown other content path fails closed",
			torrents: []qbittorrent.Torrent{
				{Hash: "new", ContentPath: "/downloads/new"},
				{Hash: "old", ContentPath: ""},
			},
			hashes:  []string{"new"},
			wantErr: "cannot prove content isolation",
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := verifyContentIsolation(test.torrents, test.hashes)
			if test.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("verifyContentIsolation() error = %v, want %q", err, test.wantErr)
			}
		})
	}
}
