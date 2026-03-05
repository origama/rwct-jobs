package retry

import (
	"context"
	"time"
)

func Exponential(ctx context.Context, attempts int, baseDelay time.Duration, fn func(int) error) error {
	if attempts < 1 {
		attempts = 1
	}
	var err error
	for i := 1; i <= attempts; i++ {
		err = fn(i)
		if err == nil {
			return nil
		}
		if i == attempts {
			break
		}
		d := baseDelay << (i - 1)
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}
