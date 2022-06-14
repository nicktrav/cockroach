package debug

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"net/http"

	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/security/username"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/sessiondata"
	"github.com/cockroachdb/cockroach/pkg/storage"
	"github.com/dustin/go-humanize"
)

func (ds *Server) registerDebugSpans(sql *sql.InternalExecutor, engines []storage.Engine) {
	ds.mux.HandleFunc("/debug/lsm/spans", func(w http.ResponseWriter, req *http.Request) {
		out, err := ds.debugSpansAllTables(sql, engines)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(out)
	})
	ds.mux.HandleFunc("/debug/lsm/span", func(w http.ResponseWriter, req *http.Request) {
		var start, end []byte
		var err error
		q := req.URL.Query()
		if s := q.Get("start"); s != "" {
			start, err = hex.DecodeString(s)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		if s := q.Get("end"); s != "" {
			end, err = hex.DecodeString(s)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		out, err := ds.debugSpan(engines, start, end)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(out)
	})
}

func (ds *Server) storeIDs(engines []storage.Engine) ([]roachpb.StoreIdent, error) {
	storeIDs := make([]roachpb.StoreIdent, len(engines))
	for i := range engines {
		id, err := kvserver.ReadStoreIdent(context.Background(), engines[i])
		if err != nil {
			return nil, err
		}
		storeIDs[i] = id
	}
	return storeIDs, nil
}

func (ds *Server) debugSpan(engines []storage.Engine, start, end []byte) ([]byte, error) {
	ids, err := ds.storeIDs(engines)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	for i, e := range engines {
		_, _ = fmt.Fprintf(&b, "Store: %d\n", ids[i].StoreID)
		s := e.DebugSpan(start, end)
		b.WriteString(s.String() + "\n")
	}
	return b.Bytes(), nil
}

func (ds *Server) debugSpansAllTables(sql *sql.InternalExecutor, engines []storage.Engine) ([]byte, error) {
	type tableLevelStats struct {
		start, end string
		files      int
		bytes      uint64
		sublevels  int
	}
	type storeStats struct {
		store  roachpb.StoreIdent
		levels [7][]tableLevelStats
	}

	ids, err := ds.storeIDs(engines)
	if err != nil {
		return nil, err
	}

	tables, err := ds.queryTableSpans(sql)
	if err != nil {
		return nil, err
	}

	var ss []storeStats
	for i, e := range engines {
		s := storeStats{store: ids[i]}
		for _, t := range tables {
			spanStats := e.DebugSpan(t.startKey, t.endKey)
			for j, l := range spanStats.Levels {
				if l.NumFiles == 0 {
					continue
				}
				s.levels[j] = append(s.levels[j], tableLevelStats{
					start:     t.startPretty,
					end:       t.endPretty,
					files:     l.NumFiles,
					bytes:     l.TotalBytes,
					sublevels: l.NumSubLevels,
				})
			}
		}
		ss = append(ss, s)
	}

	// Format the output and return ....
	// FIXME(travers): use a Go template for this.
	// TODO(travers): hyperlinks out to the /debug/lsm/span page with start / end.
	// TODO(travers): expand / collapse for each level.
	var b bytes.Buffer
	for _, s := range ss {
		fmt.Fprintf(&b, "Store %d:\n", s.store.StoreID)
		for i, level := range s.levels {
			fmt.Fprintf(&b, "--- L%d ---\n", i)
			for _, t := range level {
				fmt.Fprintf(
					&b, "  [%s,%s): %d files, %s",
					t.start, t.end, t.files, humanize.Bytes(t.bytes),
				)
				if i == 0 {
					fmt.Fprintf(&b, ", sublevels: %d", t.sublevels)
				}
				fmt.Fprint(&b, "\n")
			}
		}
		fmt.Fprint(&b, "\n")
	}

	return b.Bytes(), nil
}

type tableSpan struct {
	tableName              string
	startKey, endKey       []byte
	startPretty, endPretty string
}

// localSpans are additional local key spans to be included in the Pebble debug
// span requests, that aren't reflected in the range table. See
// `pkg/keys/doc.go` for background on these local spans.
var localSpans = []tableSpan{
	{
		tableName:   "min",
		startKey:    keys.MinKey,
		endKey:      keys.LocalPrefix,
		startPretty: "/Min",
		endPretty:   "/Local",
	},
	{
		tableName:   "local",
		startKey:    keys.LocalPrefix,
		endKey:      keys.Meta1Prefix,
		startPretty: "/Local",
		endPretty:   "/Meta1",
	},
	{
		tableName:   "meta1",
		startKey:    keys.Meta1Prefix,
		endKey:      keys.Meta2Prefix,
		startPretty: "/Meta1",
		endPretty:   "/Meta2",
	},
	{
		tableName:   "meta2",
		startKey:    keys.Meta2Prefix,
		endKey:      keys.SystemPrefix,
		startPretty: "/Meta1",
		endPretty:   "/Meta2",
	},
	{
		tableName:   "system",
		startKey:    keys.SystemPrefix,
		endKey:      keys.TableDataMin,
		startPretty: "/System",
		endPretty:   "/Table/0",
	},
}

// FIXME: document this.
const pebbleDebugSpansQuery = `
WITH min_max AS (
  SELECT
    table_id,
    range_id,
    ROW_NUMBER() OVER (PARTITION BY table_id ORDER BY start_key ASC) AS rn_start,
    ROW_NUMBER() OVER (PARTITION BY table_id ORDER BY end_key DESC) AS rn_end
  FROM crdb_internal.ranges_no_leases
  WHERE table_id != 0
), lower AS (
  SELECT DISTINCT r.table_id, r.range_id, r.start_key, r.start_pretty
  FROM min_max mm
  INNER JOIN crdb_internal.ranges_no_leases r
    ON mm.range_id = r.range_id
  WHERE rn_start = 1
), upper AS (
  SELECT DISTINCT r.table_id, r.range_id, r.end_key, r.end_pretty
  FROM min_max mm
  INNER JOIN crdb_internal.ranges_no_leases r
    ON mm.range_id = r.range_id
  WHERE rn_end = 1
)
SELECT DISTINCT
  r.table_name,
  l.start_key,
  u.end_key,
  l.start_pretty,
  u.end_pretty
FROM min_max mm
INNER JOIN lower l
  ON mm.table_id = l.table_id
INNER JOIN upper u
  ON mm.table_id = u.table_id
INNER JOIN crdb_internal.ranges_no_leases r
  ON mm.range_id = r.range_id
`

func (ds *Server) queryTableSpans(sqlExecutor *sql.InternalExecutor) ([]tableSpan, error) {
	var spans []tableSpan

	// Add some additional spans that we can't get from the ranges table. These
	// are taken from `pkg/keys/doc.go`.
	spans = append(spans, localSpans...)

	ctx := context.Background()
	it, err := sqlExecutor.QueryIteratorEx(
		ctx, "debug-lsm-span-debug", nil, /* txn */
		sessiondata.InternalExecutorOverride{User: username.RootUserName()},
		pebbleDebugSpansQuery,
	)
	if err != nil {
		return nil, err
	}
	defer it.Close()

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
