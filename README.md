# barrahome-agent

The streaming agent behind the terminal on [barrahome.org](https://barrahome.org).
Press the backtick key on the site, ask a question, and this answers it — with
read-only tools over the blog's content, confined by
[sandlock](https://github.com/multikernel/sandlock) (Landlock + seccomp) inside
a container.

Public and unauthenticated, so cost and blast radius are bounded deliberately:
three read-only file tools, no shell, no network tool, and one permitted egress
destination.

## Requirements

| | |
|---|---|
| Go | 1.24+ |
| Rust | any recent stable (builds `libsandlock_ffi`) |
| Kernel | 6.12+ — sandlock needs Landlock ABI 6. Check with `cat /sys/kernel/security/lsm` (must list `landlock`) |
| Docker | only for the confined container run |
| glibc | **not musl.** sandlock does not build on musl: its vendored `sandlock-core` calls `ptrace` with `c_uint` constants that only match glibc |

Clone with the submodule, or the build will fail on a missing sandlock:

```bash
git clone --recursive https://github.com/bet0x/barrahome-2026-agent.git
# already cloned without it:
git submodule update --init --recursive
```

## Setup

Create `.env` at the repo root. It is gitignored — never commit it.

```bash
cat > .env <<'EOF'
MOONSHOT_API_KEY=sk-your-key-here
BARRAHOME_WORKSPACE=/path/to/the/content/directory
BARRAHOME_ALLOWED_ORIGINS=http://localhost:8080,https://barrahome.org
EOF
chmod 600 .env
```

`BARRAHOME_WORKSPACE` is the directory the agent may read — the curated blog
content, and nothing else. For local development, pointing it at a checkout of
the blog repo works fine.

Get an API key from [platform.kimi.ai](https://platform.kimi.ai). The default
model is `kimi-k2.6`, the cheapest tier that supports streaming plus tool
calls.

## Two ways to run

The binary has two subcommands, and the difference matters:

- **`serve`** — the HTTP server, running **unconfined**. Fast to iterate on.
- **`supervise`** — builds the sandlock policy and runs `serve` as a confined
  child. This is what the container's entrypoint uses.

`supervise` needs a child process because sandlock's `Confine()` applies
filesystem rules only and rejects network policy outright — and `NetAllow` is
the whole reason sandlock is here.

### Native, unconfined — for development

```bash
make build                       # builds libsandlock_ffi, then the binary
set -a && . ./.env && set +a
./barrahome-agent serve
```

It listens on `127.0.0.1:9000`. There is no sandbox in this mode, so the tools
are limited only by the path validation in `internal/content`.

### Container, confined — the real thing

```bash
make up        # docker compose up -d --build
make logs      # follow
make down
```

Expect two log lines, in this order:

```
supervising confined worker (workspace=/workspace port=9000)
serving on 0.0.0.0:9000 (model=kimi-k2.6)
```

If you only see the first, the sandbox refused to start and the container
exited — see Troubleshooting. It **fails closed**: the worker never runs
unconfined.

Note how the workspace is wired: `.env`'s `BARRAHOME_WORKSPACE` is a *host*
path used both for native runs and as the bind-mount source, while
`compose.yaml` overrides the value *inside* the container to `/workspace`.

## Sending it commands

Two endpoints. `/healthz` is static JSON; `/stream` is where the agent lives.

```bash
curl -s localhost:9000/healthz
# {"ok":true}
```

`/stream` takes a POST and answers with a Server-Sent Events stream. It
requires an `Origin` header matching `BARRAHOME_ALLOWED_ORIGINS` — a browser
sends this automatically, so with `curl` you supply it yourself.

```bash
curl -sN -X POST localhost:9000/stream \
  -H 'Content-Type: application/json' \
  -H 'Origin: http://localhost:8080' \
  -d '{"session_id":"local-test-1","message":"What posts do you have about nginx?"}'
```

`-N` disables curl's buffering so you see tokens arrive rather than the whole
answer at once.

Reuse the same `session_id` to continue a conversation; change it to start
fresh. Ids must be 8-64 characters of `A-Za-z0-9_-`. The browser frontend uses
`crypto.randomUUID()` and keeps one per tab in `sessionStorage`.

### What comes back

```
event: tool_start
data: {"text":"search_content {\"query\":\"nginx\"}"}

event: tool_result
data: {"text":"search_content"}

event: text
data: {"text":"You have four posts touching nginx"}

event: text
data: {"text":", the most detailed being"}

event: done
data: {"turns_left":19}
```

| Event | Meaning |
|---|---|
| `text` | A token delta — append it to the current reply |
| `tool_start` | The model is reading something; `text` names the tool and its arguments |
| `tool_result` | That tool finished |
| `error` | Something failed; `message` is safe to show a visitor |
| `done` | Final frame, with the session's remaining turns |

### Status codes worth knowing

| Code | Why |
|---|---|
| 400 | Malformed JSON, a bad `session_id` shape, or a message over the length cap |
| 403 | `Origin` missing or not in the allow-list |
| 409 | Either a request for this session is already running, or the session spent its turns |
| 429 | This IP used its hourly quota |
| 503 | Too many streams in flight globally, or the session store is at capacity |

## The tools it has

Three, all read-only native Go file operations over the workspace. Nothing
executes a subprocess and nothing opens a socket, which is what makes prompt
injection non-exfiltrating: hostile text in the content can make the model
*say* something, but there is no mechanism for it to *send* anything anywhere.

| Tool | Does |
|---|---|
| `list_dir(path)` | Lists a directory; `""` is the workspace root |
| `read_file(path)` | Reads a file, capped at 64 KB |
| `search_content(query)` | Case-insensitive search across the tree |

Paths are relative to the workspace root. Absolute paths, `..` segments and
symlinks leaving the root are all rejected before touching disk, and Landlock
would refuse them anyway.

## Configuration

Everything is environment-driven. Only `MOONSHOT_API_KEY` is required.

| Variable | Default | |
|---|---|---|
| `MOONSHOT_API_KEY` | — | Required |
| `MOONSHOT_BASE_URL` | `https://api.moonshot.ai/v1` | |
| `MOONSHOT_MODEL` | `kimi-k2.6` | |
| `MOONSHOT_THINKING` | `disabled` on kimi-k2.x | `enabled`/`disabled`; ignored (with a startup warning) on models other than kimi-k2.x |
| `MOONSHOT_REASONING_EFFORT` | unset | `low`/`high`/`max`; applies to kimi-k3 only, ignored (with a startup warning) otherwise |
| `BARRAHOME_WORKSPACE` | `/workspace` | The only readable content directory |
| `BARRAHOME_PORT` | `9000` | |
| `BARRAHOME_ALLOWED_ORIGINS` | `https://barrahome.org,https://www.barrahome.org` | Comma-separated, matched exactly |
| `BARRAHOME_MAX_TURNS` | `20` | Per session |
| `BARRAHOME_MAX_TOKENS` | `1024` | Per response; sent upstream as `max_completion_tokens`, not the deprecated `max_tokens` |
| `BARRAHOME_MAX_TOOL_ROUNDS` | `3` | Bounds the agent loop; the round after this one gets a tools-less call forced to a prose answer rather than an error |
| `BARRAHOME_SESSION_TTL_MIN` | `30` | Idle sessions are swept |
| `BARRAHOME_PER_IP_PER_HOUR` | `20` | The main cost control |
| `BARRAHOME_MAX_CONCURRENT` | `10` | Simultaneous upstream streams; excess is refused, never queued |
| `BARRAHOME_SHUTDOWN_TIMEOUT_SEC` | `30` | How long `serve` waits for in-flight streams to drain on shutdown; `compose.yaml`'s `stop_grace_period` must stay comfortably above it |

## Tests

```bash
make test                                        # everything
go test -tags sandlock_repo -race -count=1 ./...  # what CI should run
```

The `sandlock_repo` build tag is required on every `go` command — it points cgo
at the in-tree `third_party/sandlock`. Forgetting it produces a confusing
linker error.

`internal/sandbox` tests exercise real sandlock against the running kernel:
they assert the workspace is readable while `/etc/shadow` is not, and that
`api.moonshot.ai` is reachable while `example.com` and a raw IP are refused.
They need network access and take a couple of seconds.

## Troubleshooting

**`sandlock: failed to create sandbox`** — Docker's default seccomp profile
blocks sandlock entirely. The container needs
`--security-opt seccomp=unconfined` (already in `compose.yaml`). This trades
Docker's syscall filter for sandlock's own; namespaces, dropped capabilities,
the read-only rootfs and cgroup limits all stay. Do not "fix" this by removing
the sandbox.

**`worker exited with code -1`, nothing from the child** — the sandbox policy
carries a `MaxMemory`. sandlock accounts memory by intercepting `mmap` lengths
rather than measuring RSS, and Go's arena reservation blows through any sane
cap at startup. A bare `fmt.Println` binary dies at 192M and survives only
around 2-4G, while `/bin/sh` and `python3` run fine at 192M. Memory is capped
by the container's cgroup (`mem_limit`) instead, which measures RSS and works.

**`worker exited with code 127`, "Permission denied"** — the agent binary is not
under a path the policy marks readable. It must live under `/usr`, `/bin`, or
the workspace.

**`kernel Landlock ABI vN is below the required vM`** — the kernel is older than
6.12. Nothing to configure; sandlock cannot run.

**403 on every request** — the `Origin` header is missing or not in
`BARRAHOME_ALLOWED_ORIGINS`. Matching is exact: scheme, host and port, no
trailing slash, case-sensitive.

**409 immediately, repeatedly** — one request per session at a time. Either a
previous request is still streaming, or the session spent its 20 turns; use a
new `session_id`.

**A linker error mentioning `sandlock_ffi`** — either the `-tags sandlock_repo`
flag is missing, or the FFI library was never built. Run `make ffi`.

## Layout

```
cmd/barrahome-agent/     supervise | serve
internal/sandbox/        the sandlock policy, plus tests that prove it holds
internal/content/        path validation and the three read-only tools
internal/moonshot/       wire types and the streaming client
internal/agentloop/      model ↔ tool orchestration
internal/session/        in-memory sessions, turn caps, LRU eviction
internal/limits/         per-IP quota and global concurrency
internal/httpapi/        HTTP routes, Origin check, SSE writer
internal/config/         environment configuration
third_party/sandlock/    pinned submodule
```

The design, including the measurements behind the odd-looking decisions, is
written up in the blog repo under `docs/superpowers/specs/` — worth reading
before changing the sandbox policy.
