package assets

import "embed"

// Files contains the user service and timer shipped inside the executable.
//
//go:embed systemd/swarmfolio.service systemd/swarmfolio.timer
var Files embed.FS
