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
brew install hrubymar10/tap/aws-ai-proxy
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
export AWS_AI_PROXY_NOTIFICATIONS_ENABLED=true
export AWS_AI_PROXY_NOTIFICATION_DEDUP_WINDOW=5m
export AWS_AI_PROXY_AWS_CLI_BINARY_PATH=/opt/homebrew/bin/aws
export AWS_AI_PROXY_AWS_CONFIG_PATH=$HOME/.aws/config
```

Configuration keys:

- `AWS_AI_PROXY_PROFILES` - required comma-separated profile names. Entries may
  use `profile:region` to set an explicit `/profiles` region for that profile.
- `AWS_AI_PROXY_BIND` - bind address, default `127.0.0.1:9998`. The host must
  be an IP literal. Use `0.0.0.0:9998` or `[::]:9998` only with a tight
  allowlist.
- `AWS_AI_PROXY_ALLOW` - comma-separated source IP/CIDR allowlist, default
  `127.0.0.0/8,::1/128`. Requests are checked against `RemoteAddr`;
  `X-Forwarded-For` is not trusted.
- `AWS_AI_PROXY_ACCESS_LOGS_ENABLED` - access logging to
  `~/.aws-ai-proxy/access.log`, default `true`. Set to `false`, `0`, `no`, or
  `off` to disable.
- `AWS_AI_PROXY_NOTIFICATIONS_ENABLED` - OS notifications for successful
  credential requests, default `true`. Set to `false`, `0`, `no`, or `off` to
  disable. macOS uses `osascript`; Linux uses `notify-send` when available.
- `AWS_AI_PROXY_NOTIFICATION_DEDUP_WINDOW` - throttle repeat notifications for
  the same client and profile, default `5m`. Accepts a Go duration (e.g. `30s`,
  `5m`, `1h`). Set to `0` to notify on every request. Credential serving and the
  access/`OK` logs are never throttled - only the notification is.
- `AWS_AI_PROXY_AWS_CLI_BINARY_PATH` - optional absolute path to the `aws` CLI
  binary. When unset, `aws-ai-proxy` uses `PATH`, then common install paths:
  `/opt/homebrew/bin/aws`, `/usr/local/bin/aws`, and `/usr/bin/aws`. A leading
  `~` is expanded to your home directory.
- `AWS_AI_PROXY_AWS_CONFIG_PATH` - optional path passed to `aws` subprocesses as
  `AWS_CONFIG_FILE`. Set this when a launcher such as `brew services` has a
  different `HOME` than your interactive shell. This may point at the config
  file, such as `~/.aws/config`, or the `.aws` directory; when it points at an
  existing directory, `aws-ai-proxy` appends `config`. A leading `~` is expanded
  to your home directory.

If an environment variable is unset, `aws-ai-proxy` falls back to
`~/.aws-ai-proxy/config`. This file uses `KEY=VALUE` lines with the same names
and values as the environment variables:

```sh
AWS_AI_PROXY_BIND=127.0.0.1:9998
AWS_AI_PROXY_ALLOW=127.0.0.0/8,::1/128
AWS_AI_PROXY_PROFILES=my-readonly,my-test-readonly
AWS_AI_PROXY_ACCESS_LOGS_ENABLED=true
AWS_AI_PROXY_NOTIFICATIONS_ENABLED=true
AWS_AI_PROXY_NOTIFICATION_DEDUP_WINDOW=5m
AWS_AI_PROXY_AWS_CLI_BINARY_PATH=
AWS_AI_PROXY_AWS_CONFIG_PATH=
```

Environment variables win per field. On startup, `aws-ai-proxy` creates
`~/.aws-ai-proxy/config` with uncommented defaults if it is missing. Existing
files are append-only: uncommented defaults are appended for any recognized key
that is not already present as an active `KEY=` line, and existing lines are
never rewritten.

Access log lines contain UTC timestamp, client IP, method, path, and response
status. Response bodies and credentials are never logged.
Runtime warnings and errors are also written to `~/.aws-ai-proxy/error.log`.

The Homebrew service does not read your interactive shell profile and may have
a minimal `PATH`, so use `~/.aws-ai-proxy/config` for required values and set
`AWS_AI_PROXY_AWS_CLI_BINARY_PATH` if the service cannot find `aws`.

The host must have an active session for each profile, for example:

```sh
aws sso login --profile my-readonly
```

## Usage

Show help:

```sh
aws-ai-proxy
```

Run the server (recommended; runs in the background and restarts on login):

```sh
brew services start aws-ai-proxy
```

Or run it in the foreground for development or non-Homebrew installs:

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

## Endpoints

- `GET /health`
- `GET /version`
- `GET /profiles`
- `GET /credentials/{profile}`

`/profiles` returns JSON sorted by profile name:

```json
[{"name":"my-readonly","region":"us-east-1"}]
```

Region precedence is `profile:region` from `AWS_AI_PROXY_PROFILES`, then
`aws configure get region --profile <profile>`, then an empty string.

`/credentials/{profile}` shells out on the host:

```sh
aws configure export-credentials --profile <profile>
```

When notifications are enabled, a successful credential response also sends an
OS notification. By default the notification body is `Profile "abc" was
requested`. Callers may send `X-Aws-Ai-Proxy-Client` to identify themselves;
then the body becomes `Profile "abc" was requested by xyz`. If that header is
absent, the proxy falls back to `User-Agent`. The client value is trimmed,
stripped of control characters, and capped before logging or notification.

Because the AWS CLI invokes `credential_process` on nearly every API call, a
busy client would otherwise flood Notification Center. Repeat notifications for
the same client and profile are therefore throttled to one per
`AWS_AI_PROXY_NOTIFICATION_DEDUP_WINDOW` (default `5m`; set `0` to disable). The
throttle applies only to the notification - credentials are always served and
every request is still recorded in the access and `OK` logs.

The proxy resolves the `aws` binary from `AWS_AI_PROXY_AWS_CLI_BINARY_PATH`,
then `PATH`, then common install paths. When `AWS_AI_PROXY_AWS_CONFIG_PATH` is
set, the proxy passes it to this command as `AWS_CONFIG_FILE`. The command also
inherits the launcher environment, including `AWS_SHARED_CREDENTIALS_FILE` when
set.

## Development

```sh
make test
make vet
make build
```
