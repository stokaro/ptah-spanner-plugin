---
title: Migrations on Spanner
description: How the Spanner provider runs a migration statement by statement, records it, holds the migration lock, cleans dev databases, lints migrations and runs seeds.
type: concept
audience:
  - database-engineer
  - ci-operator
readerQuestion: "How does a migration run on Spanner, and what do a failure, the lock and a dev database look like there?"
goal: "Predict how a Spanner migration runs and what it leaves after a failure, and resume or lint one."
sourceOfTruth:
  - internal/migrationunits
  - internal/history
  - internal/backend/history.go
  - internal/backend/execute.go
  - internal/locktable
  - internal/devclean
  - internal/migrationlint
  - internal/seedledger
  - internal/backend/statements.go
  - stokaro/ptah#4204
generated: false
searchAliases:
  - "Spanner migration lock"
  - "SP101"
  - "SP102"
overlaps: []
disposition: keep
sidebar:
  order: 3
---

# Migrations on Spanner

`ptah migrations up`, `down`, `status`, `set`, `repair`, `tag` and `log` run
against Spanner through this provider. Ptah's
[apply reference](https://docs.ptah.run/edge/versioned/apply/) describes the
run for every engine; this page says what is particular to Spanner: Spanner
commits every schema statement as it runs, so a migration is not one
transaction, and the provider keeps the history and the lock in tables of its
own.

## Statements and transactions

Spanner refuses a schema statement inside an explicit transaction (`DDL
statements are only allowed outside explicit transactions`). A migration file
therefore cannot run as one transaction. The provider splits each file into its
statements, and Ptah runs, counts and records them one at a time, in file
order:

- A schema statement runs outside a transaction and commits as it runs.
- A statement that reads or changes rows (`INSERT`, `UPDATE`, `DELETE`,
  `SELECT`, `WITH`, `VALUES`, `SHOW`, or a query in parentheses) runs in a
  serializable transaction together with the revision row's checkpoint, so the
  statement and the record of it commit together or not at all.

`--tx-mode all` is refused with
`tx-mode all is not supported for dialect "spanner": this target commits schema changes as they run, so a failed migration cannot be rolled back as a unit`.
The default mode, `file`, opens no transaction around a body on Spanner.

The provider refuses some files whole, before anything of them runs:

| In a Spanner migration file | Why it is refused |
| --- | --- |
| `BEGIN`, `START`, `COMMIT`, `END`, `ROLLBACK`, `ABORT`, `SAVEPOINT`, `RELEASE`, `PREPARE` or `RUN` | The statement controls a transaction or a PGAdapter batch, and Ptah runs each statement of a Spanner migration on its own |
| `SET` or `RESET` | A session setting would not reach the statements after it, which may run on another connection |
| A dollar-quoted string, an unclosed quote or comment, or a character outside Spanner's SQL | The provider cannot read the file into statements |

[Migration bodies the provider splits](https://docs.ptah.run/edge/extend/executable-providers/#migration-bodies-the-provider-splits)
shows how such a refusal reads.

`--statement-timeout` and `--lock-timeout`, and the matching `ptah.yaml` keys,
are refused before anything runs: Spanner has no statement or lock timeout that
Ptah can set and restore around a migration. `--migration-lock-timeout` bounds
the wait for [the migration lock](#the-migration-lock).

## A migration that fails

A statement Spanner refuses applied nothing, and the statements before it stay
applied. The revision row records that prefix, with the same partial checksum
Ptah's built-in Spanner engine recorded, so a database that engine left part
way is resumed over the same digests. This file creates a table, writes a row
into it, and then declares a type Spanner does not have:

```sql
CREATE TABLE a (id bigint PRIMARY KEY);
INSERT INTO a (id) VALUES (1);
CREATE TABLE b (id nosuchtype PRIMARY KEY);
```

`ptah migrations status` then reports the first two statements as applied, and
table `a` holds its row. An excerpt of its output:

```text
Dirty Migration: version=1 state=failed direction=up applied=2/3
Error: failed to execute migration SQL: provider execution failed (0 of 1 units confirmed): provider execution: NOT_APPLIED (0 units confirmed applied): Spanner refused unit 1 of 1 with SQLSTATE P0001
SQL: CREATE TABLE b (id nosuchtype PRIMARY KEY)
Error Statement: CREATE TABLE b (id nosuchtype PRIMARY KEY)

This migration stopped after 2 of 3 statements. Run 'ptah migrations repair --version 1 --resume-from 3' to run the rest, or repair with --force once you have run them yourself.
```

The error names the statement and the server's SQLSTATE, not Spanner's message:
the provider never quotes driver text, because it can carry a value or a
credential, and PGAdapter answers most Spanner refusals with `P0001`. Run the
statement by hand to read Spanner's own words. Fix the file, then resume with
`ptah migrations up --allow-dirty`, which skips the statements the row records
when they are unchanged, or follow the `repair` line the status prints.

A call that ends while Spanner is still applying a schema statement reports the
statement's outcome as unknown, because Spanner keeps applying a schema change
its client stopped waiting for. Inspect the database before you repair the
revision row.

## History tables

The provider keeps the migration records in the tables Ptah's built-in Spanner
engine kept them in, in that engine's layouts, so a database that engine
migrated keeps its history:

| Table | Holds |
| --- | --- |
| `schema_migrations` | One revision row per migration, in Ptah's format |
| `atlas_schema_revisions` | The revision rows with `--revision-format atlas` |
| `schema_migrations_log` | The migration log that `ptah migrations log` prints |
| `ptah_migration_tags` | The tags of `ptah migrations tag`, with a `TIMESTAMPTZ` column |

The tables sit in the schema `--migrations-schema` names, or in the session's
default schema, `public`. The provider creates a missing table with the statement the built-in engine used,
and refuses a table of one of these names in another layout rather than adopt
it. A revision row of a rollback records its direction in its state, as
`pending:down` or `failed:down`, because the native table has no direction
column. The rows are read and written in Spanner's read-write transactions,
which change data only.

A read reserves the history tables, so `ptah schema apply` never plans their
removal and an export never writes them.

## The migration lock

Spanner has no session lock, so the provider keeps Ptah's migration lock as a
row of the table `ptah_locks`, which it creates on first use.
`ptah schema apply` and the migration commands take the lock before they read
the target. The row names its holder and the end of a 30-second lease:

- A holder checks the row once a second and renews the lease once less than 20
  seconds of it are left, so it writes about once in ten seconds. The emulator
  runs one read-write transaction at a time, and a write beside another
  session's open transaction waits until Spanner aborts one of them, so a
  write every second would stall the migration beside it.
- A holder that dies keeps the lock until its lease ends.
- A session that loses a lock it held runs nothing more.

A read reserves `ptah_locks`, so a plan never drops it and an export never
writes it. Like every advisory lock, it keeps Ptah runs apart and does not stop
a client that ignores it.

## Dev databases

The provider empties a dev database through the cleanup Ptah runs before and
after a rehearsal or a replay. It lists what the database holds and drops it in
this order:

1. change streams;
2. views, in rounds, so a view goes before the views it reads;
3. foreign keys;
4. indexes;
5. tables, an interleaved table before its parent;
6. sequences and locality groups;
7. every schema but `public`, when the whole database is cleaned.

The lock table stays. The inventory is the database's DDL as PGAdapter's `SHOW
DATABASE DDL` prints it, checked against `information_schema`. An object of a
kind the provider has no statement for, such as a role, a grant or a property
graph, stops the cleanup before it drops anything, and so does a table that a
foreign key from outside the cleaned schemas references. The provider then
removes the whole list or nothing. `ptah db drop-all` and `ptah-compat schema
clean` empty a database through the same cleanup.

During a replay the provider judges each statement before Ptah runs it. It
accepts what a cleanup can remove: creating, changing and dropping a table, an
index, a view, a sequence, a change stream, a locality group or a schema, and
reading and writing rows. It refuses a change to a database option, a role or a
privilege, which outlive a cleanup, and a statement that sets session or
transaction state, which would reach the statements after it.

## Lint

`ptah migrations lint --dialect spanner` and `ptah sql lint --dialect spanner`
read a file through the provider's split, which takes every statement a
migration holds, and Ptah's rules for every engine run over it. The provider
adds its own rules:

| Rule | Severity | Finding |
| --- | --- | --- |
| `SP101` | warning | `SET NOT NULL` validates every existing row, and a row holding NULL fails the schema change and leaves the column nullable |
| `SP102` | info | `ALTER TABLE ... DROP TTL` drops the row deletion policy and no column; it replaces the `DS102` and `BC104` findings, which read the statement as a drop of a column named `TTL` |

For a migration that holds `ALTER TABLE events ALTER COLUMN note SET NOT NULL;`
and `ALTER TABLE events DROP TTL;`, the two findings read:

```text
lint/2_tighten.up.sql:1 [warning] SP101: SET NOT NULL runs as a schema change that validates every existing row, and a row holding NULL fails it (FAILED_PRECONDITION: Cannot specify a null value for column) and leaves the column nullable; backfill first (not-null validation scans existing rows)
lint/2_tighten.up.sql:2 [info] SP102: DROP TTL removes the table's row deletion policy and drops no column; rows the policy would have deleted are kept from now on (row deletion policy dropped)
```

Select the rule as `SP101`. `--disable PG303`, the code PostgreSQL's
`SET NOT NULL` check carries, names no rule of a Spanner run, and Atlas's
`MF104` in `--disable` or an `atlas:nolint` directive does not reach `SP101`.

`ptah sql lint` learns from the provider what each statement is: a table created
without a primary key (`DDL001`), a statement it does not lint, such as an
`INSERT` (`SQL002`), and `CREATE INDEX CONCURRENTLY`, which Spanner refuses
(`CAP001`). A text the provider cannot read, such as one with a dollar-quoted
string, fails the run with the reason.

## Seeds and check assertions

`ptah seed` runs each seed file and its entry in the ledger
`public.schema_seeds` in one read-write transaction. The ledger is the table and layout Ptah's built-in
Spanner engine kept, with `checksum` as `TEXT` and `applied_at` as
`TIMESTAMPTZ`. A file that hits an existing key keeps nothing, and
`--idempotent` records the file instead. A file that holds a schema statement,
a transaction statement or a setting is refused before anything of it runs,
naming the line. A session with an open transaction runs no seed, because the
seed's transaction cannot join it.

A migration's `-- +ptah check` assertion runs through the provider. It runs
only when it is one query that changes no row, and then in a read-only
transaction. Its columns and rows come back in the forms declared rows use,
and a type with no such form, such as an array, comes back as its text.
