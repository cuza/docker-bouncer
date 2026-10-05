//go:build e2e

package cli

import (
	"os"
	"time"
)

// The e2e build of the plugin can deploy a stack as another format would:
// BOUNCER_E2E_FORMAT=2.0, or "none" for a stack from before formats; and
// BOUNCER_E2E_LOCK_DELAY=10s holds a run before it takes the lock.
func init() {
	if f := os.Getenv("BOUNCER_E2E_FORMAT"); f != "" {
		writeFormat = f
	}
	e2eLockDelay, _ = time.ParseDuration(os.Getenv("BOUNCER_E2E_LOCK_DELAY"))
}
