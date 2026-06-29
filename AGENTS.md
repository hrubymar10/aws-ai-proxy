# aws-ai-proxy

> `CLAUDE.md` in this repo is a symlink to this file.

## Project

`aws-ai-proxy` is a host-side HTTP proxy that exposes AWS credentials for
explicitly allowlisted local AWS profiles. Docker projects such as
`claude-docker`, `codex-docker`, and `pi-docker` are consumers only; they do
not build, start, or stop this process.

## Important files

- `cmd/aws-ai-proxy/main.go` - binary entrypoint and HTTP handlers
- `bin/aws-ai-proxy` - development wrapper that builds and runs the binary
- `README.md` - user-facing setup, config, install, and endpoint docs
- `Makefile` - common test/build/format targets

## Configuration

- `AWS_AI_PROXY_PROFILES` - required allowlist, comma-separated profile names;
  entries may use `profile:region` to set an explicit `/profiles` region
- `AWS_AI_PROXY_BIND` - bind address, default `127.0.0.1:9998`; host must be
  an IP literal
- `AWS_AI_PROXY_ALLOW` - source IP/CIDR allowlist, default
  `127.0.0.0/8,::1/128`
- `AWS_AI_PROXY_NOTIFICATIONS_ENABLED` - OS notifications for successful
  credential requests, default `true`; `false`, `0`, `no`, or `off` disables
- `AWS_AI_PROXY_NOTIFICATION_DEDUP_WINDOW` - throttle repeat notifications per
  client+profile, default `5m`; Go duration, `0` disables throttling
- `AWS_AI_PROXY_AWS_CLI_BINARY_PATH` - optional absolute path to the `aws` CLI
  binary
- `AWS_AI_PROXY_AWS_CONFIG_PATH` - optional path passed to `aws` subprocesses as
  `AWS_CONFIG_FILE`

Configuration is env-first, with per-field fallback to
`~/.aws-ai-proxy/config`. The config file uses `KEY=VALUE` lines with the same
names and values as the env vars:

```sh
AWS_AI_PROXY_BIND=127.0.0.1:9998
AWS_AI_PROXY_ALLOW=127.0.0.0/8,::1/128
AWS_AI_PROXY_PROFILES=dev,prod
AWS_AI_PROXY_ACCESS_LOGS_ENABLED=true
AWS_AI_PROXY_NOTIFICATIONS_ENABLED=true
AWS_AI_PROXY_NOTIFICATION_DEDUP_WINDOW=5m
AWS_AI_PROXY_AWS_CLI_BINARY_PATH=
AWS_AI_PROXY_AWS_CONFIG_PATH=
```

Startup auto-creates `~/.aws-ai-proxy/` as `0700` and the config file as
`0600` with uncommented defaults, and appends missing recognized keys as active
`KEY=VALUE` lines to existing files. Stale `# KEY=` comments do not count as
active config. Do not rewrite user lines.

Access logging is enabled by default at `~/.aws-ai-proxy/access.log`. The
access logger must be the outermost middleware so denied requests are recorded;
never log credential response bodies or secrets.

Runtime warnings and errors are written to `~/.aws-ai-proxy/error.log`.

Successful `/credentials/{profile}` responses send an OS notification when
notifications are enabled. The optional `X-Aws-Ai-Proxy-Client` request header
identifies the caller in notification text; `User-Agent` is used as a fallback.
Client text is sanitized and capped before logging or notification. Repeat
notifications for the same client+profile are throttled to one per
`AWS_AI_PROXY_NOTIFICATION_DEDUP_WINDOW` (in-memory, mutex-guarded, with
opportunistic pruning); the throttle applies only to the notification, never to
credential serving or the access/`OK` logs.

Resolve the AWS CLI in this order: `AWS_AI_PROXY_AWS_CLI_BINARY_PATH`, then
`exec.LookPath("aws")`, then `/opt/homebrew/bin/aws`, `/usr/local/bin/aws`, and
`/usr/bin/aws`. When `AWS_AI_PROXY_AWS_CONFIG_PATH` is set, pass it to the AWS
CLI subprocess as `AWS_CONFIG_FILE`; it may point at the config file or an
existing `.aws` directory, in which case append `config`. A leading `~` is
expanded to the current user's home directory for both path-valued settings;
`~otheruser` is not expanded.

For `/profiles`, region precedence is inline `profile:region` from
`AWS_AI_PROXY_PROFILES`, then `aws configure get region --profile <profile>`,
then an empty string.

Keep the parser dependency-free; this is an env file, not a general shell
parser.

The request allowlist is enforced from `RemoteAddr`; do not trust
`X-Forwarded-For` for this direct-connection service.

## Homebrew tap

If `../homebrew-tap` exists and its `origin` targets
`https://github.com/hrubymar10/homebrew-tap`, cross-update the `aws-ai-proxy`
formula there for packaging-affecting changes such as release version/sha
bumps, service definitions, or install steps. Never push without explicit
instruction.

## Development

```bash
make test
make vet
bin/aws-ai-proxy --version
bin/aws-ai-proxy --help
AWS_AI_PROXY_PROFILES=test bin/aws-ai-proxy serve
bin/aws-ai-proxy status
bin/aws-ai-proxy stop
```

Use `AWS_AI_PROXY_FORCE_BUILD=1 bin/aws-ai-proxy --version` to force a fresh
temporary build from the wrapper.

## Commit messages

Use conventional commit subjects where they fit, with a body for any
non-trivial behavior, docs, or test change. Do not add AI attribution.
