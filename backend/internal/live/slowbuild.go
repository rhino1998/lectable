package live

import (
	"log"
	"os"
	"strconv"
	"sync"
	"time"
)

// slowBuildThreshold is how long a topic build can take before it's logged
// (LECTABLE_LIVE_SLOW_BUILD_MS, default 500; 0 or negative disables). A slow
// topic is rebuilt on every relevant write, so it quietly costs a database
// read connection and a core for as long as generation runs; this makes
// that visible.
var slowBuildThreshold = func() time.Duration {
	if v := os.Getenv("LECTABLE_LIVE_SLOW_BUILD_MS"); v != "" {
		if ms, err := strconv.Atoi(v); err == nil {
			return time.Duration(ms) * time.Millisecond
		}
		log.Printf("live: LECTABLE_LIVE_SLOW_BUILD_MS=%q is not a valid int, using 500", v)
	}
	return 500 * time.Millisecond
}()

// slowBuildLogEvery rate-limits the warning per topic kind, since a slow
// topic is usually rebuilt over and over.
const slowBuildLogEvery = time.Minute

var slowBuildLogged sync.Map // topic kind -> time.Time of last warning

// timedBuild runs build and warns (rate-limited) when it takes longer than
// slowBuildThreshold.
func timedBuild(key string, build func() (any, error)) (any, error) {
	start := time.Now()
	v, err := build()
	d := time.Since(start)
	if slowBuildThreshold > 0 && d >= slowBuildThreshold {
		kind := topicKind(key)
		now := time.Now()
		if last, ok := slowBuildLogged.Load(kind); !ok || now.Sub(last.(time.Time)) >= slowBuildLogEvery {
			slowBuildLogged.Store(kind, now)
			log.Printf("live: slow build %s took %s (warned at most once a minute per topic kind)", key, d.Round(time.Millisecond))
		}
	}
	return v, err
}

// topicKind is a topic key's kind - "speakers" for "speakers:<bookId>".
func topicKind(key string) string {
	for i := 0; i < len(key); i++ {
		if key[i] == ':' {
			return key[:i]
		}
	}
	return key
}
