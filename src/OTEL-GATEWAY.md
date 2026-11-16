# quesma-otel-gateway: collection without a daemon

Claude Code and Codex can export OpenTelemetry. `quesma-otel-gateway` receives it, so a machine
needs a configuration change and no shipper. What it receives goes through the shipper's own
pipeline: scrubbed, sealed with `age` to the organization's recipients and uploaded by presigned
PUT, under the organization's existing Fleet Manager and into its existing bucket.

It is the lighter path, not a replacement. See [what it cannot see](#what-it-cannot-see).

## How it works

```
Claude Code, Codex ──OTLP/HTTP, bearer token──▶ quesma-otel-gateway ──▶ Fleet Manager (enroll, config, tickets)
                                                   │ one hosted install per person
                                                   └──scrub, seal, presigned PUT──▶ the organization's bucket
```

- **Receive.** `POST /v1/logs` and `POST /v1/metrics`, as `application/x-protobuf` or
  `application/json`, gzip or not. `POST /v1/traces` is accepted and discarded. Records from a
  service that is neither agent are dropped.
- **One install per person.** The email an event states names its person, so one person's Claude
  Code and Codex share an install; without an email, the account id does. Each person is a hosted
  install: an ordinary shipper install kept in a directory, enrolled with the organization's
  multi-use grant, listed in Fleet Manager as `otel: <email>`.
- **Spool, then ship.** Events are appended to a per-person file. Every `flush_interval` that file
  is cut into an immutable one and shipped as its own object, under source `otel-claude-code` or
  `otel-codex`. A file is deleted only after its upload is confirmed.
- **Identity apart.** `user.email`, the account ids, `user.id` and `host.name` are removed from
  every event before the scrub and kept once per person in an `otel-identity` object, which is not
  scrubbed: the scrubber's PII rules would otherwise erase the only name this path has.

## Run it

```sh
cd src && go build -o ../bin/quesma-otel-gateway ./cmd/quesma-otel-gateway

bin/quesma-otel-gateway token acme        # prints the token for the agents and its hash for the config
bin/quesma-otel-gateway serve gateway.yaml
```

```yaml
listen: 0.0.0.0:4318
data_dir: /var/lib/quesma-otel-gateway     # the spool and every hosted install's state: keep it on a volume
flush_interval: 5m
tenants:
  - organization: acme
    fleet_manager: https://fleet.example.com
    grant_file: /run/secrets/acme-grant    # a multi-use enrollment grant from Fleet Manager
    token_sha256: [0f3c…]                  # several, so a new token can be rotated in
    email_domains: [acme.com]              # optional: drop events claiming another domain
    max_people: 10000                      # how many installs one leaked token can mint
```

Put TLS in front of it: it listens in plain HTTP. `shipper_config: <path>` names a shipper
`config.yaml` applied to every hosted install, which a local store needs for `upload_targets`.

## Point the agents at it

Claude Code, in managed settings (`managed-settings.json`), so a person cannot turn it off:

```json
{"env": {
  "CLAUDE_CODE_ENABLE_TELEMETRY": "1",
  "OTEL_LOGS_EXPORTER": "otlp",
  "OTEL_METRICS_EXPORTER": "otlp",
  "OTEL_EXPORTER_OTLP_ENDPOINT": "https://otel.example.com",
  "OTEL_EXPORTER_OTLP_HEADERS": "Authorization=Bearer qot1.acme.…",
  "OTEL_METRICS_INCLUDE_REPOSITORY": "true",
  "OTEL_LOG_USER_PROMPTS": "1",
  "OTEL_LOG_TOOL_DETAILS": "1"
}}
```

Codex, in `requirements.toml` or `config.toml`:

```toml
[otel]
log_user_prompt = true
log_agent_responses = true
exporter = { otlp-http = { endpoint = "https://otel.example.com/v1/logs", protocol = "binary", headers = { authorization = "Bearer qot1.acme.…" } } }
```

The last three Claude Code lines and the two Codex `log_` lines are optional. Without them the
gateway still receives every model call with its tokens, tool names, durations and errors, and no
prompt, response, command or file path.

## What the token is

`qot1.<organization>.<secret>` names the organization and nothing else. It lets its holder append
telemetry to that organization and read nothing. It sits in a file every developer can read, so
treat who an event says it is from as a claim: anyone with the token can send events under any
email. `email_domains` and `max_people` narrow that; they do not prove identity. A shipper on the
machine signs every request with its own device key and has no such gap.

## What it cannot see

| | shipper | gateway |
| --- | --- | --- |
| Agents | every catalogued agent | Claude Code and Codex |
| History before it was set up | yes | no |
| A machine that was offline, or an endpoint that was down | ships later | lost: neither agent buffers to disk |
| Tool output | whole | Claude Code: an error's text only. Codex: the first 2 KiB |
| Working directory, branch, memory, plans, settings | yes | no |
| Repository | from the git directory | Claude Code only, with `OTEL_METRICS_INCLUDE_REPOSITORY` |
| Account and subscription usage snapshots | yes | the plan Claude Code states; no usage |
| Plaintext leaves the machine | never | yes, to the gateway, which scrubs it |

That last row decides where the gateway may run: it holds unscrubbed telemetry until it ships, so
run it where the organization's session data is allowed to be.

A session both paths saw is the same session: the ids agree, and a reader that has the transcript
uses it and ignores the telemetry.
