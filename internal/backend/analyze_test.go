package backend_test

import (
	"context"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/stokaro/ptah-spanner-plugin/internal/backend"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// The analysis of what Render writes, and of the rows a migration body writes
// beside its schema statements, passes the SDK's framing check: every byte
// belongs to a statement or to the white space between them.
func TestAnalyzeSQL_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	request := &protocol.AnalyzeSQLRequest{Engine: "spanner", Sources: []string{
		"-- Migration\nCREATE TABLE \"t\" (\n  \"id\" bigint NOT NULL,\n  PRIMARY KEY (\"id\")\n);\nALTER TABLE \"t\" ADD COLUMN \"n\" text;\n",
		"DROP TABLE \"t\";",
		"INSERT INTO t (id) VALUES (1);\nUPDATE t SET n = 'x';\nDELETE FROM t WHERE id = 1;\nSELECT 1;",
	}}
	result, err := backend.New().AnalyzeSQL(context.Background(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidateSQLAnalysis(request, result), qt.IsNil)
	var canonical []string
	for _, file := range result.GetFiles() {
		for _, statement := range file.GetStatements() {
			canonical = append(canonical, statement.GetCanonical())
			c.Assert(statement.GetSafety(), qt.Equals, protocol.SQLStatement_COMMON_DDL)
		}
	}
	c.Assert(canonical, qt.DeepEquals, []string{
		`CREATE TABLE "t" ( "id" BIGINT NOT NULL , PRIMARY KEY ( "id" ) )`,
		`ALTER TABLE "t" ADD COLUMN "n" TEXT`,
		`DROP TABLE "t"`,
		`INSERT INTO T ( ID ) VALUES ( 1 )`,
		`UPDATE T SET N = 'x'`,
		`DELETE FROM T WHERE ID = 1`,
		`SELECT 1`,
	})
}

func TestAnalyzeSQL_FailurePath(t *testing.T) {
	tests := []struct {
		name    string
		request *protocol.AnalyzeSQLRequest
		wantErr string
	}{
		{name: "another engine", request: &protocol.AnalyzeSQLRequest{Engine: "postgres", Sources: []string{"DROP TABLE t;"}},
			wantErr: `.*the provider analyzes the SQL of the spanner engine only.*`},
		{name: "a statement the provider does not know", request: &protocol.AnalyzeSQLRequest{Engine: "spanner", Sources: []string{"DROP TABLE t;", "CREATE SCHEMA app;"}},
			wantErr: `.*the provider analyzes CREATE and DROP of a TABLE, an INDEX, a VIEW or a SEQUENCE, .*`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			result, err := backend.New().AnalyzeSQL(context.Background(), test.request)
			c.Assert(status.Code(err), qt.Equals, codes.InvalidArgument)
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(result, qt.IsNil)
		})
	}
}
