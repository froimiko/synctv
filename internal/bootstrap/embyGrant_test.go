package bootstrap

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestRunEmbyGrantCleanupStartsTicksContinuesAfterErrorAndCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time)
	calls := make(chan int, 3)
	var mu sync.Mutex
	count := 0
	cleanup := func(time.Time) error {
		mu.Lock()
		count++
		current := count
		mu.Unlock()
		calls <- current
		if current == 1 {
			return errors.New("test cleanup failure")
		}
		return nil
	}
	done := make(chan struct{})
	go func() {
		runEmbyGrantCleanup(ctx, ticks, func() time.Time { return time.Unix(1, 0) }, cleanup)
		close(done)
	}()

	if call := <-calls; call != 1 {
		t.Fatalf("startup call = %d", call)
	}
	ticks <- time.Time{}
	if call := <-calls; call != 2 {
		t.Fatalf("tick call = %d", call)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cleanup worker did not stop after cancellation")
	}
}
