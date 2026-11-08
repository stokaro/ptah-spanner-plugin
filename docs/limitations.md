---
title: What the Spanner provider refuses
description: The objects a Spanner read refuses, the declarations and changes the provider refuses before a statement is sent, the gaps that have no refusal yet, and where the emulator differs from the managed service.
type: reference
audience:
  - database-engineer
  - schema-author
readerQuestion: "Which Spanner objects and changes does the provider refuse or not describe yet, and why?"
goal: "Tell before planning whether a Spanner schema or change is one the provider refuses, and what to do instead."
sourceOfTruth:
  - internal/catalog
  - internal/tableplan
  - internal/coltype
  - internal/commonobject
  - internal/rowdeletion
  - internal/changestream
  - internal/migrationunits
  - internal/backend/identity.go
  - stokaro/ptah#4204
generated: false
searchAliases:
  - "Spanner limitations"
  - "Spanner best effort"
overlaps: []
disposition: keep
sidebar:
  order: 4
---

# What the Spanner provider refuses

Before you plan a Spanner change, check here whether the provider refuses the
object or the change, whether it describes the object yet, and whether the
emulator its tests run behaves like the managed service. A refusal names the
object and the reason, and comes before any statement is sent unless a section
here says otherwise. Ptah plans to create what a read leaves out, so an object
the provider cannot describe is refused rather than skipped.

## Support level

Support is best effort. The live tests run the Cloud Spanner emulator behind
PGAdapter, `gcr.io/cloud-spanner-pg-adapter/pgadapter-emulator:v0.56.2`, which
is evidence about the PostgreSQL interface only. No test runs against a managed
Spanner instance, and the provider serves no GoogleSQL database. Review the
statements a plan writes before you apply them to a managed database.

## Objects a read refuses

A read fails with a reason, rather than leave out an object it does not
describe:

| Object in the database | Reason the read gives |
| --- | --- |
| An interleaved table | `table "public"."t" is interleaved in a parent table, which the provider does not describe yet` |
| A generated column | `column "c" of table "public"."t" is generated, which the provider does not describe yet` |
| An identity column other than the hidden key of a table without a primary key | `column "c" of table "public"."t" is an identity column, which the provider does not describe yet` |
| A search index, a null-filtered index, or an index interleaved in a parent table | `index "i" of table "public"."t" is a SEARCH index, which the provider does not describe yet`, and the matching reason for the other two |
| An index with a `WHERE` clause, on a Ptah that does not compare index predicates (provider contract 25.28 or older) | `index "i" of table "public"."t" has a WHERE filter, which needs a host that compares index predicates (schema.index-predicates)` |
| An index whose filter the provider cannot read back as `NOT NULL` tests on the index's columns | `index "i" of table "public"."t" has a WHERE filter the provider cannot read: ...` |
| A sequence that skips a range of values | `sequence "public"."s" skips a range of values, which the provider does not describe yet` |
| An index Spanner is still building, or a foreign key or `CHECK` it is still validating | `... is still being built; read again once Spanner reports it ready`, or `still being validated` for a constraint |

The `NOT NULL` tests Spanner keeps on every key column of a unique index are
not a user filter: a read leaves them out, and a unique index without a filter
of its own reads as one without a `WHERE` clause. The index Spanner builds for a foreign key, and the `CHECK` it keeps
for a `NOT NULL` column, are Spanner's own and are left out of a read without a
refusal.

A Ptah that does not take a table's foreign keys and `CHECK` constraints
(provider contract 25.27) gets the read of a table that holds one refused, and
a Ptah that does not carry views and sequences gets the read of a schema that
holds one refused, each naming the object and the contract feature it needs.

## Declarations the provider refuses

| Declaration | Where it is described |
| --- | --- |
| `smallint`, `serial`, `timestamp`, `json`, `char`, `xml`, `interval`, a type modifier and the other types Spanner does not have | [Column types](declarations.md#column-types) |
| A descending or prefixed key part, and a key with `INCLUDE`, a method or deferral | [Primary keys](declarations.md#primary-keys) |
| A collation, an engine, a comment, custom table SQL, a trigger, an identity or generated column, and uniqueness over an expression | [What a table cannot carry](declarations.md#what-a-table-cannot-carry) |
| An index with a `WHERE` clause Spanner cannot create, an access method, an operator class, storage parameters, `NULLS NOT DISTINCT`, an expression key or a prefix | [Indexes](declarations.md#indexes) |
| `ON UPDATE`, an `ON DELETE` action other than `NO ACTION` or `CASCADE`, deferral, and a foreign key on `jsonb`, `numeric`, `real` or an array | [Foreign keys and CHECK constraints](declarations.md#foreign-keys-and-check-constraints) |
| A row deletion policy with a unit, without whole days, or on a column that is not `timestamptz` | [Row deletion policies](declarations.md#row-deletion-policies) |
| A materialized view, a view option, `with_check` and a view comment | [Views](declarations.md#views) |
| A sequence option other than its counter start | [Sequences](declarations.md#sequences) |
| A comment on an index | [Indexes](declarations.md#indexes) |

Spanner's PostgreSQL interface has no role, grant, row-level security policy,
stored function or comment, and the provider carries none of them. Ptah refuses
a declaration that writes one, naming it, such as
`the experimental provider contract does not support roles on the spanner engine, whose provider does not carry them`.

## Changes the provider refuses

These changes of a kept table are refused before a statement is sent, because
Spanner refuses them:

- a change of a primary key's columns or of their order;
- a type change of a key column;
- a type change other than between `character varying` of any length and
  `bytea`;
- the drop of the column a kept row deletion policy counts from.

Create a new table with the shape you need and copy the rows. A sequence change
other than its counter start is refused too.

The drop of a table that a change stream names in its `FOR` clause, and of a
column that one names in its column list, is refused too, but when the
statements run rather than when they are planned. Before the first statement of
a run, the provider reads what the change streams name from
`information_schema.change_stream_tables` and `change_stream_columns`. When a
statement of the run drops one of them, nothing of the run is sent, and the
reason names the table or column and the stream:

```text
unit 2 of 2 drops table "t", which change stream "cs1" names, so no unit ran: Spanner refuses to drop a table a change stream names; drop the change stream, or alter it to stop tracking the table, before dropping the table
```

Spanner refuses such a drop itself (`Table t is tracked by 1 Change Stream:
cs1`), but only when it runs, after the statements before it. Drop the stream,
or alter it with `ALTER CHANGE STREAM`, first. A run that drops or alters a
change stream before its drops leaves those drops to Spanner, because what the
streams name has changed by then. A stream created `FOR ALL` names nothing, and
a stream `FOR t` names no column of `t`, so neither stops a drop. `ptah
migrations up` runs each statement of a migration on its own, so the statements
of a migration before the drop have run when the drop is refused.

## Clauses Ptah does not hand the provider

On a Ptah of provider contract 25.28 or older, some declarations stop in Ptah
before they reach the provider, because that contract has no place for them. A
Ptah of contract 25.29 hands each of them to the provider:

- A SQL schema file's row deletion clause, `TTL INTERVAL '30 days' ON
  created_at`, is refused with
  `the experimental provider contract does not support Table.Overrides`.
  Declare the policy in Go, YAML or HCL there. An export that `ptah db read`
  writes carries the clause, so the export does not replay through the
  provider on such a Ptah.
- A schema declared as an object of its own, through SQL `CREATE SCHEMA`, a Go
  `//ptah:schema:schema` directive or an HCL `schema` block other than
  `public`, is refused with
  `the experimental provider contract does not support SchemaDiff.SchemasAdded`,
  also when the schema exists. A table in such a schema, declared in Go, YAML or
  SQL without the schema object, is created after `CREATE SCHEMA IF NOT EXISTS`.
  An HCL table always names a `schema` block, so an HCL table outside `public`
  is refused.
- An index with a `WHERE` clause is refused, in a declaration and in a read.

## Gaps without a refusal

These cases plan something Spanner refuses, or plan more than the declaration
asks for, and nothing refuses them before the plan runs. The work that closes
them is part of stokaro/ptah#4204.

- **Change streams.** A read leaves change streams out, and a plan does not see
  them. `ptah schema apply --dry-run` and `ptah migrations generate` write the
  drop of a table or a column a change stream names; the refusal comes when the
  statements run (see [Changes the provider refuses](#changes-the-provider-refuses)).
  An export carries no line that marks change streams as not described.
- **The seed ledger on an older Ptah.** On a Ptah of provider contract 25.28
  or older, after `ptah seed` has run, `ptah schema apply` plans `DROP TABLE
  "schema_seeds"` when the declaration does not name the table. Review the
  plan, or declare the table. A Ptah of contract 25.29 leaves the ledger out of
  every plan.
- **A column added with a `CHECK`.** The down file that `ptah migrations
  generate` writes for it drops the column first, and Spanner refuses that
  (`Cannot drop column score on which there is a dependent check constraint`).
  Edit the down file to drop the constraint before the column.
- **`WITH CHECK OPTION` in a SQL schema file.** The clause stays in the view's
  query text, the plan writes it, and Spanner refuses the statement when it
  runs, after the statements before it. Leave the clause out: Spanner's views
  cannot be written through.
- **A condition Spanner rewrites.** A view or a `CHECK` whose text the provider
  cannot read in Spanner's spelling is replaced on every plan, and so is any
  rewritten view under a host older than contract 25.37; see
  [Views](declarations.md#views).

## Where the emulator differs

The emulator behind PGAdapter 0.56.2 differs from the documented service in
ways that change what a plan does:

- For a foreign key, Spanner documents
  `key_column_usage.position_in_unique_constraint` as the column's position in
  the referenced key, and the provider reads it that way. The emulator reports
  each key column's own position instead. A composite foreign key that lists the
  referenced columns in another order than the referenced key, such as
  `FOREIGN KEY (y, x) REFERENCES parent (b, a)` against `PRIMARY KEY (a, b)`,
  therefore reads back paired in the key's order, and every plan drops and adds
  it. The server still enforces the key as declared. A key that lists the
  referenced columns in the key's order reads back as declared.
- The emulator runs one read-write transaction at a time, and a write beside
  another session's open transaction waits until it aborts one of them. Reads
  do not wait. [The migration lock](migrations.md#the-migration-lock) is built
  around this.
- The emulator refuses `CREATE ROLE`, so the dev database cleanup's refusal of
  roles and grants, and the replay guard's, are untested on the managed
  service.
- The emulator holds at most 100 databases per instance.
