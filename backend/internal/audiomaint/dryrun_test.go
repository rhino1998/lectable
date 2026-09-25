package audiomaint

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/rhino1998/lectable/backend/internal/store"
)

// TestDryRunAgainstRealData lists what SweepOrphans would delete in a real
// data dir, deleting nothing. Opt-in: LECTABLE_DRYRUN_DATA=<data dir> and
// LECTABLE_DRYRUN_DB=<a copy of its library.duckdb>.
func TestDryRunAgainstRealData(t *testing.T) {
	dataDir, db := os.Getenv("LECTABLE_DRYRUN_DATA"), os.Getenv("LECTABLE_DRYRUN_DB")
	if dataDir == "" || db == "" {
		t.Skip("set LECTABLE_DRYRUN_DATA and LECTABLE_DRYRUN_DB")
	}
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	refs, err := st.AudioRefs()
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	kindBytes := map[string]int64{}
	stats := sweepOrphans(context.Background(), dataDir, refs, time.Now().Add(-sweepGrace), func(path string) error {
		rel, _ := filepath.Rel(filepath.Join(dataDir, "audio"), path)
		parts := strings.Split(rel, string(filepath.Separator))
		kind := "book dir"
		switch {
		case len(parts) == 2:
			kind = "chapter dir"
		case len(parts) == 3 && parts[1] == "ambience":
			kind = "ambience loop"
		case len(parts) == 3 && voiceDirName.MatchString(parts[2]):
			kind = "voice dir"
		case len(parts) == 4 && parts[2] == "music":
			kind = "music file"
		case len(parts) == 4:
			kind = "temp file"
		}
		kinds[kind]++
		kindBytes[kind] += diskUsage(path)
		return nil
	})
	var names []string
	for k := range kinds {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		fmt.Printf("would remove %5d %-14s %s\n", kinds[k], k, mb(kindBytes[k]))
	}
	fmt.Printf("total: %d entries, %s\n", stats.Removed, mb(stats.BytesRemoved))
}
