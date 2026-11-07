# Ptah Spanner provider

This repository holds the executable provider that serves Cloud Spanner's
PostgreSQL interface to [Ptah](https://github.com/stokaro/ptah). Ptah starts it
as a subprocess and talks to it over the experimental provider protocol
([Ptah #4204](https://github.com/stokaro/ptah/issues/4204)). The provider is an
optional installation: Ptah recognizes the `spanner` engine without it and
explains where to get it.

The provider reaches Spanner through PGAdapter, which speaks the PostgreSQL
protocol in front of a Spanner database. PGAdapter is a separate component:
installing the provider installs no PGAdapter and provisions no database.

**Support is best effort.** The live tests run the Cloud Spanner emulator behind
PGAdapter (`gcr.io/cloud-spanner-pg-adapter/pgadapter-emulator:v0.56.2`), which
is evidence about the PostgreSQL interface, not about the managed service.

## What the provider does

- **Sessions.** An address is `spanner://host:port/database`, with optional
  query parameters the PostgreSQL driver reads, such as `sslmode`. The schemes
  `cloudspanner://` and `google-spanner://` are the same. A reason the provider
  writes never quotes the address.
- **Reads.** A read describes the base tables of the requested schemas, or of
  `public` when the request names none: every column with the type Spanner
  stores, its nullability and its stored default, the primary key in key order,
  and the secondary indexes with each key's direction and NULLS ordering and
  their `STORING` columns. The index Spanner builds for a foreign key is
  Spanner's, not the user's, and is left out.
- **Refusals instead of omissions.** A read fails, with a reason that names the
  object, when the schema holds something the provider does not describe yet:
  a view, a foreign key or CHECK constraint, a row deletion policy, a generated
  or identity column (Spanner adds a hidden `rowid` identity column to a table
  created without a primary key), a filtered, search or interleaved index, or an
  interleaved table. Ptah plans to create what a read leaves out, so leaving
  such an object out would plan a change the database refuses.
- **Plans.** Ptah plans the engine's tables with its common planner, and the
  provider writes each change: `CREATE TABLE`, `DROP TABLE`, and `ALTER TABLE`
  to add or drop a column. A column type Spanner refuses (`smallint`, `int4`,
  `timestamp`, `json`, `numeric(p,s)` and others) is refused before anything is
  sent, as is a `NOT NULL` column added without a default.
- **Execution.** Spanner refuses a schema statement inside an explicit
  transaction, so each statement commits as it runs. A statement Spanner refuses
  applied nothing and the statements before it stay applied; the result says how
  many applied. A call that ends while a statement runs reports the outcome as
  unknown, because Spanner keeps applying a schema change its client stopped
  waiting for.
- **Locks.** `ptah schema apply` takes a lock before it reads the target.
  Spanner has no session lock, so a lock is a row of the table `ptah_locks`,
  which the provider creates on first use. The row names its holder and the end
  of a 30-second lease that every check extends. A holder that dies keeps the
  lock until its lease ends. A read reserves the table, so Ptah never plans its
  removal or writes it into an export.

What is not served yet, in the order it is planned: column changes and indexes
through the provider's own table planning, foreign keys and CHECK constraints,
the row deletion policy, views and sequences, and versioned migrations. The
plan and the measurements behind it are kept with #4204.

## Use it

Build the provider and describe it in a provider manifest:

```sh
GOWORK=off CGO_ENABLED=0 go build -o bin/ptah-plugin-spanner ./cmd/ptah-plugin-spanner
shasum -a 256 bin/ptah-plugin-spanner
```

```json
{
  "format_version": 2,
  "providers": [{
    "executable": "/absolute/path/to/bin/ptah-plugin-spanner",
    "sha256": "<the digest>",
    "provider_id": "github.com/stokaro/ptah-spanner-plugin",
    "engines": [{"id": "spanner", "aliases": ["cloudspanner", "google-spanner", "google_spanner"]}]
  }]
}
```

Then select it explicitly:

```sh
ptah providers check --provider-config providers.json --provider-engine spanner --json
ptah schema apply --db-url 'spanner://localhost:5432/app?sslmode=disable&search_path=public' \
  --root-dir ./entities --provider-config providers.json --provider-engine spanner
```

A Ptah release that still builds in a Spanner engine of its own reads an
address that pins no `search_path` as a read of every schema, through its
built-in catalog. Pin `search_path=public` in the address until the built-in
engine is removed.

## Build and test

The module pins the published SDK, `github.com/stokaro/ptah/provider-sdk`, by
tag. It needs neither the Ptah source nor a local SDK checkout and has no
`replace` directive.

```sh
GOWORK=off go test -race ./...
GOWORK=off make lint
```

The live tests need a Spanner emulator behind PGAdapter. Each test takes a
database of its own on the endpoint, which the emulator creates on the first
connection:

```sh
docker run -d --name spanner -p 5432:5432 gcr.io/cloud-spanner-pg-adapter/pgadapter-emulator:v0.56.2
PTAH_SPANNER_TEST_URL='spanner://127.0.0.1:5432/ptah_test?sslmode=disable' \
PTAH_HOST_TEST_BIN=/path/to/ptah \
  GOWORK=off go test -tags integration -race -count=1 ./integration
```

`PTAH_HOST_TEST_BIN` names a `ptah` binary for the tests that drive the host;
without it they are skipped. CI builds the newest Ptah release whose contract
the provider speaks, and Ptah's master branch. A draft pull request that needs
an unmerged host change names the host branch in `.github/host-ref`, which
must be removed before the pull request leaves draft.

## Releases

A tag that starts with `v` runs `.github/workflows/release.yml`, which
publishes a GitHub release through GoReleaser (`.goreleaser.yaml`). Each
release carries one archive per platform, `checksums.txt`,
`release-lines.json`, and `ptah-provider.json`, which a host reads to choose
the newest release whose minimum provider contract it meets:

```text
ptah-plugin-spanner_<version>_<os>_<arch>.tar.gz   (.zip on Windows)
checksums.txt
release-lines.json
ptah-provider.json
```

`<version>` is the tag without the leading `v`. The platforms are linux,
darwin and windows on amd64 and arm64. The binaries are built with
`CGO_ENABLED=0` and `-trimpath`. `checksums.txt` lists the SHA-256 digest of
every archive and document in `sha256sum` format.

## License

MIT. See `LICENSE`.
