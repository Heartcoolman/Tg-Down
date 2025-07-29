// Package floodwait provides flood wait handling middleware for Telegram API calls.
// It automatically handles FLOOD_WAIT errors with intelligent retry mechanisms.
package floodwait

import (
	"context"
	"fmt"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"tg-down/internal/logger"
)

const (
	// DefaultMaxRetries is the default maximum number of retries
	DefaultMaxRetries = 5
	// DefaultMaxWait is the default maximum wait time per attempt
	DefaultMaxWait = 5 * time.Minute
	// MinWaitTime is the minimum wait time for flood wait
	MinWaitTime = time.Second
)

// Waiter handles flood wait errors with intelligent retry mechanisms
type Waiter struct {
	maxRetries int
	maxWait    time.Duration
	logger     *logger.Logger
	onWait     func(ctx context.Context, duration time.Duration)
}

// New creates a new flood wait handler
func New(logger *logger.Logger) *Waiter {
	return &Waiter{
		maxRetries: DefaultMaxRetries,
		maxWait:    DefaultMaxWait,
		logger:     logger,
		onWait:     func(_ context.Context, _ time.Duration) {},
	}
}

// WithMaxRetries sets the maximum number of retries
func (w *Waiter) WithMaxRetries(maxRetries int) *Waiter {
	w.maxRetries = maxRetries
	return w
}

// WithMaxWait sets the maximum wait time per attempt
func (w *Waiter) WithMaxWait(maxWait time.Duration) *Waiter {
	w.maxWait = maxWait
	return w
}

// WithCallback sets a callback function for flood wait events
func (w *Waiter) WithCallback(callback func(ctx context.Context, duration time.Duration)) *Waiter {
	w.onWait = callback
	return w
}

// Handle implements telegram.Middleware interface
func (w *Waiter) Handle(next tg.Invoker) telegram.InvokeFunc {
	return func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
		var lastErr error

		for attempt := 0; attempt <= w.maxRetries; attempt++ {
			err := next.Invoke(ctx, input, output)
			if err == nil {
				return nil
			}

			// Check if it's a flood wait error
			floodWait, ok := tgerr.AsFloodWait(err)
			if !ok {
				// Not a flood wait error, return immediately
				return err
			}

			lastErr = err

			// Check if we've exceeded max retries
			if attempt >= w.maxRetries {
				w.logger.Error("Flood wait retry limit exceeded (%d attempts)", attempt+1)
				return fmt.Errorf("flood wait retry limit exceeded after %d attempts: %w", attempt+1, err)
			}

			// Ensure minimum wait time
			waitDuration := floodWait
			if waitDuration < MinWaitTime {
				waitDuration = MinWaitTime
			}

			// Check if wait time exceeds maximum allowed
			if waitDuration > w.maxWait {
				w.logger.Error("Flood wait duration too long: %v (max: %v)", waitDuration, w.maxWait)
				return fmt.Errorf("flood wait duration too long (%v > %v): %w", waitDuration, w.maxWait, err)
			}

			w.logger.Warn("Flood wait detected, waiting %v (attempt %d/%d)", waitDuration, attempt+1, w.maxRetries+1)

			// Call the callback if set
			w.onWait(ctx, waitDuration)

			// Wait for the specified duration
			select {
			case <-time.After(waitDuration):
				// Continue to next attempt
				w.logger.Debug("Flood wait completed, retrying...")
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		return lastErr
	}
}
