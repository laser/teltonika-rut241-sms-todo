# Teltonika RUT241 SMS Todo

[![Check](https://github.com/laser/teltonika-rut241-sms-todo/actions/workflows/check.yml/badge.svg)](https://github.com/laser/teltonika-rut241-sms-todo/actions/workflows/check.yml)

Teltonika RUT241 SMS Todo is a multi-user todo service controlled entirely by
SMS. It polls a Teltonika RUT241 inbox over HTTPS, keeps each phone number's
todos separate, and sends each user a daily todo list in their configured time
zone.

The application is a single Go binary with no runtime dependencies. It is
designed to run under systemd on a Raspberry Pi.

## SMS commands

Add a todo by sending its text with no command prefix:

```text
call mom
```

Every successful addition replies with the complete updated todo list. For
example, sending `do dishes` after the first item returns:

```text
0: call mom
1: do dishes
```

Complete and remove a todo by sending only its number:

```text
0
```

The service replies with the complete updated list:

```text
1: do dishes
```

If the completed item was the last todo, the reply is `No todos.`

Renumber the current list and fill numbering gaps by sending:

```text
gc
```

The `gc` command preserves list order, assigns numbers starting at 0, and
replies with the updated list. The next added todo receives the number after
the last list item. Because `gc` can reuse numbers, avoid sending it while an
older numeric completion message is still in transit.

Leading and trailing whitespace is ignored. Apart from the exact `gc` command,
a non-empty, non-numeric message is always todo text; `add` has no special
meaning. Messages from phone numbers not listed in the configuration are
ignored, preventing unknown senders from viewing data or triggering reply
charges.

Carrier `Unable to send/receive message - Message Blocking is active` notices
are also ignored, even when they appear to come from a configured user. They
are recorded as processed without adding a todo or sending a reply, preventing
an SMS feedback loop. Ordinary todo text that mentions message blocking is
still accepted.

## Daily lists and long messages

Each user has an IANA time zone and a local delivery time. For example:

```json
{
  "phone_number": "+12065550123",
  "daily_time": "08:00",
  "timezone": "America/Los_Angeles"
}
```

The time zone database is embedded in the binary, and daylight-saving changes
are handled automatically. If the service starts after the configured time,
it sends that day's list once after startup. An empty list is sent as
`No todos.`

Long lists are split into multiple numbered messages such as `(1/3)`, `(2/3)`,
and `(3/3)`. The service prefers to split between todo lines and falls back to
splitting long individual lines when necessary.

## State

All runtime state is stored in one JSON file. It contains separate todo arrays
for each configured phone number, the next todo number, processed RUT241
message identifiers, and daily-delivery bookkeeping.

The file is replaced atomically and written with mode `0600`. The default
systemd location is:

```text
/var/lib/teltonika-rut241-sms-todo/state.json
```

The Go implementation reads the same state format as the original Python
implementation. On the first run with a new state file, the service records the
existing RUT241 inbox as a baseline without executing historical messages.
Only messages that arrive afterward are treated as commands.

## Requirements

- Go 1.23 or newer to build the application
- A Teltonika RUT241 reachable over HTTPS
- A registered SIM capable of sending and receiving SMS
- The RUT241 HTTPS API enabled for the host network

The compiled application has no external runtime dependencies.

## Local setup

Create a working configuration:

```bash
cp config.example.json config.json
```

Edit `config.json` and set the router address, state file, and users. For a
local run, set `state_file` to `./state.json`. Keep phone numbers in E.164
format, such as `+12065550123`.

Build and test the application:

```bash
just check
just build-macos
```

`just build-macos` creates Apple Silicon and Intel binaries under `dist/`.
Run `just --list` to see all available build and release targets. Direct Go
commands also work if `just` is not installed.

Validate the configuration:

```bash
./teltonika-rut241-sms-todo -config config.json check-config
```

Set the router password in the environment, then run one polling and scheduling
cycle:

```bash
export RUT241_PASSWORD='your-router-password'
./teltonika-rut241-sms-todo -config config.json once
```

Run continuously in the foreground:

```bash
./teltonika-rut241-sms-todo -config config.json run
```

Logs are written to standard output. Stop the service with `Ctrl-C`.

## Configuration reference

| Setting | Meaning |
| --- | --- |
| `router.url` | RUT241 HTTPS URL |
| `router.username` | RUT241 WebUI/API username |
| `router.password_env` | Environment variable containing its password |
| `router.modem_id` | Modem used for SMS, normally `1-1` |
| `router.verify_tls` | Verify the router TLS certificate when `true` |
| `state_file` | Persistent JSON state path |
| `poll_interval_seconds` | Inbox polling interval |
| `max_todo_length` | Maximum text length accepted for a todo |
| `users[].phone_number` | User identity and SMS destination in E.164 format |
| `users[].daily_time` | Local daily time in 24-hour `HH:MM` format |
| `users[].timezone` | IANA time zone, such as `America/Los_Angeles` |

The router password is intentionally excluded from JSON configuration. The
service reads it from the environment variable named by `password_env`.

## Cross-compile for Raspberry Pi 3

Build a static 32-bit ARMv7 binary on macOS:

```bash
just build-pi
```

The resulting binary runs on the Raspberry Pi without installing Go or
upgrading the Pi's Python version. It is written to
`dist/teltonika-rut241-sms-todo-linux-armv7`.

Create a Raspberry Pi release archive containing the binary, configuration
example, systemd files, and README:

```bash
just package
```

Publish that archive and its SHA-256 checksum as a GitHub Release:

```bash
VERSION=v0.1.0 just release
```

The release target requires the GitHub CLI to be installed and authenticated.
It refuses to publish unless the working tree is clean and the current commit
matches `origin/main`. The GitHub CLI creates the release tag on `main` and
generates release notes automatically.

On the Raspberry Pi, install the GitHub CLI and download a release manually,
or download the ARMv7 package directly from the repository's Releases page.
For example, with the GitHub CLI available:

```bash
gh release download v0.1.0 \
  --repo laser/teltonika-rut241-sms-todo \
  --pattern '*-linux-armv7.tar.gz*'
```

## Raspberry Pi and systemd installation

Copy the cross-compiled binary, configuration template, environment template,
and service unit to the Pi. On the Pi, install them with:

```bash
sudo mkdir -p /opt/teltonika-rut241-sms-todo
sudo install -m 755 teltonika-rut241-sms-todo \
  /opt/teltonika-rut241-sms-todo/teltonika-rut241-sms-todo

sudo install -o root -g pi -m 640 config.example.json \
  /etc/teltonika-rut241-sms-todo.json
sudo install -o root -g root -m 600 \
  systemd/teltonika-rut241-sms-todo.env.example \
  /etc/teltonika-rut241-sms-todo.env
sudo install -m 644 systemd/teltonika-rut241-sms-todo.service \
  /etc/systemd/system/teltonika-rut241-sms-todo.service

sudo mkdir -p /var/lib/teltonika-rut241-sms-todo
sudo chown pi:pi /var/lib/teltonika-rut241-sms-todo
sudo chmod 700 /var/lib/teltonika-rut241-sms-todo
```

Edit the two configuration files:

```bash
sudoedit /etc/teltonika-rut241-sms-todo.json
sudoedit /etc/teltonika-rut241-sms-todo.env
```

Validate before starting the daemon:

```bash
sudo /opt/teltonika-rut241-sms-todo/teltonika-rut241-sms-todo \
  -config /etc/teltonika-rut241-sms-todo.json check-config
```

Enable and start the service:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now teltonika-rut241-sms-todo.service
```

Inspect status and follow logs:

```bash
systemctl status teltonika-rut241-sms-todo.service
journalctl -u teltonika-rut241-sms-todo.service -f
```

The supplied unit runs as the unprivileged `pi` user and uses protections
supported by the Pi's older systemd release.

## Testing

Run the test suite and static checks:

```bash
go test ./...
go vet ./...
```

The tests cover per-user isolation, numeric completion commands, todo
renumbering, time-zone scheduling, once-per-day delivery, inbox baselining,
idempotent command processing, atomic JSON persistence, state compatibility,
and SMS chunking.

## Operational notes

- If carrier error notices were previously added as todos, stop the service,
  back up the state file, and remove only those error entries before restarting
  with the filter installed. Preserve real todos, their numbers, processed
  message keys, and daily-delivery bookkeeping. Check the SIM provider's SMS
  service and balance separately; the application cannot clear a carrier block.
- The service records command state before sending a reply, so restarting it
  cannot apply the same inbound command twice.
- The RUT241 API does not offer SMS idempotency keys. Daily delivery attempts
  are recorded before sending to avoid duplicate reminders after an ambiguous
  network failure.
- Reserve the RUT241's Wi-Fi address in the upstream DHCP server so its API URL
  remains stable.
- Keep cellular packet data disabled on the RUT241 if the SIM should be used
  only for SMS. The modem must remain registered with the carrier.
