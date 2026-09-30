package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/liblaf/swarmfolio/internal/qbittorrent"
)

// A successful start response only acknowledges the request. Wait until every
// requested torrent is running, queued, or complete before completing the run.
func (r Runner) waitForStarted(ctx context.Context, expected []qbittorrent.Torrent, targetPath string) error {
	return r.poll(ctx, "wait for torrent start", func(ctx context.Context) (error, error) {
		current, err := r.QBittorrent.Torrents(ctx)
		if err != nil {
			return nil, err
		}
		var pending error
		for _, want := range expected {
			torrent := findHash(current, want.Hash)
			if torrent == nil {
				return nil, fmt.Errorf("started torrent %q disappeared", want.Hash)
			}
			if torrent.Size != want.Size || torrent.Category != r.Config.QBittorrent.Category || !torrent.AutoTMM || !within(targetPath, torrent.SavePath) {
				return nil, fmt.Errorf("started torrent %q changed size or management settings", want.Hash)
			}
			switch strings.ToLower(torrent.State) {
			case "downloading", "forceddl", "stalleddl", "queueddl":
				// Queueing and waiting for peers are managed by qBittorrent.
			case "uploading", "forcedup", "stalledup", "queuedup", "stoppedup", "pausedup":
				if torrent.Progress != 1 || torrent.AmountLeft != 0 {
					return nil, fmt.Errorf("started torrent %q reports upload state %q before completion", want.Hash, torrent.State)
				}
			case "stoppeddl", "pauseddl", "checkingdl", "checkingup", "checkingresumedata", "allocating", "moving":
				pending = fmt.Errorf("torrent %q remains in state %q", want.Hash, torrent.State)
			default:
				return nil, fmt.Errorf("started torrent %q entered unexpected state %q", want.Hash, torrent.State)
			}
		}
		return pending, nil
	})
}
