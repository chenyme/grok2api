package gateway

import "time"

// accountCreationWarmup keeps newly imported accounts out of the front of the
// queue while their billing, quota, and model capability snapshots settle.
const accountCreationWarmup = 30 * time.Minute

func accountCreatedWithinWarmup(createdAt, now time.Time) bool {
	return !createdAt.IsZero() && now.Sub(createdAt) < accountCreationWarmup
}
