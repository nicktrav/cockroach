package debug

import (
	"encoding/hex"
	"fmt"
	"os"
	"testing"
	"text/template"

	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/stretchr/testify/require"
)

func Test(t *testing.T) {
	type TableLevelStats struct {
		Start, End         string
		StartByte, EndByte string
		Files              int
		Bytes              uint64
		Sublevels          int
	}
	type StoreStats struct {
		Store  roachpb.StoreID
		Levels [7][]TableLevelStats
	}

	data := []StoreStats{
		{
			Store: 1,
			Levels: [7][]TableLevelStats{
				{
					TableLevelStats{
						Start:     "/Table/1",
						End:       "/Table/2",
						Files:     10,
						Bytes:     20,
						Sublevels: 1,
					},
				},
			},
		},
	}

	tmpl, err := template.New("test").Parse(debugSpansAllTablesTemplate)
	require.NoError(t, err)
	err = tmpl.Execute(os.Stdout, data)
	require.NoError(t, err)
}

func Test2(t *testing.T) {
	start := []byte{'\x00', '\x01'}
	s := hex.EncodeToString(start)
	fmt.Println(s)
}
