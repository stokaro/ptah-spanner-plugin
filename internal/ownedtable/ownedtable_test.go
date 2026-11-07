package ownedtable_test

import (
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-spanner-plugin/internal/ownedtable"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

var definition = ownedtable.Definition{
	Name:       "records",
	Columns:    []ownedtable.Column{{Name: "id", Type: "bigint"}, {Name: "note", Type: "character varying", Nullable: true}},
	PrimaryKey: []string{"id"},
}

// A table holds the layout only with exactly its columns, in order, with
// their types and nullability, and exactly its primary key.
func TestDefinition_Holds(t *testing.T) {
	tests := []struct {
		name    string
		columns []ownedtable.Column
		key     []string
		want    bool
	}{
		{name: "the layout", columns: definition.Columns, key: []string{"id"}, want: true},
		{name: "another type", columns: []ownedtable.Column{{Name: "id", Type: "bigint"}, {Name: "note", Type: "character varying(10)", Nullable: true}}, key: []string{"id"}},
		{name: "another nullability", columns: []ownedtable.Column{{Name: "id", Type: "bigint"}, {Name: "note", Type: "character varying"}}, key: []string{"id"}},
		{name: "another order", columns: []ownedtable.Column{{Name: "note", Type: "character varying", Nullable: true}, {Name: "id", Type: "bigint"}}, key: []string{"id"}},
		{name: "an extra column", columns: append(append([]ownedtable.Column(nil), definition.Columns...), ownedtable.Column{Name: "x", Type: "bigint"}), key: []string{"id"}},
		{name: "another key", columns: definition.Columns, key: []string{"id", "note"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(definition.Holds(test.columns, test.key), qt.Equals, test.want)
		})
	}
}

// A table in the default schema is named alone, one in another schema with
// its schema.
func TestDefinition_Quoted(t *testing.T) {
	c := testcheck.New(t)
	c.Assert(definition.Quoted(), qt.Equals, `"records"`)
	c.Assert(definition.SchemaName(), qt.Equals, "public")
	other := definition
	other.Schema = "ops"
	c.Assert(other.Quoted(), qt.Equals, `"ops"."records"`)
}
