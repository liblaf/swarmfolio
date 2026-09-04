<div align="center" markdown>

![Swarmfolio](https://socialify.git.ci/liblaf/swarmfolio/image?description=1&forks=1&issues=1&language=1&name=1&owner=1&pattern=Transparent&pulls=1&stargazers=1&theme=Auto)

[![Made with Copier](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/copier-org/copier/master/img/badge/badge-black.json)](https://github.com/copier-org/copier)
[![Test](https://github.com/liblaf/swarmfolio/actions/workflows/test.yaml/badge.svg)](https://github.com/liblaf/swarmfolio/actions/workflows/test.yaml)
[![MegaLinter](https://github.com/liblaf/swarmfolio/actions/workflows/shared-mega-linter.yaml/badge.svg)](https://github.com/liblaf/swarmfolio/actions/workflows/shared-mega-linter.yaml)
[![Release](https://img.shields.io/github/v/release/liblaf/swarmfolio?logo=github)](https://github.com/liblaf/swarmfolio/releases/latest)

[Releases](https://github.com/liblaf/swarmfolio/releases) · [Report Bug](https://github.com/liblaf/swarmfolio/issues) · [Request Feature](https://github.com/liblaf/swarmfolio/issues)

![Rule](https://cdn.jsdelivr.net/gh/andreasbm/readme/assets/lines/rainbow.png)

</div>

Swarmfolio is a stateless, one-shot M-Team freeleech optimizer for qBittorrent. Each run reads the current qBittorrent portfolio, ranks fresh download-free torrents by leecher-to-seeder opportunity, and either uses safe headroom or replaces the least productive eligible torrent.

## ✨ Safety Model

- qBittorrent is the only persistent source of truth. Swarmfolio has no database or cache.
- The default ownership tag and category are both `swarmfolio`. A torrent must have that tag, belong to that category, and use Automatic Torrent Management before it can be deleted.
- `keep` and `archive` are protected by default; configured protected tags always win.
- Only complete, old, idle, low-activity managed torrents are eligible for replacement.
- New torrents are added stopped with a `swarmfolio-pending` tag before any old data is removed. An interrupted run is recovered from those qBittorrent tags on the next applied run.
- Applied runs use an XDG runtime lock, so two Swarmfolio processes cannot delete from the same portfolio concurrently.
- Candidate API responses, disk accounting, torrent metadata, and state changes are validated; unexpected state stops the run visibly.

## 📦 Installation

The GitHub Release contains one static Linux AMD64 executable:

```bash
install -d ~/.local/bin
gh release download --repo liblaf/swarmfolio --pattern swarmfolio --dir ~/.local/bin --clobber
chmod +x ~/.local/bin/swarmfolio
```

Alternatively, build it with Go 1.25 or newer:

```bash
go install github.com/liblaf/swarmfolio/cmd/swarmfolio@latest
```

Swarmfolio targets qBittorrent 5.0 or newer. qBittorrent 5.2 or newer can use an API key; older supported versions use WebUI username/password login.

## ⚙️ Configuration

Create `${XDG_CONFIG_HOME:-$HOME/.config}/swarmfolio/config.toml`:

```bash
swarmfolio config init
${EDITOR:-vi} "$(swarmfolio config path)"
```

The generated file documents every setting. Its important defaults are:

- `qbittorrent.managed_tag = "swarmfolio"`;
- `qbittorrent.category = "swarmfolio"`;
- no hard byte ceiling unless `portfolio.budget` is set;
- at least `portfolio.minimum_free_percent = 25` of the download disk remains free;
- at most two additions and four removals per hourly run.

The disk limit accounts for both current free space and every unfinished byte already promised to qBittorrent. For local qBittorrent, Swarmfolio probes the configured category's save path. Set `portfolio.disk_path` to the host-visible mount for a container. For a remote host, `portfolio.disk_capacity` can use qBittorrent's reported free space only when the category and default save paths are identical; otherwise run Swarmfolio where it can probe the category filesystem.

Swarmfolio manages torrents only in its required `qbittorrent.category` (default `swarmfolio`). Before running it, create that category in qBittorrent, set its desired save path, and explicitly disable the category's separate incomplete-download path. Swarmfolio enables **Automatic Torrent Management** for every torrent it adds, keeping its files separate from normal user-managed torrents while one filesystem budget accounts for every downloaded byte.

Keep credentials in the mode-`0600` config or in `${XDG_CONFIG_HOME:-$HOME/.config}/swarmfolio/environment`:

```bash
SWARMFOLIO_MTEAM_API_KEY=replace-me
SWARMFOLIO_QBITTORRENT_PASSWORD=replace-me
# qBittorrent >= 5.2 alternative:
# SWARMFOLIO_QBITTORRENT_API_KEY=qbt_replace-me
```

M-Team requires an API Access Token in `x-api-key`; create one under Control Panel → Lab → Access Token. Swarmfolio asks M-Team only for `FREE` results, also recognizes `_2X_FREE`, and skips promotions without a verifiable expiry.

Test the complete read-only path before enabling mutations:

```bash
swarmfolio plan
swarmfolio run --apply
```

Use `--json` with `plan` or `run` for machine-readable reports.

## 🐟 Fish Completion

```fish
mkdir -p "${XDG_CONFIG_HOME:-$HOME/.config}/fish/completions"
swarmfolio completion fish >"${XDG_CONFIG_HOME:-$HOME/.config}/fish/completions/swarmfolio.fish"
```

## ⏱️ Hourly User Timer

The executable embeds [`swarmfolio.service`](https://github.com/liblaf/swarmfolio/blob/main/assets/systemd/swarmfolio.service) and [`swarmfolio.timer`](https://github.com/liblaf/swarmfolio/blob/main/assets/systemd/swarmfolio.timer). Install the user units and start the hourly timer with:

```bash
swarmfolio systemd install
systemctl --user daemon-reload
systemctl --user enable --now swarmfolio.timer
systemctl --user list-timers swarmfolio.timer
```

The timer is persistent and adds up to five minutes of jitter. Run `loginctl enable-linger "$USER"` if it must execute while the user is logged out.

## 🧠 Selection Policy

Candidates must be recent, have enough freeleech time remaining, and pass the configured swarm thresholds. They are ranked deterministically by `(leechers + 1) / (seeders + 1)`. Eligible incumbents are ranked from lowest to highest lifetime upload throughput per stored byte. Spare capacity is filled first; otherwise the planner removes the weakest managed torrents needed to fit the best candidate while respecting action caps.

The read-only `plan` command never requests a torrent download token and never mutates qBittorrent. `run --apply` downloads metainfo only for selected candidates, verifies its info hash, uploads it stopped, rechecks ownership and size, then performs the replacement.

## ⌨️ Development and Releases

```bash
go test -race ./...
go vet ./...
go build ./cmd/swarmfolio
```

Repository maintenance comes from [`liblaf/copier-shared`](https://github.com/liblaf/copier-shared), while release PRs, tags, and GitHub Releases come from [`liblaf/copier-release`](https://github.com/liblaf/copier-release). The project-owned release-assets workflow tests the tagged source and attaches the single `swarmfolio` executable.

The generated release workflows require GitHub App credentials in the `release-please` environment: `vars.APP_CLIENT_ID` and `secrets.APP_PRIVATE_KEY`.

---

#### 📝 License

Copyright © 2026 [liblaf](https://github.com/liblaf). <br />
This project is [MIT](https://github.com/liblaf/swarmfolio/blob/main/LICENSE) licensed.
