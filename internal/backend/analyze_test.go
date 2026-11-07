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

// The analysis of what Render writes passes the SDK's framing check: every
// byte belongs to a statement or to the white space between them.
func TestAnalyzeSQL_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	request := &protocol.AnalyzeSQLRequest{Engine: "spanner", Sources: []string{
		"-- Migration\nCREATE TABLE \"t\" (\n  \"id\" bigint NOT NULL,\n  PRIMARY KEY (\"id\")\n);\nALTER TABLE \"t\" ADD COLUMN \"n\" text;\n",
		"DROP TABLE \"t\";",
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
		{name: "a statement the provider does not know", request: &protocol.AnalyzeSQLRequest{Engine: "spanner", Sources: []string{"DROP TABLE t;", "CREATE INDEX i ON t (a);"}},
			wantErr: `.*the provider analyzes CREATE TABLE, DROP TABLE and ALTER TABLE.*`},
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
