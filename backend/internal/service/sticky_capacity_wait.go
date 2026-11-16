package service

import "time"

const minimumStickySessionWait = 10 * time.Second

// A brief capacity spike should keep an established conversation on the account
// that already owns its prompt cache. Queue limits still apply at admission.
func stickySessionWaitTimeout(timeout time.Duration) time.Duration {
	if timeout < minimumStickySessionWait {
		return minimumStickySessionWait
	}
	return timeout
}
