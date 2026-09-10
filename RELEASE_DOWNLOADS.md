# Release downloads

This document describes the public release repository at `updates.quesma.dev` and the proposed
friendly download endpoints. It is both an operator guide and a statement of the trust boundary for
people downloading Quesma Shipper.

The TUF repository is live. The friendly `/download/` endpoints described below are not live until
the Worker route is deployed and verified. Do not publish those links in installation instructions
before that point.

## Public release repository

Release artifacts and [The Update Framework (TUF)](https://theupdateframework.io/) metadata are
stored in a public Cloudflare R2 bucket exposed through `https://updates.quesma.dev`. Public bucket
access does not provide a directory listing, so a `404` at the origin or at `/targets/` is expected.

The stable public entry points are:

- `https://updates.quesma.dev/metadata/1.root.json` — the initial offline-signed root of trust.
- `https://updates.quesma.dev/metadata/timestamp.json` — the current timestamp metadata.

The timestamp identifies the versioned snapshot, the snapshot identifies the versioned targets
metadata, and the targets metadata contains the length and SHA-256 hash of each released file. With
consistent snapshots enabled, their public URLs have these forms:

```text
https://updates.quesma.dev/metadata/<version>.snapshot.json
https://updates.quesma.dev/metadata/<version>.targets.json
https://updates.quesma.dev/targets/<sha256>.<target-name>
```

`release.json` is itself a TUF target. It records the release version and maps platform identifiers
such as `linux/amd64` to target names. Consumers should use that manifest instead of assuming that a
binary's logical name never changes.

The hash-addressed target URLs are immutable but not stable aliases: a new artifact has a new hash
and therefore a new URL. They are appropriate for the updater and diagnostics, but not for a
permanent link in the README.

## Friendly download URLs

The public, stable download URLs will be:

```text
https://updates.quesma.dev/download/quesma-shipper-macos-universal.pkg
https://updates.quesma.dev/download/quesma-shipper-linux-amd64
https://updates.quesma.dev/download/quesma-shipper-linux-arm64
https://updates.quesma.dev/download/quesma-shipper-windows-amd64.exe
https://updates.quesma.dev/download/quesma-shipper-windows-arm64.exe
https://updates.quesma.dev/download/QuesmaShipperSetup-amd64.exe
https://updates.quesma.dev/download/QuesmaShipperSetup-arm64.exe
```

Each endpoint resolves `timestamp.json`, the referenced snapshot and targets metadata, and
`release.json`, then returns a temporary redirect to the current hash-addressed target. The Worker
does not copy, rename, overwrite, or delete any TUF object. No Worker deployment is needed for an
ordinary Shipper release.

Use a `302` response and do not cache it permanently. A `301` can leave browsers and intermediate
caches pointing at an older release.

## Trust boundary

The installed updater verifies the TUF signatures, expiry, rollback protection, target length, and
target hash against the root embedded in the binary. That remains the authenticated update path.

The Worker below parses public TUF metadata for discovery, but it does **not** verify its signatures.
A browser following its redirect is not a TUF client. Manual downloads therefore trust HTTPS, the
Cloudflare configuration, and any platform-specific code signing. The macOS package is signed and
notarized. The Linux and Windows release jobs do not currently apply an operating-system code
signature; their binaries become authenticated when a TUF client verifies them.

Do not describe a manual browser download as “TUF verified.” If manual downloads need the same
trust guarantee, provide a small installer that embeds the trusted root and performs the complete
TUF client workflow.

The Worker also must not receive the TUF signing key, R2 API credentials, or CI publication
credentials. The route needs read access only to URLs that are already public.

## Worker implementation

Create a Worker with the following TypeScript entry point:

```ts
const downloads: Record<string, string> = {
  "/download/quesma-shipper-macos-universal.pkg": "darwin/pkg",
  "/download/quesma-shipper-linux-amd64": "linux/amd64",
  "/download/quesma-shipper-linux-arm64": "linux/arm64",
  "/download/quesma-shipper-windows-amd64.exe": "windows/amd64",
  "/download/quesma-shipper-windows-arm64.exe": "windows/arm64",
  "/download/QuesmaShipperSetup-amd64.exe": "windows/amd64/setup",
  "/download/QuesmaShipperSetup-arm64.exe": "windows/arm64/setup",
};

type Metadata = {
  signed?: {
    meta?: Record<string, { version?: unknown }>;
    targets?: Record<string, { hashes?: { sha256?: unknown } }>;
  };
};

type Release = {
  targets?: Record<string, unknown>;
};

async function readJSON<T>(origin: string, path: string): Promise<T> {
  const response = await fetch(`${origin}/${path}`);
  if (!response.ok) {
    throw new Error(`${path}: HTTP ${response.status}`);
  }
  return response.json() as Promise<T>;
}

function version(value: unknown, label: string): number {
  if (
    typeof value !== "number" ||
    !Number.isSafeInteger(value) ||
    value < 1
  ) {
    throw new Error(`invalid ${label}`);
  }
  return value;
}

function hash(value: unknown, label: string): string {
  if (typeof value !== "string" || !/^[0-9a-f]{64}$/.test(value)) {
    throw new Error(`invalid ${label}`);
  }
  return value;
}

export default {
  async fetch(request: Request): Promise<Response> {
    if (request.method !== "GET" && request.method !== "HEAD") {
      return new Response("Method not allowed", {
        status: 405,
        headers: { Allow: "GET, HEAD" },
      });
    }

    const url = new URL(request.url);
    const platform = downloads[url.pathname];
    if (!platform) {
      return new Response("Unknown download", { status: 404 });
    }

    try {
      const timestamp = await readJSON<Metadata>(
        url.origin,
        "metadata/timestamp.json",
      );
      const snapshotVersion = version(
        timestamp.signed?.meta?.["snapshot.json"]?.version,
        "snapshot version",
      );

      const snapshot = await readJSON<Metadata>(
        url.origin,
        `metadata/${snapshotVersion}.snapshot.json`,
      );
      const targetsVersion = version(
        snapshot.signed?.meta?.["targets.json"]?.version,
        "targets version",
      );

      const targets = await readJSON<Metadata>(
        url.origin,
        `metadata/${targetsVersion}.targets.json`,
      );
      const releaseHash = hash(
        targets.signed?.targets?.["release.json"]?.hashes?.sha256,
        "release manifest hash",
      );
      const release = await readJSON<Release>(
        url.origin,
        `targets/${releaseHash}.release.json`,
      );

      const targetName = release.targets?.[platform];
      if (
        typeof targetName !== "string" ||
        !/^[A-Za-z0-9._-]+$/.test(targetName)
      ) {
        throw new Error(`invalid target for ${platform}`);
      }

      const targetHash = hash(
        targets.signed?.targets?.[targetName]?.hashes?.sha256,
        "target hash",
      );
      const location = `${url.origin}/targets/${targetHash}.${targetName}`;

      return new Response(null, {
        status: 302,
        headers: {
          Location: location,
          "Cache-Control": "no-store",
        },
      });
    } catch (error) {
      console.error(error);
      return new Response("Release temporarily unavailable", { status: 503 });
    }
  },
};
```

The Worker deliberately reads the public origin rather than using an R2 binding. Because its route
matches only `/download/*`, requests to `/metadata/*` and `/targets/*` continue to the existing R2
origin. This avoids giving the Worker credentials or the ability to mutate the release bucket.

The `release.json` file is an ordinary JSON object, unlike signed TUF metadata. The separate type
and validation in the example prevent accidentally treating it as signed metadata.

## Cloudflare setup

The `quesma.dev` zone and the R2 custom domain must be in an account where Workers can add a route.
The existing `updates.quesma.dev` DNS record must remain proxied through Cloudflare.

1. In **Workers & Pages**, create a Worker named `quesma-release-downloads`.
2. Paste or deploy the implementation above. It needs no variables, secrets, or R2 binding.
3. Under the Worker's **Settings > Domains & Routes**, add a route rather than a Worker Custom
   Domain.
4. Set the route to `updates.quesma.dev/download/*` in the `quesma.dev` zone.
5. Configure the route to fail closed. Failure must produce an error rather than silently treating
   `/download/*` as an R2 object path.
6. Leave the existing R2 custom domain in place. It continues serving every unmatched path.

The equivalent Wrangler configuration is:

```toml
name = "quesma-release-downloads"
main = "src/index.ts"
compatibility_date = "2026-09-09"
compatibility_flags = ["global_fetch_strictly_public"]

routes = [
  { pattern = "updates.quesma.dev/download/*", zone_name = "quesma.dev" }
]
```

Do not add an `r2_buckets` binding or commit account IDs, API tokens, bucket names, or credentials.
The compatibility flag permits the Worker to fetch public URLs in the same Cloudflare zone. It does
not grant access to private resources. Without it, a same-zone subrequest can fail with Cloudflare
error `1042`.
Cloudflare documents path-specific
[Worker routes](https://developers.cloudflare.com/workers/configuration/routing/routes/) and public
[R2 custom domains](https://developers.cloudflare.com/r2/buckets/public-buckets/).

## Deployment checks

Test the metadata chain before deploying the route:

```sh
curl -fsS https://updates.quesma.dev/metadata/1.root.json >/dev/null
curl -fsS https://updates.quesma.dev/metadata/timestamp.json >/dev/null
```

After deployment, check every friendly endpoint. Each response must be `302`; its `Location` must
start with `https://updates.quesma.dev/targets/`, and the target request must return `200`:

```sh
curl -fsSI https://updates.quesma.dev/download/quesma-shipper-linux-amd64
curl -fsSL -o /dev/null https://updates.quesma.dev/download/quesma-shipper-linux-amd64
```

Also test both architectures on Linux and Windows and install the macOS package on a supported Mac.
Only after all five endpoints pass should the README switch from hash-addressed URLs to the friendly
URLs.

The release workflow publishes targets first, versioned metadata second, and `timestamp.json` last.
Preserve that order: the Worker reads the timestamp first, so it sees either the complete previous
release or the complete new release.

## Operations and rollback

- Monitor Worker `5xx` responses and the expiry of TUF metadata.
- Keep redirects uncached while validating the service. A short cache lifetime such as 60 seconds
  can be considered later.
- Worker requests and R2 reads count against their respective Cloudflare allowances. R2 egress is
  currently free; consult the official
  [Workers pricing](https://developers.cloudflare.com/workers/platform/pricing/) and
  [R2 pricing](https://developers.cloudflare.com/r2/pricing/) pages rather than copying prices into
  this repository.
- To roll back, remove or disable only the `updates.quesma.dev/download/*` Worker route. Do not
  remove the R2 custom domain or change `/metadata/*` or `/targets/*`; installed clients depend on
  them.
