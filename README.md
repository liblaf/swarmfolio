<div align="center" markdown>

![Swarmfolio](https://socialify.git.ci/liblaf/swarmfolio/image?description=1&forks=1&issues=1&language=1&name=1&owner=1&pattern=Transparent&pulls=1&stargazers=1&theme=Auto)

[![Made with Copier](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/copier-org/copier/master/img/badge/badge-black.json)](https://github.com/copier-org/copier)
[![Test](https://github.com/liblaf/swarmfolio/actions/workflows/test.yaml/badge.svg)](https://github.com/liblaf/swarmfolio/actions/workflows/test.yaml)
[![MegaLinter](https://github.com/liblaf/swarmfolio/actions/workflows/shared-mega-linter.yaml/badge.svg)](https://github.com/liblaf/swarmfolio/actions/workflows/shared-mega-linter.yaml)
[![Release](https://img.shields.io/github/v/release/liblaf/swarmfolio?logo=github)](https://github.com/liblaf/swarmfolio/releases/latest)

[Releases](https://github.com/liblaf/swarmfolio/releases) · [Report Bug](https://github.com/liblaf/swarmfolio/issues) · [Request Feature](https://github.com/liblaf/swarmfolio/issues)

![Rule](https://cdn.jsdelivr.net/gh/andreasbm/readme/assets/lines/rainbow.png)

</div>

Swarmfolio is a stateless, one-shot M-Team freeleech optimizer for qBittorrent. Each run chooses a portfolio of download-free torrents to maximize a heuristic score for **M-Team credited upload**, including 2× promotions, within its disk budget and action limits.

## ✨ Safety Model

- qBittorrent is the only persistent source of truth. Swarmfolio has no database or cache.
- Both plans and applied runs use qBittorrent's reported free space by default. Swarmfolio subtracts outstanding download commitments and rechecks the reported budget before starting downloads.
- The qBittorrent category is the sole ownership marker. Every torrent in the configured category is Swarmfolio-managed; move a torrent out of it to protect that torrent.
- Only complete, old, idle, low-activity managed torrents are eligible for replacement.
- Only `FREE` and `_2X_FREE` offers are allowed. An explicit `null` or empty `discountEndTime` means no scheduled end; timed offers must have sufficient freeleech time remaining. Swarmfolio refreshes M-Team offers before deleting replacements and immediately before starting or resuming a download. Missing offers, non-free discounts, malformed expiry data, or insufficient freeleech time stop the action.
- These checks govern download admission. As a one-shot tool, Swarmfolio does not continuously monitor running downloads or stop them when a promotion expires.
- New torrents are added stopped before any old data is removed. Swarmfolio waits for qBittorrent's initial checking state to settle before verifying the addition. On each applied run, stopped incomplete downloads in the category, including partial downloads, are verified against current M-Team metainfo before they are resumed or their registrations are removed.
- Before starting a torrent or deleting completed content, Swarmfolio rejects overlapping content paths across all categories. Missing content paths stop the action because isolation cannot be verified.
- Removing a pending torrent or rolling back an addition always retains its files (`deleteFiles=false`): zero verified progress does not prove that the files are absent or unshared. Retained files continue to consume disk space and may require manual cleanup.
- Before deleting replacements, Swarmfolio rechecks that the new torrent is still stopped and correctly managed. Matching retained partial or complete data can be reused after its identity, size, and content isolation are verified. After deletion, it waits up to ten minutes (`qbittorrent.poll_timeout`) for the removed torrents to disappear and the reported disk space to preserve the reserve, retrying temporary read timeouts within that window. A timeout leaves the new torrent stopped; the installed service retries automatically, rechecks qBittorrent and current freeleech offers, and resumes or replans the pending addition. A retry that finds the pending addition over budget first waits the same window for space reclaimed by the interrupted run to appear, so it does not delete further replacements for the same offer.
- Preallocated additions must fit alongside all existing unfinished download commitments before any replacement data is deleted.
- Torrent identity and payload size are checked against the downloaded metainfo before adding it, so differences between M-Team titles and qBittorrent names do not cause duplicate additions or failed recovery. Torrents already present in any category are excluded from new additions.
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

The remaining defaults are qBittorrent at `http://localhost:8080` without authentication, category `swarmfolio`, no hard byte ceiling, at least **1 TiB free** on the download filesystem, and at most two additions and four removals per run. Optional `[portfolio]`, `[mteam]`, `[qbittorrent]`, `[policy]`, and `[http]` keys override those defaults; unknown keys are rejected. Existing `api_key` entries remain supported; use only one spelling per section.

For a different WebUI address or authenticated access, add:

```toml
[qbittorrent]
base_url = "http://localhost:8080"
api-key = "your-qbittorrent-api-key" # Omit when authentication is not required.
poll_timeout = "10m" # How long to wait for qBittorrent to settle after each change.
```

The disk limit subtracts every unfinished byte already promised to qBittorrent from free space before reserving 1 TiB (1,099,511,627,776 bytes). Total disk capacity and Docker access are not needed. qBittorrent caches its API free-space value, so Swarmfolio polls after deletion until the reported space permits the download, starting at one-second intervals and backing off to fifteen seconds so a stalled qBittorrent is not flooded with requests. The wait is bounded by `qbittorrent.poll_timeout` (default ten minutes) and leaves the addition stopped until automatic recovery. The same bound applies while waiting for an addition to initialize or start. Temporary read timeouts are retried within that window; malformed responses and other API errors still fail immediately. A busy download disk can stall qBittorrent's Web API for minutes after a delete; a timeout error states the last condition observed, or that no read succeeded during the wait. The reserve is checked against that estimate; recent disk writes may not yet appear in it.

If a refreshed budget no longer permits the next addition before any mutation is attempted for it, Swarmfolio rebuilds the remaining plan from current qBittorrent state. Completed additions and removals count toward the original per-run action limits. The report preserves completed actions, replaces the unexecuted plan, and records the number of budget changes under `replans`. At most three such replans are allowed per run; continued changes fail visibly. Budget failures after an addition has begun retain the existing rollback or pending-recovery behavior.

The managed category must be within qBittorrent's default save path on the same filesystem, such as `/downloads/.swarmfolio` under `/downloads`. No extra setting is needed for this layout. The existing optional `portfolio.disk_path` override remains available for a direct measurement or a category on another filesystem, including a separate mount nested under the default path. To override the fixed reserve, use:

```toml
[portfolio]
minimum_free = "1 TiB"
```

`minimum-free` is also accepted; use only one spelling. The former `minimum_free_percent` and `disk_capacity` settings are no longer supported.

If the reserve is already exhausted and no safe replacement plan fits, the run reports an error. Swarmfolio replaces only eligible managed torrents; it does not delete unrelated data to restore free space.

Swarmfolio manages torrents only in its `qbittorrent.category` (default `swarmfolio`). Before running it, create that category in qBittorrent, set its desired save path, and explicitly disable the category's separate incomplete-download path. Swarmfolio enables **Automatic Torrent Management** for every torrent it adds, using the category save path while one filesystem budget accounts for every downloaded byte. Content isolation checks use qBittorrent-reported paths; keep the category directory dedicated, avoid filesystem aliases such as symlinks or bind mounts that expose the same files under different paths, and avoid concurrent content moves during an applied run. Do not place user-managed torrents in this category.

For the minimal configuration, qBittorrent must already allow unauthenticated access from Swarmfolio's connection. If authentication is required, open **Tools → Preferences → Web UI**, generate an API key, and put it in `qbittorrent.api-key`; Swarmfolio sends it as a Bearer token. M-Team requires an API Access Token in `x-api-key`; create one under Control Panel → Lab → Access Token. Swarmfolio searches M-Team for both `FREE` and `_2X_FREE` results, including promotions with no scheduled end (`discountEndTime: null` or an empty string). Both promotions are download-free; `_2X_FREE` also doubles upload credit. An offer must still appear in the configured search pages when it is rechecked before replacement or start. A missing expiry field or malformed timestamp is an API error.

Test the complete read-only path before enabling mutations. Planning downloads selected candidates' metainfo to verify their identities and payload sizes and selects alternatives when a torrent already exists. It does not change qBittorrent:

```bash
swarmfolio plan
swarmfolio run --apply
```

Use `--json` with `plan` or `run` for machine-readable reports.

An execution failure still returns the available report and a nonzero exit status. Its `error` describes the failure, and `mutations` lists attempted `add`, `delete`, and `start` operations by infohash. Delete receipts also include `delete_files`, distinguishing content deletion from removal of the torrent registration while retaining files. Status `accepted` means qBittorrent acknowledged the request; asynchronous work may still be running. Status `unconfirmed` means the request failed without a confirmed outcome and may have taken effect. `actions[].applied` becomes true only after that entire addition sequence succeeds and qBittorrent reports the torrent running, queued, or complete. A start acknowledgement that leaves an incomplete torrent stopped fails the run so the service can retry. Recovery records likewise require confirmation that a torrent resumed or its registration disappeared. Configuration and startup errors that occur before execution begins produce only an error.

## 🐟 Fish Completion

```fish
mkdir -p "${XDG_CONFIG_HOME:-$HOME/.config}/fish/completions"
swarmfolio completion fish >"${XDG_CONFIG_HOME:-$HOME/.config}/fish/completions/swarmfolio.fish"
```

## ⏱️ Unattended Operation (Linux)

The executable embeds [`swarmfolio.service`](https://github.com/liblaf/swarmfolio/blob/main/assets/systemd/swarmfolio.service) and [`swarmfolio.timer`](https://github.com/liblaf/swarmfolio/blob/main/assets/systemd/swarmfolio.timer). Install the user units and start the hourly timer with:

```bash
swarmfolio systemd install
systemctl --user list-timers swarmfolio.timer
```

`systemd install` writes the embedded units, reloads the user systemd manager, and enables and starts `swarmfolio.timer`. It can be called repeatedly from a dotfiles lifecycle hook: identical units are accepted, while changed unit files require `--force` to replace. A failed systemctl command stops installation with an error and can be retried.

The timer performs routine optimization hourly. If a run fails, the service retries automatically after one minute, backing off to at most thirty minutes between consecutive failures, and continues retrying without a start-limit lockout. The backoff requires systemd 254 or newer; older versions ignore it and retry every minute. Each retry rebuilds its decisions from current qBittorrent state and M-Team offers; it can resume a valid pending addition, remove a stale registration while retaining its files, or replan an interrupted replacement. An API response lost after a successful operation is reconciled from the next snapshot. No manual torrent start or recovery command is needed for these interrupted runs. Errors remain visible in `journalctl --user -u swarmfolio.service`.

When upgrading an existing installation, `swarmfolio systemd install --force` installs the updated retry policy and keeps the hourly timer enabled. The standalone `run --apply` command still performs one pass; the installed service supplies automatic retries.

The service runs `swarmfolio` by name, searching `~/.local/bin` and standard system binary directories. For another installation directory, extend `ExecSearchPath` in a drop-in with `systemctl --user edit swarmfolio.service`.

The timer is persistent and adds up to five minutes of jitter. Run `loginctl enable-linger "$USER"` if it must execute while the user is logged out.

## 🧠 Selection Policy

Candidates must be recent, have enough freeleech time remaining, have at least one seeder and one leecher, and pass the configured swarm thresholds. Opportunity is `leechers / (seeders + 1)`: the additional seeder represents us, and zero demand earns no score.

For a planning horizon `H` (default **24 hours**), each candidate receives a credited-byte opportunity score:

```text
freshness = H / (H + torrent_age)
credit_factor = 1 + (upload_multiplier - 1) × min(freeleech_remaining / H, 1)
candidate_score = size × leechers / (seeders + 1) × freshness × credit_factor
```

`FREE` has an upload multiplier of 1; `_2X_FREE` has a multiplier of 2. The extra credit is prorated over the time remaining in the promotion, while base upload retains value after it ends. Conflicting promotion metadata for the same M-Team ID stops the run.

For each eligible existing torrent, the cost of removing it is:

```text
retention_score = 2 × max(current_upload_rate, uploaded_bytes / max(age_seconds, 1)) × H_seconds
net_gain = sum(candidate_score) - replacement_margin × sum(retention_score)
```

The existing torrent's [qBittorrent upload counters](https://github.com/qbittorrent/qBittorrent/wiki/WebUI-API-%28qBittorrent-5.0%29#get-torrent-list) measure transfer activity; they do not identify its current M-Team promotion. The retention score conservatively reserves 2× credit for incumbents. The default replacement margin of **1.25** adds a further buffer against uncertain acquisition value. Existing ownership, completion, residency, idle-time, and active-upload protections still decide which torrents may be removed.

The planner jointly searches additions and removals for the largest positive `net_gain`, subject to the exact byte budget and both action caps. This lets a pair of candidates beat a single attractive torrent and avoids consuming the removal cap on tiny, low-value torrents. Equal scores favor fewer removals, fewer additions, lower final storage, then stable IDs and hashes. No positive gain means no changes. Removals occur only as part of an addition, and each applied step must fit the budget.

The search uses exact Pareto frontiers over the eligible offers returned by the configured M-Team pages. It fails visibly if its work limit is exceeded; reduce candidate pages or action caps in that case. It does not silently discard candidates or switch to a greedy approximation.

Optional policy overrides:

```toml
[policy]
planning_horizon = "24h"
replacement_margin = 1.25 # Must be finite and at least 1.
max_incomplete_downloads = 6 # Optional; unset means no cap.
```

`max_incomplete_downloads` limits how many managed torrents may be unfinished at once, including stopped pending additions. A run adds no more torrents than that limit leaves room for. Concurrent downloads share one disk, and on a hard drive many of them can starve seeding and stall qBittorrent.

When an addition needs several removals, the least valuable selected incumbents are deleted first, so an interrupted run has spent the cheapest part of its removal set.

Offers with no scheduled end use the full promotion multiplier throughout the planning horizon. Their JSON `free_until` is `null`; timed offers retain an expiry timestamp.

The JSON report exposes `upload_score_bytes` for additions and removals, `planning_horizon` as a duration string, `replacement_margin`, and `net_gain_score_bytes`. These are **heuristic scores, not measured or guaranteed future upload**. The model assumes full-file leecher demand shared with seeders, reduces older demand, and spreads upload uniformly across the horizon when valuing a bonus. It cannot observe leecher completion, predict download time or future arrivals, or model bandwidth contention from one snapshot. Minimum freeleech time is an eligibility guard, not proof a download will finish before expiry. Real improvement needs comparison with actual credited upload over time.

The read-only `plan` command requests download tokens and reads selected candidates' metainfo to verify their infohashes, without mutating qBittorrent. `run --apply` also resolves interrupted additions, reuses verified metainfo within the run, uploads additions stopped, waits for initialization, and rechecks category ownership and size before performing the replacement.

M-Team limits each torrent's metainfo downloads to ten per day. If it reports that this allowance is exhausted, Swarmfolio lists the affected ID under `skipped_candidates` (or `Skipped M-Team` in text output), excludes it for the current run, and replans using other verified freeleech offers. A later scheduled run can try that torrent again. This specific quota response does not block the whole portfolio. Likewise, if the metainfo download itself returns an M-Team API error code, the offer is skipped for the run with its code, and planning continues; more than three such refusals in one run stop it, because they suggest an account-wide problem. While recovering a pending addition, such a refusal stops the run instead, because it may be transient and concern the pending torrent's own offer. Other API, authentication, network, or malformed-metadata errors still stop the run visibly.

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
