---
title: Using the Spanner provider with Ptah
description: How a spanner:// address, a dev database and ptah-compat reach Spanner's PostgreSQL interface through this provider.
type: how-to
audience:
  - database-engineer
readerQuestion: "How do ptah and ptah-compat connect to Spanner through this provider?"
goal: "Connect Ptah to a Spanner database through PGAdapter and choose the database a run rehearses on."
sourceOfTruth:
  - internal/address
  - internal/backend/sessions.go
  - internal/backend/identity.go
  - internal/backend/capabilities.go
  - internal/backend/devclean.go
  - cmd/ptah-plugin-spanner
  - stokaro/ptah#4204
generated: false
searchAliases:
  - "spanner:// URL"
  - "Cloud Spanner PostgreSQL interface"
  - "PGAdapter"
overlaps: []
disposition: keep
sidebar:
  order: 1
---

# Using the Spanner provider with Ptah

Ptah reaches Spanner through this provider, provider ID
`github.com/stokaro/ptah-spanner-plugin`. The provider serves Spanner's
PostgreSQL interface: it reads a live database, plans every table change,
renders and runs the statements, keeps the migration history and the migration
lock, lints migrations, writes declared rows, runs seed files and empties dev
databases. Ptah keeps the shared pipeline: the source formats, the comparison
and ordering of a plan, the migration policy and the command surfaces.

The engine name is `spanner`, with the aliases `cloudspanner`, `google-spanner`
and `google_spanner`. A `spanner://`, `cloudspanner://` or `google-spanner://`
address and `--dialect spanner` name it. The provider writes PostgreSQL-dialect
statements only; it renders no GoogleSQL.

## Support level

Support is best effort. The provider's live tests run the Cloud Spanner
emulator behind PGAdapter, image
`gcr.io/cloud-spanner-pg-adapter/pgadapter-emulator:v0.56.2`. That is evidence
about the PostgreSQL interface, not about the managed service, and no test runs
against a managed instance. Review the statements a plan writes before you
apply them to a managed database.

The provider declares one release line, `0`, labeled "Cloud Spanner, PostgreSQL
interface", at the best-effort level. The managed service has no release lines,
and over the PostgreSQL wire the server reports PGAdapter's PostgreSQL
compatibility level, which names no Spanner release.

The provider also states the capability facts Ptah plans the engine with when no
database is connected, as for `ptah sql lint --dialect spanner` or an offline
render. They are the facts Ptah's built-in Spanner engine planned with, most of
them measured on the emulator, and `ptah db capabilities` prints them for a
connected database.

## Before you start

The provider talks to PGAdapter, which speaks the PostgreSQL protocol in front
of a Spanner database. PGAdapter is a separate component: installing the
provider installs no PGAdapter and provisions no database.

For a local database, the emulator image runs the Cloud Spanner emulator and
PGAdapter in one container. The emulator creates a database on the first
connection to it:

```console
docker run -d --name spanner -p 5432:5432 gcr.io/cloud-spanner-pg-adapter/pgadapter-emulator:v0.56.2
```

## Install and select the provider

Install the newest release this Ptah can run:

```console
ptah providers install spanner
```

A command that names a `spanner://` address or the `spanner` dialect then
selects the installed copy without a flag or a manifest, on a Ptah that builds
in no Spanner engine of its own; see
[On a Ptah with a built-in Spanner engine](#on-a-ptah-with-a-built-in-spanner-engine)
for the other case.
[Configure the Spanner provider](https://docs.ptah.run/edge/extend/providers/spanner/)
describes the install checks, how a command selects the provider, and the
refusals a command reports when no usable installation is selected. The
release archives and the minimum contract each release declares are listed in
[Releases](../README.md#releases).

A build you obtained and reviewed yourself is selected through a provider
manifest instead. Record the executable's SHA-256 digest in it:

```json
{
  "format_version": 2,
  "providers": [{
    "executable": "/absolute/path/to/ptah-plugin-spanner",
    "sha256": "<the digest>",
    "provider_id": "github.com/stokaro/ptah-spanner-plugin",
    "engines": [{"id": "spanner", "aliases": ["cloudspanner", "google-spanner", "google_spanner"]}]
  }]
}
```

Check the installation the manifest names:

```console
ptah providers check --provider-config providers.json --provider-engine spanner --json
```

A successful check reports `"status": "compatible"`. A command then takes
`--provider-config providers.json --provider-engine spanner`.

### What the provider needs from Ptah

The provider starts on a Ptah of provider contract 25.27 or a later minor
version of contract 25.
Some of what these pages describe needs more:

| Service | Needs |
| --- | --- |
| Reading, comparing and changing foreign keys and `CHECK` constraints | Provider contract 25.28; an older Ptah gets the read of a table that holds one refused |
| Creating a table with a foreign key or a table `CHECK`, and `ptah db read` | A Ptah that carries [stokaro/ptah#5559](https://github.com/stokaro/ptah/pull/5559) |
| Dev and shadow databases | A Ptah that carries [stokaro/ptah#5565](https://github.com/stokaro/ptah/pull/5565) |
| The down file of a schema whose foreign key is a table constraint | A Ptah that carries [stokaro/ptah#5585](https://github.com/stokaro/ptah/pull/5585) |

## Connecting

An address names PGAdapter's endpoint and the Spanner database behind it:

```text
spanner://localhost:5432/app?sslmode=disable
```

- The host and port are PGAdapter's. The path is the database's name, one
  segment with nothing below it.
- The query string goes to the PostgreSQL driver as written, so the provider
  takes the parameters that driver reads, such as `sslmode` and
  `application_name`. A user name and a password in the address go to PGAdapter.
- `cloudspanner://` and `google-spanner://` are the same address, in any case.

The provider refuses an address with another scheme, no host, no database, a
path below the database or a fragment, with
`a Spanner address is spanner://host:port/database, with an optional query string`.
It also refuses a session whose endpoint reports a database other than the one
the address names. A reason the provider writes never quotes the address,
because the address can carry a password.

### The schemas a read covers

A read covers the schemas Ptah asks for, and `public` when it asks for none.
`--schemas` names more, such as `--schemas public,app` for a declaration that
keeps tables in a schema `app`.

A `search_path` parameter in the address goes to PGAdapter as a session
parameter, which PGAdapter accepts. The provider names the schema of every
catalog query itself, so `search_path` does not change what it reads, and its
own tests run with `search_path=public` pinned.

## Dev and shadow databases

A Spanner database is created through the instance's administration API, not
through SQL, so the provider creates no dev database. A `--dev-url` or
`--shadow-db` address names a Spanner database that exists. On the emulator any
new name works, because the emulator creates the database on the first
connection. A `docker://` dev URL is refused for Spanner.

`ptah schema apply --dev-url` rehearses a plan on the dev database,
`ptah migrations generate --replay` and `ptah migrations lint --dev-url`
replay a migration directory there, and `--shadow-db` verifies a generated
migration. In each case the provider lists what the dev database holds and
empties it before and after the run, and judges every replayed statement
before Ptah runs it. [Dev databases](migrations.md#dev-databases) says what the
cleanup drops and what stops it.

The emulator holds at most 100 databases per instance and answers
`RESOURCE_EXHAUSTED: Too many databases` past that, so a long series of runs
that each name a new database needs a fresh container.

## ptah-compat

The community Atlas binary has no Spanner driver: it answers `unknown driver`
for a `spanner://` URL. Spanner on `ptah-compat` is therefore a Ptah extension.

`ptah-compat` finds the installed provider, or reads a manifest from
`PTAH_PROVIDER_CONFIG`, and starts the provider when a connection, a dev
database or a planning runtime first needs it.
[Use a provider from ptah-compat](https://docs.ptah.run/edge/extend/executable-providers/#use-a-provider-from-ptah-compat)
describes the session. A plan of a Spanner database is saved in the native JSON
plan format, because the `.plan.hcl` format cannot record which provider planned
it.

`PTAH_ATLAS_STRICT_COMPAT=1` reproduces the community binary: it refuses
`PTAH_PROVIDER_CONFIG` by name and starts no provider. See
[Strict CE mode](https://docs.ptah.run/edge/atlas/strict-ce-mode/).

## On a Ptah with a built-in Spanner engine

A Ptah release that builds in a Spanner engine of its own keeps that engine for
`spanner://` addresses, and the provider serves a command only where the
command asks for it:

- `ptah` uses the provider when the command passes `--provider-engine spanner`.
  The installed copy serves it, or the entry of the manifest that
  `--provider-config` names.
- Pin `search_path=public` in the address, as in
  `spanner://localhost:5432/app?sslmode=disable&search_path=public`. Such a
  release reads an address that pins no `search_path` as a read of every
  schema, through its built-in engine's catalog, which the provider does not
  serve, and the command stops with
  `read database schema: no realm-scope schema probe for dialect "spanner"`.
  `--schemas` does not change that.
- `ptah-compat` serves `spanner://` with the built-in engine, and refuses a
  manifest entry for the provider with
  `invalid provider registration: target name "cloudspanner" is claimed by "ptah.run/spanner" and "github.com/stokaro/ptah-spanner-plugin"`.

## Next steps

- Declare tables, indexes, row deletion policies, views and sequences:
  [Declaring Spanner objects](declarations.md).
- Run versioned migrations and lint them: [Migrations on Spanner](migrations.md).
- Check what the provider refuses before you plan a change:
  [What the Spanner provider refuses](limitations.md).
