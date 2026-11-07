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

The [documentation](docs/README.md) in `docs/` describes how to use the
provider, what a schema can declare for Spanner, how migrations run and what
the provider refuses. Ptah publishes those pages on docs.ptah.run under
`extend/providers/spanner/`.

## What the provider does

- **Sessions.** An address is `spanner://host:port/database`, with optional
  query parameters the PostgreSQL driver reads, such as `sslmode`. The schemes
  `cloudspanner://` and `google-spanner://` are the same. A reason the provider
  writes never quotes the address.
- **Reads.** A read describes the base tables of the requested schemas, or of
  `public` when the request names none: every column with the type Spanner
  stores, its nullability and its stored default, the primary key in key order,
  the secondary indexes with each key's direction and NULLS ordering and
  their `STORING` columns, the row deletion policy with the interval in the
  spelling Spanner stored, and the foreign keys and `CHECK` constraints. A
  foreign key carries its columns in key order, the table and columns it
  references and its rules; a `CHECK` carries its clause as Spanner keeps it,
  such as `(score >= '0'::bigint)` for `score >= 0`. The indexes Spanner builds
  for a foreign key, on either table, are Spanner's, not the user's, and are
  left out, and so are the CHECKs it keeps for `NOT NULL` columns. A key's
  NULLS ordering is stated only where it is not the default for its direction
  (`NULLS LAST` ascending, `NULLS FIRST` descending). Foreign keys and CHECKs
  reach a Ptah that reads constraints (provider contract 25.28); an older Ptah
  gets the read refused for a table that holds one.
- **Views and sequences.** A read reports each view with its query as Spanner
  stores it, and each sequence with its counter start. A view is created with
  `CREATE VIEW`, replaced with `CREATE OR REPLACE VIEW` and dropped with `DROP
  VIEW`; a declared query converges whatever its case, spacing and quoting,
  but a condition Spanner rewrites (`WHERE id > 0` reads back as `WHERE (id >
  '0'::bigint)`) is replaced on every plan until it is declared in the stored
  spelling. A sequence takes `START COUNTER WITH` alone: a changed start is
  `ALTER SEQUENCE ... RESTART COUNTER WITH`, and any other option is refused.
  A default `nextval('s')` converges with the `nextval('s'::text)` Spanner
  stores. A sequence that skips a range is refused by the read.
- **Refusals instead of omissions.** A read fails, with a reason that names the
  object, when the schema holds something the provider does not describe yet:
  a generated or identity column, a filtered, search or interleaved index, or
  an interleaved table. Ptah plans
  to create what a read leaves out, so leaving such an object out would plan a
  change the database refuses.
- **Tables without a primary key.** Spanner keys a table created without a
  primary key on a hidden column `rowid` of its own. A read leaves that column
  and its key out, so the table reads as declared and converges.
- **Plans.** The provider owns the lifecycle of the engine's tables: Ptah hands
  it every table a plan creates, changes or drops, and it plans each statement
  itself, one action per statement as Spanner runs them.
  - A table in a schema other than `public` follows `CREATE SCHEMA IF NOT
    EXISTS`, written once per schema a plan creates tables in, as a YAML
    source names the schema. A SQL or HCL source that declares a schema other
    than `public` is refused by Ptah before the provider plans anything: the
    host does not hand a provider a schema to create yet.
  - A created table is one `CREATE TABLE` with its key in key order, its
    defaults and its column `CHECK` constraints (an enum column is `TEXT` under
    a `CHECK` that lists its values), followed by its indexes and then by each
    foreign key and table `CHECK`, so two new tables may reference each other.
    A foreign key on `jsonb`, `numeric`, `real` or an array column is refused,
    as are `ON UPDATE`, deferral and every `ON DELETE` but `CASCADE`.
  - An index keeps each key's direction and NULLS ordering and its `INCLUDE`
    (`STORING`) columns. A changed index, a changed NULLS ordering included,
    is dropped and created again under its name.
  - A kept table's foreign keys and `CHECK` constraints are compared with the
    ones it holds: one declared and not held is added with `ALTER TABLE ...
    ADD CONSTRAINT`, one held and not declared is dropped with `ALTER TABLE ...
    DROP CONSTRAINT`, and a changed one is dropped and added again. A drop
    comes before any column of the table changes and before the removal of a
    table the key references, since Spanner refuses either while the
    constraint stands; an addition comes after the column changes and the
    creation of a table it references. A column added with a `CHECK` or a
    foreign key is added first and its constraints follow it. A `CHECK` is
    compared with the clause Spanner keeps by reading both into the form the
    server stores (literals cast to the column's type, `IN` as `= ANY` of an
    array, `BETWEEN` as two comparisons), so `score >= 0` converges with
    `(score >= '0'::bigint)`; a text the provider cannot read is replaced.
  - A column of a kept table changes with `ALTER COLUMN`: its type, between
    `character varying` of any length and `bytea`, which is all Spanner changes
    in place; its default; and `NOT NULL`. A `NOT NULL` column without a
    default is added nullable and then made `NOT NULL`, because Spanner refuses
    to add it otherwise.
  - A dropped table, and a dropped column, lose their indexes first. A dropped
    table loses its foreign keys first too, after the tables the plan creates,
    so `ptah migrations generate` writes a down file that drops every key
    before the tables.
  - A row deletion policy, `TTL INTERVAL '30 days' ON created_at`, follows
    `CREATE TABLE`'s column list, and a kept table's policy changes in place
    with `ADD TTL`, `ALTER TTL` or `DROP TTL`, after the table's column
    additions and before its column removals. The interval is compared by the
    days it denotes, because Spanner stores its own spelling (`30 days` reads
    back as `4 WEEKS 2 DAYS`). A policy on a column that is not `timestamptz`,
    an interval that is not a whole number of days, and a drop of the column a
    kept policy counts from are refused before a statement is sent.
  - Spanner cannot change a primary key or the type of a key column, so a plan
    that does either is refused with that reason, as is any other type change
    and a column type Spanner does not have (`smallint`, `timestamp`, `json`,
    `numeric(p,s)` and others), before a statement is sent.
- **Execution.** Spanner refuses a schema statement inside an explicit
  transaction, so each statement commits as it runs. A statement Spanner refuses
  applied nothing and the statements before it stay applied; the result says how
  many applied and names the server's SQLSTATE. A call that ends while a
  statement runs reports the outcome as unknown, because Spanner keeps applying
  a schema change its client stopped waiting for.
- **Locks.** `ptah schema apply` and the migration commands take a lock before
  they read the target. Spanner has no session lock, so a lock is a row of the
  table `ptah_locks`, which the provider creates on first use. The row names its
  holder and the end of a 30-second lease. A check reads the row and renews the
  lease once less than 20 seconds of it are left, so a check every second
  writes about once in ten seconds: Spanner's emulator runs one read-write
  transaction at a time, and a write beside another session's open transaction
  waits for Spanner to abort one of them. A holder that dies keeps the lock
  until its lease ends. A read reserves the table, so Ptah never plans its
  removal or writes it into an export.
- **Migrations.** `ptah migrations up`, `down`, `status`, `set`,
  `repair`, `tag` and `log` keep their records in the tables Ptah's built-in
  Spanner engine kept them in, in its layouts: the revision table
  `schema_migrations` (or `atlas_schema_revisions` with `--revision-format
  atlas`), the log `schema_migrations_log` and the tags `ptah_migration_tags`.
  A database that engine migrated keeps its history, and a table of one of
  those names in another layout is refused rather than adopted. The tag table
  has a `TIMESTAMPTZ` column where the built-in engine wrote `TIMESTAMP`, which
  Spanner refuses, so `migrations tag` works here where it did not before.
  - A migration body runs statement by statement. A schema statement runs
    outside a transaction; a statement that changes or reads rows runs in a
    serializable transaction together with the revision row's checkpoint. A
    body that fails part way keeps what ran, and the revision row records that
    prefix with the same partial checksum the built-in engine recorded.
    `--tx-mode all` is refused. A statement that controls a transaction or a
    batch, or changes a session setting, is refused with the whole body before
    anything runs.
  - The revision rows are compared and written in Spanner's read-write
    transactions, which change data only. A read reserves the three tables, so a
    schema apply never plans their removal.
- **Declared rows.** Rows a schema declares (`//ptah:schema:data`) are typed,
  read and written by the provider. A value is compared in one form for its
  column type, so a row Spanner stores in another spelling plans nothing:
  numeric `1e3` is 1000 and `12.50` is 12.5, JSON members are sorted and
  numbers compared by value, text in bytea hex is the bytes it spells, and a
  moment is rounded to the microsecond Spanner reads back. A moment written
  without an offset is UTC, and every moment the provider writes carries its
  offset, because Spanner reads one without in America/Los_Angeles. A read
  that names a transaction reads in it and sees what it wrote.
- **Seeds.** `ptah seed` runs each file and its entry in the ledger
  `schema_seeds`, the table and layout Ptah's built-in Spanner engine kept, in
  one read-write transaction. A file that hits an existing key keeps nothing,
  and `--idempotent` records it instead. A file holding a schema statement, a
  transaction statement or a setting is refused before anything of it runs,
  naming the line. A session with an open transaction runs no seed, because a
  seed's transaction cannot join it.
- **Operator statements.** A statement an operator writes, such as a
  migration's `-- +ptah check` assertion, runs through the provider and
  returns its columns and rows in the forms the declared rows use; a type no
  common value holds, such as an array, arrives as its text. A read-only
  statement runs only when it is one query that changes no row, and then in a
  read-only transaction. Any other statement runs as written, in the
  transaction it names or on its own. A statement Spanner refuses returns the
  server's message and SQLSTATE.
- **Migration lint.** `ptah migrations lint --dialect spanner` and `ptah sql
  lint --dialect spanner` read a file through the provider's split, which takes
  every statement a migration holds, and Ptah's rules for every engine run over
  it. The provider adds its own rules:
  - `SP101` (warning): `SET NOT NULL` validates every existing row, and a row
    holding NULL fails the schema change. Ptah's built-in Spanner engine
    reported this as `PG303`; select it as `SP101`, since `--disable PG303` and
    Atlas's `MF104` do not reach a provider's rule.
  - `SP102` (info): `ALTER TABLE ... DROP TTL` drops the row deletion policy
    and no column. It replaces the `DS102` and `BC104` findings Ptah reports
    for the statement otherwise, which read it as a drop of a column named
    `TTL`.

  `ptah sql lint` learns from the provider what each statement is: a table
  created without a primary key (`DDL001`), a statement it does not lint, such
  as an `INSERT` (`SQL002`), and `CREATE INDEX CONCURRENTLY`, which Spanner
  refuses (`CAP001`). A text the provider cannot read, such as one with a
  dollar-quoted string, fails the run with the reason.
- **Dev databases.** Ptah rehearses a plan on a dev database (`schema apply
  --dev-url`) and replays a migration directory on one (`migrations generate
  --replay`), and the provider lists and empties what the dev database holds:
  change streams, views, foreign keys, indexes, tables (an interleaved table
  before its parent), sequences, locality groups and, when the whole database
  is cleaned, every schema but `public`. The lock table stays. The database's
  DDL, as PGAdapter's `SHOW DATABASE DDL` prints it, is the inventory, so an
  object of a kind the provider has no statement for, such as a role, stops the
  cleanup before it drops anything, and so does a table that a foreign key from
  outside the cleaned schemas references. The provider also judges each
  replayed statement and refuses a change to a database option, a role or a
  privilege, and a statement that sets session or transaction state. It
  provisions no dev database, because a Spanner database is created through the
  instance's administration API: `--dev-url` names a database that exists (the
  emulator creates one on the first connection). A dev database needs a Ptah
  release with [Ptah #5565](https://github.com/stokaro/ptah/pull/5565).

- **Sources.** A table's row deletion policy is declared as Ptah's built-in
  Spanner engine read it: the `platform.spanner.row_deletion_column` and
  `platform.spanner.row_deletion_interval` attributes of a Go
  `ptah:schema:table` directive, the `spanner` group of a YAML table's
  `platform` key, or the `override` blocks of an HCL table's
  `platform "spanner"` block. A Go or YAML table without a policy declares
  none; an HCL table without the block leaves the policy it holds alone. An
  export writes the policy back in the same spelling, and `ptah schema stats`
  counts the tables that hold one. A SQL schema document's `TTL` clause is
  refused: Ptah does not hand a provider a table clause of a SQL document yet.

Creating a table with a foreign key or a table `CHECK`, and writing a read as
SQL with `ptah db read`, need a Ptah release with
[Ptah #5559](https://github.com/stokaro/ptah/pull/5559). Reading, comparing and
changing foreign keys and CHECKs need a Ptah of provider contract 25.28
([Ptah #5561](https://github.com/stokaro/ptah/pull/5561)). A down file of a
schema whose foreign key is a table constraint needs a Ptah that predicts the
key for the rollback plan; Ptah refuses to plan that down file until then. The
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
