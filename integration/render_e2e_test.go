//go:build integration

package integration_test

import (
	"path/filepath"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// renderedSchema declares a table with a key, a default, a named CHECK and a
// row deletion policy, a unique index, a foreign key, a table in a schema of
// its own and a view.
const renderedSchema = `CREATE TABLE p (id bigint PRIMARY KEY, name varchar(20) NOT NULL DEFAULT 'x', score bigint CONSTRAINT score_ck CHECK (score >= 0),
  created_at timestamptz) TTL INTERVAL '30 days' ON created_at;
CREATE UNIQUE INDEX p_name_idx ON p (name);
CREATE TABLE ch (id bigint PRIMARY KEY, p_id bigint, CONSTRAINT ch_p_fk FOREIGN KEY (p_id) REFERENCES p (id));
CREATE SCHEMA app;
CREATE TABLE app.q (id bigint PRIMARY KEY);
CREATE VIEW pv AS SELECT id, name FROM p;
`

// `ptah schema render --dialect spanner` writes a schema's statements without
// a database. The host names a table of the default schema without its
// schema when no session reads the search path, and the provider plans it in
// public. Without that default the provider refuses every table: `a Spanner
// table operation names a table by its schema and name and nothing else`.
func TestHostRendersASchemaWithoutADatabaseE2E(t *testing.T) {
	c := testcheck.New(t)
	path := filepath.Join(writeFiles(c, map[string]string{"schema.sql": renderedSchema}), "schema.sql")
	output, err := hostCommand(c, "schema", "render", "--dialect", "spanner", "--schema-file", path)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, `-- Statement 1/7
CREATE SCHEMA IF NOT EXISTS "app";

-- Statement 2/7
CREATE TABLE "app"."q" (
  "id" bigint PRIMARY KEY NOT NULL
);

-- Statement 3/7
CREATE TABLE "p" (
  "id" bigint PRIMARY KEY NOT NULL,
  "name" varchar(20) NOT NULL DEFAULT 'x',
  "score" bigint CONSTRAINT "score_ck" CHECK (score >= 0),
  "created_at" timestamptz
) TTL INTERVAL '30 days' ON "created_at";

-- Statement 4/7
CREATE TABLE "ch" (
  "id" bigint PRIMARY KEY NOT NULL,
  "p_id" bigint
);

-- Statement 5/7
CREATE UNIQUE INDEX "p_name_idx" ON "p" ("name");

-- Statement 6/7
ALTER TABLE "ch" ADD CONSTRAINT "ch_p_fk" FOREIGN KEY ("p_id") REFERENCES "p"("id");

-- Statement 7/7
CREATE VIEW "pv" AS
SELECT id, name FROM p;
`)
}
