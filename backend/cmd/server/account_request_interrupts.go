package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/accountrequest"
	"github.com/redis/go-redis/v9"
)

// Every serving process subscribes, including workers without scheduled probes.
// Keep local interruption available while Redis is temporarily unavailable.
func startAccountRequestInterrupts(rdb redis.UniversalClient) func() {
	ctx, cancel := context.WithCancel(context.Background())
	stop, err := accountrequest.Default.Start(ctx, rdb)
	if err == nil {
		return sync.OnceFunc(func() { cancel(); stop() })
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("retrying account request interruption subscription", "error", err)
			timer := time.NewTimer(time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			stop, err = accountrequest.Default.Start(ctx, rdb)
			if err == nil {
				<-ctx.Done()
				stop()
				return
			}
		}
	}()
	return sync.OnceFunc(func() { cancel(); <-done })
}
