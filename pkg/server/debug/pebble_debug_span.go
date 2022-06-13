package debug

import (
	"context"

	"github.com/cockroachdb/cockroach/pkg/security/username"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/sessiondata"
	"github.com/cockroachdb/cockroach/pkg/storage"
)

type tableSpan struct {
	tableId                int
	tableName              string
	startKey, endKey       []byte
	startPretty, endPretty string
}

func (ds *Server) queryTableSpans(sqlExecutor *sql.InternalExecutor) ([]tableSpan, error) {
	query := `WITH min_max AS (
SELECT
  table_id,
  range_id,
  ROW_NUMBER() OVER (PARTITION BY table_id ORDER BY start_key ASC) AS rn_start,
  ROW_NUMBER() OVER (PARTITION BY table_id ORDER BY end_key ASC) AS rn_end
FROM crdb_internal.ranges_no_leases
)
SELECT DISTINCT
  r.table_id,
  r.table_name,
  r.start_key,
  r.end_key,
  r.start_pretty,
  r.end_pretty
FROM min_max mm
INNER JOIN crdb_internal.ranges_no_leases r
ON mm.range_id = r.range_id
WHERE mm.rn_start = 1 AND mm.rn_end = 1`
	ctx := context.Background()
	it, err := sqlExecutor.QueryIteratorEx(
		ctx, "debug-lsm-span-debug", nil, /* txn */
		sessiondata.InternalExecutorOverride{User: username.RootUserName()},
		query)
	if err != nil {
		return nil, err
	}
	defer it.Close()

	var spans []tableSpan
	ok, err := it.Next(ctx)
	if err != nil {
		return nil, err
	}

	if !ok {
		return spans, nil
	}

	scanner := makeResultScanner(it.Types())
	for ; ok; ok, err = it.Next(ctx) {
		row := it.Cur()
		var span tableSpan
		if err := scanner.Scan(row, "table_id", &span.tableId); err != nil {
			return nil, err
		}
		if err := scanner.Scan(row, "table_name", &span.tableName); err != nil {
			return nil, err
		}
		if err := scanner.Scan(row, "start_key", &span.startKey); err != nil {
			return nil, err
		}
		if err := scanner.Scan(row, "end_key", &span.endKey); err != nil {
			return nil, err
		}
		if err := scanner.Scan(row, "start_pretty", &span.startPretty); err != nil {
			return nil, err
		}
		if err := scanner.Scan(row, "end_pretty", &span.endPretty); err != nil {
			return nil, err
		}
		spans = append(spans, span)
	}
	return spans, err
}

func (ds *Server) getDebugSpansAllTables(engine *storage.Engine, start, end []byte) {

}

func (ds *Server) getDebugSpan(engine *storage.Engine, start, end []byte) {
}
