# aws-ai-proxy

`aws-ai-proxy` is a host-side HTTP proxy that exposes AWS credentials for
explicitly allowlisted local AWS profiles. It is used by Docker sandboxes such
as `claude-docker`, `codex-docker`, and `pi-docker` so containers do not need
direct access to host AWS files.

By default the proxy binds to `127.0.0.1:9998` and only accepts loopback
clients. Set `AWS_AI_PROXY_BIND` and `AWS_AI_PROXY_ALLOW` when the proxy should
serve sandboxes from another machine.

## Install

From source:

```sh
go install github.com/hrubymar10/aws-ai-proxy/cmd/aws-ai-proxy@latest
```

With Homebrew:

```sh
brew tap hrubymar10/tap
brew trust hrubymar10/tap
brew install --HEAD aws-ai-proxy
```

During development, use the wrapper:

```sh
bin/aws-ai-proxy --version
AWS_AI_PROXY_FORCE_BUILD=1 bin/aws-ai-proxy --version
```

## Configuration

Set configuration in your shell profile, such as `~/.zshrc` or `~/.bashrc`,
or export it before starting the proxy:

```sh
export AWS_AI_PROXY_PROFILES=my-readonly,my-test-readonly
export AWS_AI_PROXY_BIND=127.0.0.1:9998
export AWS_AI_PROXY_ALLOW=127.0.0.0/8,::1/128
export AWS_AI_PROXY_ACCESS_LOGS_ENABLED=true
```

Configuration keys:

- `AWS_AI_PROXY_PROFILES` - required comma-separated profile names.
- `AWS_AI_PROXY_BIND` - bind address, default `127.0.0.1:9998`. The host must
  be an IP literal. Use `0.0.0.0:9998` or `[::]:9998` only with a tight
  allowlist.
- `AWS_AI_PROXY_ALLOW` - comma-separated source IP/CIDR allowlist, default
  `127.0.0.0/8,::1/128`. Requests are checked against `RemoteAddr`;
  `X-Forwarded-For` is not trusted.
- `AWS_AI_PROXY_ACCESS_LOGS_ENABLED` - access logging to
  `~/.aws-ai-proxy/access.log`, default `true`. Set to `false`, `0`, `no`, or
  `off` to disable.

If an environment variable is unset, `aws-ai-proxy` falls back to
`~/.aws-ai-proxy/config`. This file uses `KEY=VALUE` lines with the same names
and values as the environment variables:

```sh
AWS_AI_PROXY_BIND=127.0.0.1:9998
AWS_AI_PROXY_ALLOW=127.0.0.0/8,::1/128
AWS_AI_PROXY_PROFILES=my-readonly,my-test-readonly
AWS_AI_PROXY_ACCESS_LOGS_ENABLED=true
```

Environment variables win per field. On startup, `aws-ai-proxy` creates
`~/.aws-ai-proxy/config` with uncommented defaults if it is missing. Existing
files are append-only: uncommented defaults are appended for any recognized key
that is not already present as an active `KEY=` line, and existing lines are
never rewritten.

Access log lines contain UTC timestamp, client IP, method, path, and response
status. Response bodies and credentials are never logged.

The Homebrew service does not read your interactive shell profile, so use
`~/.aws-ai-proxy/config` when running the proxy through `brew services`.

The host must have an active session for each profile, for example:

```sh
aws sso login --profile my-readonly
```

## Usage

Show help:

```sh
aws-ai-proxy
```

Run the server:

```sh
aws-ai-proxy serve
```

Check and stop it:

```sh
aws-ai-proxy status
aws-ai-proxy stop
```

The docker projects are consumers only. Start `aws-ai-proxy` independently
with the binary, the development wrapper, or the Homebrew service, then enable
the proxy in each docker project with `AWS_AI_PROXY_ENABLED=1` and
`AWS_AI_PROXY_URL`.

```sh
AWS_AI_PROXY_ENABLED=1 AWS_AI_PROXY_URL=http://host.docker.internal:9998 \
  ../codex-docker/bin/codex-docker-ctrl start
```

With Homebrew:

```sh
brew services start aws-ai-proxy
```

## Endpoints

- `GET /health`
- `GET /version`
- `GET /profiles`
- `GET /credentials/{profile}`

`/profiles` returns JSON sorted by profile name:

```json
[{"name":"my-readonly","region":"us-east-1"}]
```

`/credentials/{profile}` shells out on the host:

```sh
aws configure export-credentials --profile <profile>
```

The command inherits the launcher environment, including `AWS_CONFIG_FILE` and
`AWS_SHARED_CREDENTIALS_FILE` when those are set.

## Development

```sh
make test
make vet
make build
```
