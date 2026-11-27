# GitHub CLI Throttle

[English](README.md) | [中文](README.zh-CN.md)

In November 2026 my GitHub account was suspended without warning. My profile, my repositories and years of open-source work disappeared overnight. The likely trigger was the AI coding agents I had just started using: Claude Code, Codex and Copilot all drive GitHub through `gh`, and several of them running at once, each calling `gh` as fast as it could, looks a lot like a bot. After about two weeks GitHub Support reviewed the account by hand and restored it. Plenty of others weren't so lucky. GitHub's abuse detection has no published threshold, and `gh` itself never paces, backs off or retries ([cli/cli#3292](https://github.com/cli/cli/issues/3292)). So I built GitHub CLI Throttle: `gh` with brakes built in, so it can't happen again.

> [!IMPORTANT]
> This is an unofficial fork of [GitHub CLI](https://github.com/cli/cli). It is not affiliated with or endorsed by GitHub.

![screenshot of gh pr status](https://user-images.githubusercontent.com/98482/84171218-327e7a80-aa40-11ea-8cd1-5177fc2d0e72.png)

GitHub CLI Throttle is a drop-in replacement for `gh`. Every command works exactly as before, but writes from every `gh` process on the machine are queued and paced, GitHub rate limits make all of them back off together, and read commands are served from a shared cache. It supports macOS, Windows, and Linux.

<!-- this anchor is linked to from elsewhere, so avoid renaming it -->
## Installation

### macOS & Linux

```shell
curl -fsSL https://raw.githubusercontent.com/Qiming-Liu/gh-throttle/gh-throttle/install.sh | sh
```

### Windows (PowerShell)

```powershell
irm https://raw.githubusercontent.com/Qiming-Liu/gh-throttle/gh-throttle/install.ps1 | iex
```

The script downloads the latest release for your platform, checks its checksum, and installs `gh` to `~/.local/bin` (Windows: `%LOCALAPPDATA%\Programs\gh-throttle\bin`), putting it first on your `PATH`. If the official `gh` is installed, it asks before uninstalling it, and only does so if you type `y` or `Y`; agents then can't fall back to it. **Run the same command again to update.**

Then open a new terminal and run:

```shell
gh --version
gh auth login
gh throttle status
```

Set `GH_THROTTLE_VERSION=2.106.0` to install a specific release. You can also download an archive yourself from the [releases page][], and put `bin/gh` on your `PATH` ahead of any other `gh`.

### Build from source

With Go 1.27 or newer:

```shell
git clone https://github.com/Qiming-Liu/gh-throttle
cd gh-throttle
go install -ldflags "-X github.com/cli/cli/v2/internal/build.Version=2.105.0-throttle" ./cmd/gh
```

## Documentation

For usage of `gh` itself, [see the manual](https://cli.github.com/manual/). For the throttle:

```shell
gh throttle status   # circuit breaker, cooldowns, writes used this minute and hour
gh throttle log      # recent API calls (-n 50 for more)
gh throttle config   # effective limits
gh throttle reset    # close the circuit breaker; interactive terminal only
```

## How it works

### Write throttling

Writes from every `gh` process on the machine share one queue:

| Rule | Default |
|---|---|
| Writes in flight at once | **1** |
| Minimum gap between writes | **3 s** |
| Writes per minute | **20** |
| Writes per hour | **150** |
| Refuse an identical write that already succeeded | within **1 min** |

GitHub's documentation allows 80 content-creating requests per minute and 500 per hour, but people hit "was submitted too quickly" at around 20 per minute and 150 per hour ([cli/cli#4801](https://github.com/cli/cli/issues/4801)), so the defaults follow the measured limits. Anything other than `GET`, `HEAD` or `OPTIONS` counts as a write, and so do GraphQL mutations. Classification happens at the HTTP layer, so it covers every command, `gh api`, aliases and extensions.

### Rate limits and the circuit breaker

- Detects `403` and `429` responses, `Retry-After`, `X-RateLimit-*` headers, and GraphQL `RATE_LIMIT` errors returned with status 200.
- **Secondary limit**: every `gh` process stops for 1, 2, 4… up to 30 minutes, or for as long as `Retry-After` says if that is longer.
- **Content-creation block**: writes stop, and reads keep working.
- **Primary quota exhausted**: only that resource (for example `search`) waits until it resets.
- **3 secondary limits within an hour open the circuit breaker.** All API calls stop until a person runs `gh throttle reset` in an interactive terminal; an agent can't do it for you.
- Writes are never retried automatically. Refused or rate-limited commands exit with code **75** and tell the caller not to retry, and when it may try again.

### Read cache

Cacheable read commands are cached exactly as [ghx](https://github.com/brunoborges/ghx) does it: its allowlist, cache keys and output shaping are ported unchanged. TTLs are 30 s by default, 1 h for content addressed by commit SHA, and 10 min for completed Actions runs. `--json`/`--jq` variants share one fetch, identical in-flight commands are coalesced, and mutations invalidate what they touch. The cache is shared on disk by every `gh` process, and is used when stdout is not a terminal.

### Call log

Every API call is appended to `calls.jsonl` in gh's state directory. Request bodies are never logged.

## Configuration

Create `throttle.yml` in gh's config directory (`gh throttle config` prints the path):

```yaml
write_interval: 3s
writes_per_minute: 20
writes_per_hour: 150
dedupe_window: 1m
max_wait: 1m            # longest a write may wait for its turn before it is refused
breaker_strikes: 3
breaker_window: 1h
cooldown_base: 1m
cooldown_max: 30m
cache:                  # same keys as ghx
  ttl: 30s
  immutable_ttl: 1h
  settled_ttl: 10m
  max_cache_entries: 1000
  max_cache_bytes: 67108864
  ttl_overrides:
    pr_list: 60s
  additional_cacheable:
    - "status"
```

An invalid file stops all API calls until it is fixed. There is deliberately no environment variable that turns throttling off.

## Contributing

All of GitHub CLI Throttle lives in [`throttle/`](../throttle). It is wired into `gh` through small `gh_throttle.go` files next to upstream code, so upstream files carry no extra imports and only two one-line hooks. That keeps syncing with `cli/cli` almost free of conflicts:

| File | Hook |
|---|---|
| `cmd/gh/gh_throttle.go` | runs `gh` under `throttle.Main(...)`: read cache, exit code 75. `cmd/gh/main.go` is unchanged |
| `api/http_client.go` | one line, `throttleTransport()` from `api/gh_throttle.go`: write throttling at the HTTP layer |
| `pkg/cmd/root/root.go` | one line, `newCmdThrottle(f)` from `pkg/cmd/root/gh_throttle.go`: registers `gh throttle` |

The `trunk` branch mirrors upstream, and `gh-throttle` carries the changes on top. A daily workflow rebases `gh-throttle` onto upstream (so there are no merge commits) and publishes a matching release whenever `cli/cli` releases.

## What it can't protect

- It only governs traffic that goes through this `gh`. `git push`, `curl` with your token, or another copy of `gh` are not covered.
- Following GitHub's limits doesn't guarantee you won't be flagged, because abuse detection has no public threshold. This makes bursts much less likely; it doesn't make them impossible.

## Comparison with ghx

[ghx](https://github.com/brunoborges/ghx) is a caching proxy that sits in front of `gh`: it cuts repeated reads but lets every write straight through. GitHub CLI Throttle reuses ghx's read cache and adds what it leaves out: write pacing, deduplication, rate-limit cooldowns and a circuit breaker. It's built into `gh` itself, so there is no daemon to run and no second binary for agents to route around.

## Credits

- [GitHub CLI](https://github.com/cli/cli), MIT License
- [ghx](https://github.com/brunoborges/ghx) by Bruno Borges, MIT License. The read cache is ported from it; see [`throttle/ghx/LICENSE`](../throttle/ghx/LICENSE).

[releases page]: https://github.com/Qiming-Liu/gh-throttle/releases/latest
