package serve

import (
	"github.com/NextProbe/aiprobe/internal/adapters"
	"github.com/NextProbe/aiprobe/internal/config"
	"github.com/NextProbe/aiprobe/internal/store"
)

// serveRoots is the per-runtime session directory overrides in force.
//
// Read on each scan rather than held on the Server, so editing the settings file
// and pressing Refresh picks the change up. That is the whole point of having the
// refresh button, and a value captured at startup would quietly ignore it.
func serveRoots() adapters.Roots {
	p := config.Load(store.DefaultDir())
	roots := adapters.Roots{}
	for _, k := range adapters.Kinds {
		if d := p.Dir(string(k)); d != "" {
			roots[string(k)] = d
		}
	}
	return roots
}

// serveSettings returns the settings file itself, for pages that report where they
// looked and why.
func serveSettings() *config.Paths { return config.Load(store.DefaultDir()) }
