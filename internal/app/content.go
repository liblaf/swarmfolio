package app

import (
	"fmt"
	"strings"

	"github.com/liblaf/swarmfolio/internal/qbittorrent"
)

// verifyContentIsolation checks that each selected torrent has a distinct
// content tree according to the paths qBittorrent reports. Callers may only
// delete files after this check succeeds: qBittorrent's delete-with-files API
// otherwise follows paths, rather than torrent identities.
func verifyContentIsolation(torrents []qbittorrent.Torrent, hashes []string) error {
	for _, hash := range hashes {
		selected := findHash(torrents, hash)
		if selected == nil {
			return fmt.Errorf("selected torrent %q disappeared before content isolation check", hash)
		}
		if !remotePathIsAbs(selected.ContentPath) {
			return fmt.Errorf("selected torrent %q has no valid absolute content path", selected.Hash)
		}
		for _, other := range torrents {
			if strings.EqualFold(other.Hash, selected.Hash) {
				continue
			}
			if !remotePathIsAbs(other.ContentPath) {
				return fmt.Errorf("cannot prove content isolation: torrent %q has no valid absolute content path", other.Hash)
			}
			if within(selected.ContentPath, other.ContentPath) || within(other.ContentPath, selected.ContentPath) {
				return fmt.Errorf("selected torrent %q content path %q overlaps torrent %q content path %q", selected.Hash, selected.ContentPath, other.Hash, other.ContentPath)
			}
		}
	}
	return nil
}
