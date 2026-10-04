package app

import (
	"testing"
	"time"

	"github.com/liblaf/swarmfolio/internal/qbittorrent"
)

// indefiniteFreeleech removes a deadline from test offers when a test covers
// another safety boundary. Timed-offer tests set their own expiry and supply
// qBittorrent transfer evidence deliberately.
func indefiniteFreeleech(mt *fakeMTeam) {
	for index := range mt.results {
		mt.results[index].DiscountEndTime = time.Time{}
	}
}

func nonTimedTestServices(t *testing.T) (*fakeQBT, *fakeMTeam) {
	t.Helper()
	qbt, mt := testServices(t)
	indefiniteFreeleech(mt)
	return qbt, mt
}

func addCompletionEvidence(qbt *fakeQBT) {
	qbt.torrents = append(qbt.torrents, qbittorrent.Torrent{
		Hash: "download-evidence", Name: "download evidence", Size: 1,
		Downloaded: 100, DownloadTime: time.Second, CompletionOn: appNow.Add(-time.Minute),
		AddedOn: appNow.Add(-time.Hour), LastActivity: appNow.Add(-time.Hour),
		SavePath: "/downloads/evidence", ContentPath: "/downloads/evidence",
		Category: "user-managed", State: "stoppedUP",
	})
}
