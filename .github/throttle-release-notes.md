GitHub CLI {{VERSION}} with **gh-throttle**: built-in protection against bursts of GitHub API traffic from AI coding agents and scripts.

Based on upstream [cli/cli v{{VERSION}}](https://github.com/cli/cli/releases/tag/v{{VERSION}}). This is an unofficial fork and is not affiliated with GitHub.

### Write throttling (shared by every gh process on the machine)
- Writes are queued, one at a time, at least **3s** apart
- At most **20 per minute** and **150 per hour** (community-measured content-creation limits)
- An identical write that succeeded within **1 minute** is refused
- On a GitHub rate limit (403/429, `Retry-After`, GraphQL `RATE_LIMIT`) all gh processes cool down
- **3** secondary rate limits within an hour open a circuit breaker that only `gh throttle reset` (interactive terminal) can close
- Refused or rate-limited requests exit with status **75**

### Read cache
Follows [ghx](https://github.com/brunoborges/ghx) (MIT): same allowlist, cache keys, TTLs (30s, 1h for SHA-addressed content, 10m for completed runs), `--json`/`--jq` sharing, singleflight and invalidation after mutations. Used when stdout is not a terminal.

### New command
`gh throttle status | reset | log | config`. Limits can be changed in `throttle.yml` in gh's config directory.

### Install or update
macOS and Linux:
```shell
curl -fsSL https://raw.githubusercontent.com/Qiming-Liu/gh-throttle/gh-throttle/install.sh | sh
```
Windows (PowerShell):
```powershell
irm https://raw.githubusercontent.com/Qiming-Liu/gh-throttle/gh-throttle/install.ps1 | iex
```
Or download the archive for your platform below and put `bin/gh` on your PATH ahead of any other gh. Then run `gh auth login`.
