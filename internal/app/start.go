package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/liblaf/swarmfolio/internal/qbittorrent"
)

// A successful start response only acknowledges the request. Wait until every
// requested torrent is running, queued, or complete before completing the run.
func (r Runner) waitForStarted(ctx context.Context, expected []qbittorrent.Torrent, targetPath string) error {
	pollCtx, cancel := context.WithTimeout(ctx, r.PollTimeout)
	defer cancel()
	ticker := time.NewTicker(r.PollInterval)
	defer ticker.Stop()
	for {
		current, err := r.QBittorrent.Torrents(pollCtx)
		if err != nil {
			return err
		}
		pending := ""
		for _, want := range expected {
			torrent := findHash(current, want.Hash)
			if torrent == nil {
				return fmt.Errorf("started torrent %q disappeared", want.Hash)
			}
			if torrent.Size != want.Size || torrent.Category != r.Config.QBittorrent.Category || !torrent.AutoTMM || !within(targetPath, torrent.SavePath) {
				return fmt.Errorf("started torrent %q changed size or management settings", want.Hash)
			}
			switch strings.ToLower(torrent.State) {
			case "downloading", "forceddl", "stalleddl", "queueddl":
				// Queueing and waiting for peers are managed by qBittorrent.
			case "uploading", "forcedup", "stalledup", "queuedup", "stoppedup", "pausedup":
				if torrent.Progress != 1 || torrent.AmountLeft != 0 {
					return fmt.Errorf("started torrent %q reports upload state %q before completion", want.Hash, torrent.State)
				}
			case "stoppeddl", "pauseddl", "checkingdl", "checkingup", "checkingresumedata", "allocating", "moving":
				pending = fmt.Sprintf("torrent %q remains in state %q", want.Hash, torrent.State)
			default:
				return fmt.Errorf("started torrent %q entered unexpected state %q", want.Hash, torrent.State)
			}
		}
		if pending == "" {
			return nil
		}
		select {
		case <-pollCtx.Done():
			return fmt.Errorf("wait for torrent start within %s: %s: %w", r.PollTimeout, pending, pollCtx.Err())
		case <-ticker.C:
		}
	}
}
