---
title: Declaring Spanner objects
description: The column types, keys, indexes, constraints, row deletion policies, views, sequences and schemas the Spanner provider creates, and the statements it writes for them.
type: reference
audience:
  - schema-author
  - database-engineer
readerQuestion: "What can a Go, YAML, HCL or SQL schema declare for Spanner, and which statements does the provider write for it?"
goal: "Declare a Spanner schema that the provider creates and converges on, and predict the statements a plan writes."
sourceOfTruth:
  - internal/coltype
  - internal/tableplan
  - internal/ddl
  - internal/checkexpr
  - internal/policysource
  - internal/rowdeletion
  - internal/commonobject
  - internal/catalog
  - internal/indexfilter
  - internal/rowdata
  - stokaro/ptah#4204
generated: false
searchAliases:
  - "Spanner row deletion policy"
  - "Spanner TTL"
  - "Spanner column types"
  - "START COUNTER WITH"
overlaps: []
disposition: keep
sidebar:
  order: 2
---

# Declaring Spanner objects

A schema that targets Spanner declares tables, indexes, constraints, row
deletion policies, views, sequences and schemas, and the provider turns each
into the statements Spanner's PostgreSQL interface runs. Each section below
names what a declaration may hold, the statement a plan writes for it, and what
the provider refuses. The directives, keys and blocks themselves are Ptah's,
and every engine shares them: see the
[Go annotation reference](https://docs.ptah.run/edge/reference/go-annotations/),
the [YAML schema reference](https://docs.ptah.run/edge/reference/yaml-schema/),
the [HCL schema reference](https://docs.ptah.run/edge/reference/hcl-schema/)
and [SQL schema files](https://docs.ptah.run/edge/schema/sql/).

The provider checks a declaration before any statement is sent. A refusal names
the object and says why, such as:

```text
column "n" of table "t": type "smallint": Spanner stores integers as bigint only
```

## What a plan writes

This SQL schema declares two tables, a unique index, a foreign key, a `CHECK`,
an index with stored columns, a sequence and a view:

```sql
CREATE TABLE authors (
  id bigint PRIMARY KEY,
  name varchar(100) NOT NULL,
  email text
);
CREATE UNIQUE INDEX authors_email ON authors (email);
CREATE TABLE posts (
  id bigint PRIMARY KEY,
  author_id bigint NOT NULL REFERENCES authors (id) ON DELETE CASCADE,
  title text NOT NULL,
  score bigint DEFAULT 0 CHECK (score >= 0),
  created_at timestamptz NOT NULL
);
CREATE INDEX posts_author ON posts (author_id, created_at DESC) INCLUDE (title);
CREATE SEQUENCE ticket_ids START WITH 1000;
CREATE VIEW top_posts AS SELECT id, title FROM posts WHERE score > 10;
```

On an empty database the plan holds these statements, measured on the
emulator:

```sql
CREATE SEQUENCE "ticket_ids" START COUNTER WITH 1000;
CREATE TABLE "authors" (
  "id" bigint PRIMARY KEY NOT NULL,
  "name" varchar(100) NOT NULL,
  "email" text
);
CREATE TABLE "posts" (
  "id" bigint PRIMARY KEY NOT NULL,
  "author_id" bigint NOT NULL,
  "title" text NOT NULL,
  "score" bigint DEFAULT 0 CONSTRAINT "posts_score_check" CHECK (score >= 0),
  "created_at" timestamptz NOT NULL
);
CREATE UNIQUE INDEX "authors_email" ON "authors" ("email");
CREATE INDEX "posts_author" ON "posts" ("author_id", "created_at" DESC) INCLUDE ("title");
ALTER TABLE "posts" ADD CONSTRAINT "fk_posts_author_id" FOREIGN KEY ("author_id") REFERENCES "authors"("id") ON DELETE CASCADE;
CREATE VIEW "top_posts" AS
SELECT id, title FROM posts WHERE score > 10;
```

Each statement runs on its own, because Spanner runs no schema statement inside
a transaction. A second plan against the same schema is empty except for the
view: Spanner stores its condition as `WHERE (score > '10'::bigint)`, and
[Views](#views) says why that replaces the view on every plan.

## Tables

### Column types

A declared type is compared with the type Spanner stores, so a column declared
`int` or `text` converges with the `bigint` or `character varying` a read
reports.

| Declared | Stored as |
| --- | --- |
| `bigint`, `int8`, `int`, `integer` | `bigint` |
| `boolean`, `bool` | `boolean` |
| `bytea` | `bytea` |
| `date` | `date` |
| `double precision`, `float8`, `float` | `double precision` |
| `real`, `float4` | `real` |
| `numeric`, `decimal` | `numeric` |
| `jsonb` | `jsonb` |
| `text`, `varchar`, `character varying` | `character varying` |
| `varchar(n)` with `n` from 1 to 2621440 | `character varying(n)`; 2621440 stores no length |
| `timestamptz`, `timestamp with time zone` | `timestamp with time zone` |
| `uuid` | `uuid` |
| `spanner.commit_timestamp` | `spanner.commit_timestamp` |
| Any of these followed by `[]` | An array of that type |

A column declared `int` or `integer` keeps that spelling in the statement, and
Spanner stores a `bigint`: the column takes values outside the 32-bit range of
PostgreSQL's `integer`. A Ptah of provider contract 25.33 receives this as an
omission of the render, which Ptah's Go API returns with the statements
(`engine.Runtime.RenderSchema`) and which a caller that must not lose a
declaration refuses. A `CHECK (c BETWEEN -2147483648 AND 2147483647)` keeps the
range.

These types are refused before a statement is sent, with the reason in the
right column:

| Declared | Reason |
| --- | --- |
| `smallint`, `int2`, `int4` | Spanner stores integers as `bigint` only |
| `serial`, `bigserial` | A serial column is an identity column, which the provider does not describe yet |
| `timestamp`, `timestamp without time zone` | Spanner has no timestamp without time zone; use `timestamptz` |
| `json` | Spanner has no `json` type; use `jsonb` |
| `interval`, `oid` | Spanner refuses a column of that type |
| `char`, `character`, `bpchar` | Spanner refuses the character type; use `varchar` |
| `xml` | Spanner has no `xml` type; use `text` |
| A type with a modifier, such as `numeric(10,2)` or `timestamptz(3)` | Spanner refuses a type modifier |
| An array of arrays | Spanner refuses it |

### Primary keys

A key is created in key order: `PRIMARY KEY (b, a)` declares a key that starts
with `b`, and the plan writes it that way. Every key part is an ascending whole
column. A descending or prefixed key part, and a key with `INCLUDE`, an access
method or deferral, are refused.

Spanner cannot change a table's key. A plan that changes a key's columns or
their order is refused with
`Spanner cannot change a table's primary key; create a new table with the key and copy the rows`,
and so is a type change of a key column.

A table declared without a primary key is created as written. Spanner keys such
a table on a hidden column `rowid` of its own. A read leaves that column and its
key out, so the table reads as declared and the next plan is empty.

### Defaults and NOT NULL

A default is written as declared, with a number or a boolean bare. Spanner
stores its own spelling, such as `'5'::bigint` for `5`, and the comparison reads
the stored spelling back, so the default converges. A default
`nextval('ticket_ids')` converges with the `nextval('ticket_ids'::text)` Spanner
stores.

Spanner refuses `ADD COLUMN` for a `NOT NULL` column without a default, even on
an empty table. Such a column is added nullable and made `NOT NULL` by a second
statement, which the plan marks with a comment:

```sql
ALTER TABLE "t" ADD COLUMN "d" bigint;
-- SPANNER: SET NOT NULL fails if any row of "t" holds NULL in "d"; the column declares no default to fill it with.
ALTER TABLE "t" ALTER COLUMN "d" SET NOT NULL;
```

On a table that holds rows, the second statement fails, because no row has a
value for the new column. Give the column a default, which Spanner writes into
every row, or fill the column before you make it `NOT NULL`.

### Changing a column

A column of a kept table changes with `ALTER TABLE ... ALTER COLUMN`:

- its type, between `character varying` of any length and `bytea`, arrays alike,
  which is every type change Spanner makes in place;
- its default, with `SET DEFAULT` or `DROP DEFAULT`;
- its nullability, with `SET NOT NULL` or `DROP NOT NULL`.

Any other type change is refused before a statement is sent, such as
`changing column "b" of table "t" from bigint to double precision: Spanner changes a column's type only between character varying and bytea, or the length of character varying`.

A dropped column loses its indexes first, and a dropped table loses its indexes
and its foreign keys first.

### What a table cannot carry

Spanner has no place for these declarations, and the provider refuses each one
by name:

- a table collation or engine, a table comment, custom table SQL and triggers;
- a column collation, a column comment, a `CHECK` that is not enforced, and
  uniqueness over an expression;
- a constraint other than a primary key, a unique constraint, a foreign key or a
  `CHECK`.

Spanner has identity and generated columns, but the provider does not describe
them yet, and refuses a column declared as either with
`is an identity column, which the provider does not describe yet` or
`is a generated column, which the provider does not describe yet`.

## Indexes

An index keeps each key's direction and NULLS ordering and its stored columns,
which the PostgreSQL interface spells `INCLUDE`:

```sql
CREATE INDEX "t_i" ON "t" ("a" DESC, "b" NULLS FIRST) INCLUDE ("c");
```

A unique column is created as a unique index, `"t_e_key"` for column `e` of
table `t`, because Spanner refuses the `UNIQUE` constraint spelling.

Indexes are compared by uniqueness, by their keys (column, direction and NULLS
ordering) and by their stored columns. A changed index is dropped and created
again under its name. A NULLS ordering is stated only where it differs from the
default for its direction, which is `NULLS LAST` ascending and `NULLS FIRST`
descending, so an index that spells the default converges with one that leaves
it out.

These index declarations are refused:

- an access method, an operator class, storage parameters, `NULLS NOT DISTINCT`
  or an invisible index, none of which a Spanner index has;
- a key that is an expression or a column prefix;
- a comment (`has a comment; Spanner's PostgreSQL interface stores no comment`),
  on a created index and on a kept one: Spanner answers `COMMENT ON INDEX` with
  `Unknown statement`.

### Filtered indexes

Spanner creates an index with one kind of `WHERE` clause: `NOT NULL` tests,
joined by `AND`, on columns the index keys or stores. A declared clause is
written that way, each column once and in byte order of the names:

```sql
CREATE INDEX t_cd ON t (c, d) WHERE d IS NOT NULL AND c IS NOT NULL;
```

```sql
CREATE INDEX "t_cd" ON "t" ("c", "d") WHERE "c" IS NOT NULL AND "d" IS NOT NULL;
```

A read states the clause as Spanner keeps it, `c IS NOT NULL AND d IS NOT
NULL`, and the declaration is compared in the same spelling, so the index
converges whatever order and quoting the declaration uses. A changed clause
drops the index and creates it again. `ptah db read` writes the clause.

Spanner gives a unique index a `NOT NULL` test on each of its key columns by
itself, so a read leaves those tests out, and a unique index takes a clause on
the columns it stores alone. These clauses are refused, naming the index:

- a test other than `IS NOT NULL`, such as `WHERE c > 0`
  (`has a WHERE filter Spanner cannot create: Spanner takes only <column> IS NOT NULL tests joined by AND`);
- a column the index neither keys nor stores
  (`filters on column "d", which the index neither keys nor stores`);
- a unique index's own key (`is unique and filters on its key "c"`).

Filtered indexes need a Ptah of provider contract 25.29, which compares the
clause. An older Ptah gets every `WHERE` clause refused, in a declaration and in
a read.

The index Spanner builds for a foreign key, on either table, is Spanner's. A
read leaves it out, so a plan never drops it and an export never writes it.

## Foreign keys and CHECK constraints

A foreign key or a table `CHECK` of a created table is added with `ALTER TABLE
... ADD CONSTRAINT` after every table the plan creates, so two new tables may
reference each other. A column `CHECK` stays in `CREATE TABLE`.

A foreign key takes `ON DELETE NO ACTION` or `ON DELETE CASCADE`. These are
refused before a statement is sent:

- `ON UPDATE` of any kind, and every other `ON DELETE` action;
- deferral;
- a column of type `jsonb`, `numeric`, `real` or an array, which Spanner refuses
  in a key, such as
  `invalid foreign key: Spanner type jsonb cannot participate in foreign keys: "c"."j" references "p"."id"`;
- columns whose types differ between the two tables.

On a kept table, a declared foreign key or `CHECK` the table does not hold is
added, one the table holds and the declaration leaves out is dropped with `ALTER
TABLE ... DROP CONSTRAINT`, and a changed one is dropped and added again. A drop
runs before any column of the table changes and before the removal of a table
the key references, because Spanner refuses either while the constraint stands.
An addition runs after the column changes and after the creation of a table it
references. A column added with a `CHECK` is added first, and its constraint
follows it.

Spanner keeps a `CHECK` in its own spelling: `score >= 0` reads back as
`(score >= '0'::bigint)`, and `status IN ('a', 'b')` as `(status = ANY
(ARRAY['a'::text, 'b'::text]))`. The provider reads both texts into the form
Spanner stores, with literals cast to the column's type, `IN` as `= ANY` of an
array and `BETWEEN` as two comparisons, so such a `CHECK` converges. A text the
provider cannot read is replaced on every plan.

### Enum columns

Spanner has no enum type: the emulator answers `CREATE TYPE ... AS ENUM` with
`Statement is not supported`. A column declared with enum values, or typed
with the name of an enum the source declares, is created as `TEXT` under a
`CHECK` that lists them, named `<table>_<column>_check`, as Ptah's built-in
Spanner engine created it. A quote in a value is doubled:

```sql
"kind" TEXT CONSTRAINT "posts_kind_check" CHECK (kind IN ('draft', 'it''s'))
```

On a kept table the column is added without the `CHECK`, which Spanner refuses
inside `ADD COLUMN`, and `ALTER TABLE ... ADD CONSTRAINT` follows it. Spanner
refuses a value the `CHECK` does not list.

A changed list of values replaces the `CHECK`: Ptah asks the provider whether
the `CHECK` Spanner keeps, `(kind = ANY (ARRAY['draft'::text]))`, is the
declared one, and the plan drops it and adds the new one under the same name:

```sql
ALTER TABLE "posts" DROP CONSTRAINT "posts_kind_check";
ALTER TABLE "posts" ADD CONSTRAINT "posts_kind_check" CHECK (kind IN ('draft', 'live'));
```

When a row holds a value the new list leaves out, the run is refused before
its first statement, and the column keeps its `CHECK`; see
[Changes the provider refuses](limitations.md#changes-the-provider-refuses).
A Ptah that does not ask the provider about the `CHECK` it builds for an enum
column plans nothing for a changed list.

The provider states this form to Ptah (provider contract 25.31), and Ptah
builds the column before it compares and plans. An older Ptah without a
built-in Spanner engine keeps the enum a type of its own and refuses it with
`the experimental provider contract does not support SchemaDiff.EnumsAdded`.

## Row deletion policies

Spanner deletes a row once an interval has passed since the time a
`timestamptz` column holds. A Go, YAML or HCL table declares the policy with
two properties, `row_deletion_column` and `row_deletion_interval`, and a SQL
schema file with Spanner's own clause.

In Go, they are `platform.spanner` attributes of the table directive:

```go
//ptah:schema:table name="sessions" platform.spanner.row_deletion_column="created_at" platform.spanner.row_deletion_interval="30 days"
type Session struct {
	//ptah:schema:field name="id" type="BIGINT" primary="true"
	ID int64
	//ptah:schema:field name="created_at" type="TIMESTAMPTZ" not_null="true"
	CreatedAt time.Time
}
```

In YAML, they sit in the `spanner` group of the table's `platform` key:

```yaml
tables:
  sessions:
    platform:
      spanner:
        row_deletion_column: created_at
        row_deletion_interval: 30 days
    columns:
      id: {type: bigint, primary: true}
      created_at: {type: timestamptz, not_null: true}
```

In HCL, they are `override` blocks of the table's `platform "spanner"` block:

```hcl
table "sessions" {
  schema = schema.public
  platform "spanner" {
    override "row_deletion_column" {
      value = "created_at"
    }
    override "row_deletion_interval" {
      value = "30 days"
    }
  }
  column "id" {
    type = bigint
  }
  column "created_at" {
    type = timestamptz
  }
  primary_key {
    columns = [column.id]
  }
}
```

In a SQL schema file, the policy is the clause after the table's column list:

```sql
CREATE TABLE sessions (
  id bigint PRIMARY KEY,
  created_at timestamptz NOT NULL
) TTL INTERVAL '30 days' ON created_at;
```

The column after `ON` is one name, taken as written, as the file names its
columns. A clause the provider cannot read is refused while the file is read,
at the clause's line and column, such as
`table clause of sessions: provider decode source failed (InvalidArgument): the row deletion policy of table "sessions": the interval is not a whole number of days, which Spanner refuses`.

A created table carries the policy after its column list:

```sql
CREATE TABLE "sessions" (
  "id" BIGINT PRIMARY KEY NOT NULL,
  "created_at" TIMESTAMPTZ NOT NULL
) TTL INTERVAL '30 days' ON "created_at";
```

A kept table's policy changes in place. The plan writes `ALTER TABLE ... ADD
TTL` for a table without a policy, `ALTER TABLE ... ALTER TTL` for a changed
one and `ALTER TABLE ... DROP TTL` when the declaration names none, because
Spanner refuses `ADD` and `ALTER` in each other's place. The change runs after
the table's column additions and before its column removals, so a policy may
move to a new column in one plan.

The interval is a number of months, weeks, days or hours that adds up to whole
days. Spanner stores its own spelling, `30 days` as `4 WEEKS 2 DAYS` and `60
days` as `2 MONTHS`, counting a month as 30 days, so the comparison reads both
spellings as hours rather than as text. These declarations are refused before
anything runs:

- a policy without its column or its interval, or with an empty one;
- `row_deletion_unit`, because the policy counts from a timestamp and takes no
  unit;
- an interval that is not a whole number of days, such as `36 hours`;
- a column that is not `timestamptz`;
- a plan that drops the column a kept policy counts from.

A Go or YAML table without the properties, and a SQL table without the clause,
declares no policy, so a plan drops a policy the table holds. An HCL table without the `platform "spanner"` block
says nothing about the policy, and the table keeps the one it holds.
`ptah schema inspect` and `ptah db read` write a policy back with the interval
in the spelling Spanner stores, and `ptah schema stats` counts the tables that
hold one as `ptah_schema_spanner_row_deletion_policy_tables`.

The SQL clause needs a Ptah of provider contract 25.29, which hands it to the
provider; the SQL that `ptah db read` writes then replays and converges. An
older Ptah refuses the clause, as
[Clauses Ptah does not hand the provider](limitations.md#clauses-ptah-does-not-hand-the-provider)
describes; declare the policy in Go, YAML or HCL there.

## Views

A view is created with `CREATE VIEW`, replaced with `CREATE OR REPLACE VIEW` and
dropped with `DROP VIEW`:

```go
//ptah:schema:view name="top_posts" body="SELECT id FROM posts WHERE (score > '10'::bigint)"
type Objects struct{}
```

A read reports the query as Spanner stores it, which is Spanner's own
rendering: `WHERE score > 10` reads back as `WHERE (score > '10'::bigint)`, a
join in parentheses, a `LIMIT` cast to `bigint`, `t AS x` as `t x` with its
columns unqualified, a `SELECT *` with every column named and a `UNION`'s
columns qualified. A declared query converges with it whatever its case,
spacing and quoting. `ptah db read` prints the stored spelling.

How a view whose query Spanner rewrites converges depends on the host:

- With a host of contract 25.37 the provider reads both queries into one form,
  typed by the declared columns, so a query in any of the spellings above
  converges too.
- A query the provider cannot read, such as one with a CASE, a subquery, an
  aggregate call or `GROUP BY`, is left to the host's comparison of the two
  texts. A condition Spanner rewrites there is replaced on every plan until the
  declaration uses the stored spelling.
- A host of contracts 25.33 to 25.36 asks the provider without the declared
  columns, so a `SELECT *` view is replaced on every plan.

A materialized view, `WITH CHECK OPTION`, a view option such as
`security_invoker` and a view comment are refused: Spanner has no materialized
view, writes through no view, reports no view option back and stores no
comment.

## Sequences

A Spanner sequence is bit-reversed and takes one option, its counter start. The
provider writes a declared start as `START COUNTER WITH`:

```sql
CREATE SEQUENCE ticket_ids START WITH 1000;
```

```sql
CREATE SEQUENCE "ticket_ids" START COUNTER WITH 1000;
```

In Go, the same sequence is `//ptah:schema:sequence name="ticket_ids"
start="1000"`. A changed start is `ALTER SEQUENCE ... RESTART COUNTER WITH`,
and a removed sequence is `DROP SEQUENCE`. A sequence type other than `bigint`,
`INCREMENT`, `MINVALUE`, `MAXVALUE`, `CACHE`, `CYCLE`, `OWNED BY` and a comment
are refused. A read reports each sequence with its counter start, and refuses a
sequence that skips a range of values.

## Schemas

A declared schema, and the schema of a table a plan creates, is created with
`CREATE SCHEMA IF NOT EXISTS` before the tables in it, as Ptah's built-in
Spanner engine created it:

```sql
CREATE SCHEMA es_app;
CREATE TABLE app.t (id bigint NOT NULL, PRIMARY KEY (id));
```

```sql
CREATE SCHEMA IF NOT EXISTS "app";
CREATE SCHEMA IF NOT EXISTS "es_app";
CREATE TABLE "app"."t" (
  "id" bigint PRIMARY KEY NOT NULL
);
```

A SQL `CREATE SCHEMA`, a Go `//ptah:schema:schema` directive and an HCL
`schema` block declare a schema, and a schema with nothing in it is created
too. A read lists the schemas, so a schema the database holds is not created
again, and a schema the declaration leaves out is kept, whatever it holds. The
down file that `ptah migrations generate` writes drops a declared schema with
`DROP SCHEMA "es_app";` after the tables, without `CASCADE`. A rehearsal on a
dev database removes the schemas it created there.

This needs a Ptah of provider contract 25.29. An older Ptah refuses a schema
declared as an object of its own, as
[Clauses Ptah does not hand the provider](limitations.md#clauses-ptah-does-not-hand-the-provider)
describes, and the provider creates the schema of a Go, YAML or SQL table in a
schema of its own with `CREATE SCHEMA IF NOT EXISTS` itself. Read such tables
with `--schemas`, such as `--schemas public,app`.

## Declared rows

Rows a schema declares with `//ptah:schema:data` are typed, read and written
by the provider. Each value is compared in one form for its column type, so a
row Spanner stores in another spelling plans nothing:

- a numeric `1e3` is 1000, and `12.50` is 12.5;
- JSON members are compared in sorted order, and JSON numbers by value;
- text in `bytea` hex is the bytes it spells;
- a moment is rounded to the microsecond Spanner keeps.

A moment written without an offset is UTC. Every moment the provider writes
carries its offset, because Spanner reads a moment without one in
America/Los_Angeles.
