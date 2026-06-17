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

- `AWS_AI_PROXY_PROFILES` - required allowlist, comma-separated profile names
- `AWS_AI_PROXY_BIND` - bind address, default `127.0.0.1:9998`; host must be
  an IP literal
- `AWS_AI_PROXY_ALLOW` - source IP/CIDR allowlist, default
  `127.0.0.0/8,::1/128`

Configuration is env-first, with per-field fallback to
`~/.aws-ai-proxy/config`. The config file uses `KEY=VALUE` lines with the same
names and values as the env vars:

```sh
AWS_AI_PROXY_BIND=127.0.0.1:9998
AWS_AI_PROXY_ALLOW=127.0.0.0/8,::1/128
AWS_AI_PROXY_PROFILES=dev,prod
AWS_AI_PROXY_ACCESS_LOGS_ENABLED=true
```

Startup auto-creates `~/.aws-ai-proxy/` as `0700` and the config file as
`0600` with uncommented defaults, and appends missing recognized keys as active
`KEY=VALUE` lines to existing files. Stale `# KEY=` comments do not count as
active config. Do not rewrite user lines.

Access logging is enabled by default at `~/.aws-ai-proxy/access.log`. The
access logger must be the outermost middleware so denied requests are recorded;
never log credential response bodies or secrets.

Keep the parser dependency-free; this is an env file, not a general shell
parser.

The request allowlist is enforced from `RemoteAddr`; do not trust
`X-Forwarded-For` for this direct-connection service.

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
