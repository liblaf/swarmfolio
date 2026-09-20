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
- The qBittorrent category is the sole ownership marker. Every torrent in the configured category is Swarmfolio-managed; move a torrent out of it to protect that torrent.
- Only complete, old, idle, low-activity managed torrents are eligible for replacement.
- New torrents are added stopped before any old data is removed. On the next applied run, an empty stopped download in the category is treated as an interrupted addition and is verified against current M-Team metainfo before it is resumed or removed.
- Applied runs use an operating-system file lock, so two Swarmfolio processes under the same local account cannot delete from the same portfolio concurrently.
- Candidate API responses, disk accounting, torrent metadata, and state changes are validated; unexpected state stops the run visibly.

## 📦 Installation

GitHub Releases provide compressed archives for these targets:

| Target | Release asset |
| --- | --- |
| macOS Apple Silicon (`darwin/arm64`) | `swarmfolio-darwin-arm64.tar.gz` |
| Linux x86-64 (`linux/amd64`) | `swarmfolio-linux-amd64.tar.gz` |
| Windows x86-64 (`windows/amd64`) | `swarmfolio-windows-amd64.zip` |

Download and extract the matching archive from [the latest release](https://github.com/liblaf/swarmfolio/releases/latest), then place the included `swarmfolio` (or `swarmfolio.exe` on Windows) on your `PATH`. The macOS and Linux archives preserve executable permissions.

For example, install a Linux AMD64 release into `~/.local/bin` and add it to your shell's search path:

```bash
install -d ~/.local/bin
gh release download --repo liblaf/swarmfolio --pattern swarmfolio-linux-amd64.tar.gz --clobber
tar -xzf swarmfolio-linux-amd64.tar.gz -C ~/.local/bin swarmfolio
export PATH="$HOME/.local/bin:$PATH"
```

Alternatively, build it with Go 1.26 or newer:

```bash
go install github.com/liblaf/swarmfolio/cmd/swarmfolio@latest
```

Swarmfolio targets qBittorrent 5.2 or newer. By default it connects to `http://localhost:8080` without authentication; a Bearer API key is optional.

## ⚙️ Configuration

Create the configuration file with:

```bash
swarmfolio config init
swarmfolio config path
```

Edit the file at the printed path. The default location is:

| Platform | Configuration file |
| --- | --- |
| Linux | `${XDG_CONFIG_HOME:-$HOME/.config}/swarmfolio/config.toml` |
| macOS | `~/Library/Application Support/swarmfolio/config.toml` |
| Windows | `%AppData%\swarmfolio\config.toml` |

The generated private file requires only your M-Team API key:

```toml
[mteam]
api-key = "replace-me"
```

Everything else has an application default: qBittorrent at `http://localhost:8080` without authentication, category `swarmfolio`, no hard byte ceiling, at least **1 TiB free** on the download filesystem, and at most two additions and four removals per run. Optional `[portfolio]`, `[mteam]`, `[qbittorrent]`, `[policy]`, and `[http]` keys override those defaults; unknown keys are rejected. Existing `api_key` entries remain supported; use only one spelling per section.

For a different WebUI address or authenticated access, add:

```toml
[qbittorrent]
base_url = "http://localhost:8080"
api-key = "your-qbittorrent-api-key" # Omit when authentication is not required.
```

The disk limit uses qBittorrent's reported free space and subtracts every unfinished byte already promised to qBittorrent before reserving 1 TiB (1,099,511,627,776 bytes). Total disk capacity and Docker access are not needed. By default, the managed category must be within qBittorrent's default save path and share its filesystem, such as `/downloads/.swarmfolio` under `/downloads`.

For a category on another filesystem, including a separate mount nested under the default path, set `portfolio.disk_path` to a host-visible path on that filesystem. To override the fixed reserve, use:

```toml
[portfolio]
minimum_free = "1 TiB"
```

`minimum-free` is also accepted; use only one spelling. The former `minimum_free_percent` and `disk_capacity` settings are no longer supported.

If the reserve is already exhausted and no safe replacement plan fits, the run reports an error. Swarmfolio replaces only eligible managed torrents; it does not delete unrelated data to restore free space.

Swarmfolio manages torrents only in its `qbittorrent.category` (default `swarmfolio`). Before running it, create that category in qBittorrent, set its desired save path, and explicitly disable the category's separate incomplete-download path. Swarmfolio enables **Automatic Torrent Management** for every torrent it adds, keeping its files separate from normal user-managed torrents while one filesystem budget accounts for every downloaded byte. Do not place user-managed torrents in this category.

For the minimal configuration, qBittorrent must already allow unauthenticated access from Swarmfolio's connection. If authentication is required, open **Tools → Preferences → Web UI**, generate an API key, and put it in `qbittorrent.api-key`; Swarmfolio sends it as a Bearer token. M-Team requires an API Access Token in `x-api-key`; create one under Control Panel → Lab → Access Token. Swarmfolio asks M-Team only for `FREE` results, also recognizes `_2X_FREE`, and skips promotions without a verifiable expiry.

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

## ⏱️ Hourly User Timer (Linux)

The executable embeds [`swarmfolio.service`](https://github.com/liblaf/swarmfolio/blob/main/assets/systemd/swarmfolio.service) and [`swarmfolio.timer`](https://github.com/liblaf/swarmfolio/blob/main/assets/systemd/swarmfolio.timer). Install the user units and start the hourly timer with:

```bash
swarmfolio systemd install
systemctl --user list-timers swarmfolio.timer
```

`systemd install` writes the embedded units, reloads the user systemd manager, and enables and starts `swarmfolio.timer`. It can be called repeatedly from a dotfiles lifecycle hook: identical units are accepted, while changed unit files require `--force` to replace. A failed systemctl command stops installation with an error and can be retried.

The service runs `swarmfolio` by name, searching `~/.local/bin` and standard system binary directories. For another installation directory, extend `ExecSearchPath` in a drop-in with `systemctl --user edit swarmfolio.service`.

The timer is persistent and adds up to five minutes of jitter. Run `loginctl enable-linger "$USER"` if it must execute while the user is logged out.

## 🧠 Selection Policy

Candidates must be recent, have enough freeleech time remaining, and pass the configured swarm thresholds. They are ranked deterministically by `(leechers + 1) / (seeders + 1)`. Eligible category torrents are ranked from lowest to highest lifetime upload throughput per stored byte. Spare capacity is filled first; otherwise the planner removes the weakest managed torrents needed to fit the best candidate while respecting action caps.

The read-only `plan` command never requests a torrent download token and never mutates qBittorrent. `run --apply` downloads metainfo only for selected candidates or interrupted-addition recovery, verifies exact info hashes, uploads additions stopped, rechecks category ownership and size, then performs the replacement.

## ⌨️ Development and Releases

```bash
go test -race ./...
go vet ./...
go build ./cmd/swarmfolio
```

Repository maintenance comes from [`liblaf/copier-shared`](https://github.com/liblaf/copier-shared), while release PRs, tags, and GitHub Releases come from [`liblaf/copier-release`](https://github.com/liblaf/copier-release). CI runs tests natively on macOS ARM64, Linux AMD64, and Windows AMD64. The project-owned release-assets workflow tests the tagged source on all three platforms before [GoReleaser](https://goreleaser.com/) builds and publishes exactly the three platform-named archives configured in [`.goreleaser.yaml`](.goreleaser.yaml). Each archive contains the executable, README, license, and changelog.

Validate the release configuration and build all release artifacts locally without publishing:

```bash
goreleaser check
goreleaser release --snapshot --clean
```

The generated release workflows require GitHub App credentials in the `release-please` environment: `vars.APP_CLIENT_ID` and `secrets.APP_PRIVATE_KEY`.

---

#### 📝 License

Copyright © 2026 [liblaf](https://github.com/liblaf). <br />
This project is [MIT](https://github.com/liblaf/swarmfolio/blob/main/LICENSE) licensed.
