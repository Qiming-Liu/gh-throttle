# GitHub CLI Throttle

[English](README.md) | [中文](README.zh-CN.md)

2026 年 11 月，我的 GitHub 账号在没有任何警告的情况下被封了。一夜之间，我的主页、我的仓库和几年的开源工作全都不见了。最可能的原因，是我刚开始用的 AI 编程 agent：Claude Code、Codex 和 Copilot 都通过 `gh` 来操作 GitHub。好几个 agent 同时运行、各自拼命调用 `gh`，看上去就和机器人一模一样。大约两周后，GitHub 客服人工复查了我的账号，把它恢复了。很多人就没这么幸运了。GitHub 的反滥用检测没有公开的阈值，而 `gh` 本身既不控制请求节奏、也不退避或重试（[cli/cli#3292](https://github.com/cli/cli/issues/3292)）。所以我做了 GitHub CLI Throttle：一个自带刹车的 `gh`，让这种事不再发生。

> [!IMPORTANT]
> 这是 [GitHub CLI](https://github.com/cli/cli) 的非官方 fork，与 GitHub 没有任何关系，也未获其认可。

![screenshot of gh pr status](https://user-images.githubusercontent.com/98482/84171218-327e7a80-aa40-11ea-8cd1-5177fc2d0e72.png)

GitHub CLI Throttle 可以直接替换 `gh`。所有命令用法完全不变，区别在于：本机所有 `gh` 进程的写操作会排队、按节奏发送；遇到 GitHub 限流时所有进程一起退避；读命令从共享缓存返回。支持 macOS、Windows 和 Linux。

## 安装

### macOS 和 Linux

```shell
curl -fsSL https://raw.githubusercontent.com/Qiming-Liu/gh-throttle/gh-throttle/install.sh | sh
```

### Windows（PowerShell）

```powershell
irm https://raw.githubusercontent.com/Qiming-Liu/gh-throttle/gh-throttle/install.ps1 | iex
```

脚本会下载适合你平台的最新版本，校验 checksum，把 `gh` 装到 `~/.local/bin`（Windows 是 `%LOCALAPPDATA%\Programs\gh-throttle\bin`），并把它放在 `PATH` 最前面。如果装了官方的 `gh`，脚本会先问你要不要卸载，只有输入 `y` 或 `Y` 才会卸载。卸掉之后，agent 就没法退回去用官方的 `gh`。**更新时再运行一次同一条命令即可。**

然后打开一个新终端，运行：

```shell
gh --version
gh auth login
gh throttle status
```

想装指定版本，设置 `GH_THROTTLE_VERSION=2.106.0`。也可以自己从 [Releases 页面][releases page]下载压缩包，把 `bin/gh` 放进 `PATH`，排在其他 `gh` 前面。

### 从源码编译

需要 Go 1.27 或更新版本：

```shell
git clone https://github.com/Qiming-Liu/gh-throttle
cd gh-throttle
go install -ldflags "-X github.com/cli/cli/v2/internal/build.Version=2.105.0-throttle" ./cmd/gh
```

## 文档

`gh` 本身的用法请[看官方手册](https://cli.github.com/manual/)。限流相关命令：

```shell
gh throttle status   # 熔断器、冷却状态、本分钟和本小时用掉的写操作次数
gh throttle log      # 最近的 API 调用（加 -n 50 看更多）
gh throttle config   # 当前生效的限额
gh throttle reset    # 关闭熔断器；只能在交互式终端里执行
```

## 工作原理

### 写操作限流

本机所有 `gh` 进程的写操作共用一个队列：

| 规则 | 默认值 |
|---|---|
| 同时进行的写操作 | **1** 个 |
| 两次写操作的最小间隔 | **3 秒** |
| 每分钟写操作上限 | **20** 次 |
| 每小时写操作上限 | **150** 次 |
| 拒绝已经成功过的相同写操作 | **1 分钟**内 |

GitHub 文档写的是每分钟 80 次、每小时 500 次创建内容的请求，但实际上大约每分钟 20 次、每小时 150 次就会遇到 "was submitted too quickly"（[cli/cli#4801](https://github.com/cli/cli/issues/4801)），所以默认值采用实测的限额。除了 `GET`、`HEAD`、`OPTIONS` 之外的请求都算写操作，GraphQL mutation 也算。判断是在 HTTP 层做的，所以所有命令、`gh api`、别名和扩展都覆盖到了。

### 限流与熔断

- 能识别 `403` 和 `429` 响应、`Retry-After`、`X-RateLimit-*` 响应头，以及以 200 状态返回的 GraphQL `RATE_LIMIT` 错误。
- **二级限流**：所有 `gh` 进程暂停 1、2、4……分钟，最长 30 分钟。如果 `Retry-After` 给的时间更长，就按它来。
- **创建内容被限制**：只停写操作，读操作照常。
- **主额度用完**：只有对应的资源（比如 `search`）等到额度重置。
- **1 小时内遇到 3 次二级限流就熔断**：所有 API 调用停止，直到有人在交互式终端里执行 `gh throttle reset`。agent 无法替你恢复。
- 写操作永远不会自动重试。被拒绝或被限流的命令退出码为 **75**，并告诉调用方不要重试、什么时候可以再试。

### 读缓存

可缓存的读命令完全按照 [ghx](https://github.com/brunoborges/ghx) 的方式缓存：它的白名单、缓存 key 和输出处理代码都原样移植过来。默认 TTL 为 30 秒；按 commit SHA 定位的内容为 1 小时；已完成的 Actions run 为 10 分钟。只有 `--json`/`--jq` 不同的请求共用一次请求，同时发出的相同命令会合并成一次，写操作之后会让受影响的缓存失效。缓存存放在磁盘上，所有 `gh` 进程共用，在 stdout 不是终端时生效。

### 调用日志

每次 API 调用都会追加到 gh 状态目录下的 `calls.jsonl`。请求正文永远不会被记录。

## 配置

在 gh 的配置目录里创建 `throttle.yml`（`gh throttle config` 会显示路径）：

```yaml
write_interval: 3s
writes_per_minute: 20
writes_per_hour: 150
dedupe_window: 1m
max_wait: 1m            # 写操作排队的最长等待时间，超过就拒绝
breaker_strikes: 3
breaker_window: 1h
cooldown_base: 1m
cooldown_max: 30m
cache:                  # 和 ghx 的配置项相同
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

配置文件写错时，所有 API 调用都会停止，直到修好为止。我们刻意没有提供任何能关闭限流的环境变量。

## 参与开发

GitHub CLI Throttle 的全部代码都在 [`throttle/`](../throttle) 里。它通过放在上游代码旁边的几个小文件 `gh_throttle.go` 接入 `gh`，上游文件里不加任何 import，只有两处单行调用。所以和 `cli/cli` 同步时几乎不会冲突：

| 文件 | 接入点 |
|---|---|
| `cmd/gh/gh_throttle.go` | 用 `throttle.Main(...)` 运行 `gh`：读缓存、退出码 75。`cmd/gh/main.go` 不改 |
| `api/http_client.go` | 一行，调用 `api/gh_throttle.go` 里的 `throttleTransport()`：在 HTTP 层做写限流 |
| `pkg/cmd/root/root.go` | 一行，调用 `pkg/cmd/root/gh_throttle.go` 里的 `newCmdThrottle(f)`：注册 `gh throttle` 命令 |

`trunk` 分支是上游的镜像，`gh-throttle` 分支在它之上放我们的改动。每天有一个 workflow 把 `gh-throttle` rebase 到上游最新代码上（所以没有 merge commit），`cli/cli` 一发新版，就发布对应的版本。

## 它防不住什么

- 它只管经过这个 `gh` 的请求。`git push`、带着你 token 的 `curl`、或者另一个 `gh`，都不在保护范围内。
- 遵守 GitHub 的限额并不能保证不被标记，因为反滥用检测没有公开的阈值。它能让请求扎堆的情况大大减少，但不能完全杜绝。

## 和 ghx 的对比

[ghx](https://github.com/brunoborges/ghx) 是挡在 `gh` 前面的缓存代理：它减少了重复的读请求，但写操作会直接放行。GitHub CLI Throttle 复用了 ghx 的读缓存，并补上了它没做的部分：写操作节流、去重、限流冷却和熔断。而且它直接内置在 `gh` 里，不需要运行 daemon，agent 也没有第二个程序可以绕过去。

## 致谢

- [GitHub CLI](https://github.com/cli/cli)，MIT 协议
- [ghx](https://github.com/brunoborges/ghx)，作者 Bruno Borges，MIT 协议。读缓存移植自 ghx，见 [`throttle/ghx/LICENSE`](../throttle/ghx/LICENSE)。

[releases page]: https://github.com/Qiming-Liu/gh-throttle/releases/latest
