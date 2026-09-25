# Telemetry forwarding

An enrolled shipper posts operational telemetry to `POST /v1/telemetry` on its fleet-manager.
Fleet-manager authenticates the install with the existing `Shipper-Device` scheme, checks the
organization's collector setting, and forwards the event to that collector under a deployment
Ed25519 signature. Event contents are opaque to fleet-manager and are never written to object storage.

## Organization setting

`POST /v1/admin/orgs` and `PUT /v1/admin/orgs/{slug}/config` accept `telemetry_collector_url`;
`GET` returns the effective value.

- Absent (including in records written before this field existed): the deployment's default
  collector, `FLEET_MANAGER_DEFAULT_TELEMETRY_COLLECTOR_URL` (`default_telemetry_collector_url` in
  the Terraform). That is empty unless set, so by default an organization forwards nowhere until
  it names a collector. The default is applied when the setting is read, never written into the
  record, so changing it reaches every organization that left the setting unset.
- Empty string: disabled. Every submission re-reads the setting, so a shipper holding cached
  configuration gets `403 {"error":"telemetry_disabled"}` immediately.
- Omitted or `null` on update: the stored value is preserved.
- A bare hostname (optionally with a port) means `https://<host>/v1/telemetry`. A full HTTPS URL
  is used as given, with `/v1/telemetry` appended when it has no path. HTTP, credentials, queries,
  fragments, and redirects are refused.

Served configuration carries `telemetry_endpoint`: `/v1/telemetry` when enabled, `""` when
disabled. The shipper resolves it against its enrolled fleet-manager origin; the collector URL is
never sent to shippers.

## Shipper request

The body is `{"schema":1,"batch_id":"<uuid>","issued_at":"<RFC3339>","payload":<any JSON>}`,
at most 1 MiB, with no other fields. `issued_at` may be at most five minutes old or one minute
ahead. The device signature covers this ASCII prefix followed by the exact body bytes:

```text
trajectory-shipper-telemetry-v1\nPOST\n/v1/telemetry\n
```

Responses: `204` accepted by the collector; `400` invalid envelope; `403` disabled or revoked;
`413` too large; `429` with `Retry-After` for per-install rate limits or instance capacity;
`502`/`504` for collector failures and the five-second upstream deadline. Collector `400`, `409`,
`413`, and `422` are relayed as-is with `telemetry_rejected`. Collector bodies are never relayed.

## Collector request

Fleet-manager POSTs JSON to the resolved collector URL with `Content-Type: application/json`:

```json
{"schema":1,"fleet_manager_id":"<uuid>","organization":"<slug>","install_id":"<uuid>",
 "batch_id":"<uuid>","forwarded_at":"<RFC3339>","audience":"<resolved collector URL>",
 "shipper_envelope":"<base64 of the original shipper body>"}
```

`Authorization: Fleet-Telemetry key=<key-id>,sig=<base64>` where the key ID is the lowercase
SHA-256 hex of the raw public key and the signature covers this prefix followed by the exact body:

```text
quesma-fleet-telemetry-v1\nPOST\n<escaped request path>\n
```

Collectors should verify the signature against a registered key, check `audience` against their
own configured URL, apply the same timestamp window to `forwarded_at`, and deduplicate on
`(fleet_manager_id, organization, install_id, batch_id)`.

## Signing identity

On startup fleet-manager reads `private/fleet-manager/telemetry-identity.json` from its object
store, creating it with a conditional write when absent so concurrent replicas share one identity.
The object holds `fleet_manager_id` and a base64 Ed25519 `seed`. It lives outside `v1/` so archive
readers cannot see it; do not include it in archive copies. An unreadable or invalid object fails
startup rather than replacing a key the collector already trusts. To rotate, replace the object and
restart every replica, then re-register the public key.

`GET /v1/telemetry/public-key` returns `{"fleet_manager_id","key_id","public_key"}` without
authentication. Terraform reads it and exposes `fleet_manager_public_key`:

```sh
terraform output -raw fleet_manager_public_key
```

## Limits

| Environment variable | Default | Meaning |
| --- | --- | --- |
| `FLEET_MANAGER_TELEMETRY_CONCURRENCY` | 32 | Simultaneous telemetry requests per instance |
| `FLEET_MANAGER_TELEMETRY_RATE` | 60 | Requests per minute per install |
| `FLEET_MANAGER_TELEMETRY_BURST` | 10 | Per-install burst |

Values are integers from 1 to 10000. Limits are per instance and the rate limiter keeps at most
10000 install entries.

## Rollback

Older binaries reject stored configuration containing `telemetry_collector_url`. Reading a record
without the field never writes it back, so only organizations whose setting was explicitly changed
need the field removed before rolling back.
