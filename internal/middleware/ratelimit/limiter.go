// Package ratelimit provides rate limiting middleware for Telegram API calls.
// It controls the frequency of requests to avoid hitting API limits.
package ratelimit

import (
	"context"

	"golang.org/x/time/rate"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"

	"tg-down/internal/logger"
)

const (
	// DefaultRate is the default rate limit (requests per second)
	DefaultRate = rate.Limit(1.0) // 1 request per second
	// DefaultBurst is the default burst size
	DefaultBurst = 2
)

// Limiter provides rate limiting for Telegram API calls
type Limiter struct {
	limiter *rate.Limiter
	logger  *logger.Logger
}

// New creates a new rate limiter
func New(r rate.Limit, burst int, logger *logger.Logger) *Limiter {
	return &Limiter{
		limiter: rate.NewLimiter(r, burst),
		logger:  logger,
	}
}

// NewDefault creates a new rate limiter with default settings
func NewDefault(logger *logger.Logger) *Limiter {
	return New(DefaultRate, DefaultBurst, logger)
}

// Handle implements telegram.Middleware interface
func (l *Limiter) Handle(next tg.Invoker) telegram.InvokeFunc {
	return func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
		// Wait for rate limiter to allow the request
		if err := l.limiter.Wait(ctx); err != nil {
			l.logger.Error("Rate limiter wait failed: %v", err)
			return err
		}

		l.logger.Debug("Rate limiter allowed request")
		return next.Invoke(ctx, input, output)
	}
}

// SetRate updates the rate limit
func (l *Limiter) SetRate(r rate.Limit) {
	l.limiter.SetLimit(r)
	l.logger.Info("Rate limit updated to: %v requests/second", float64(r))
}

// SetBurst updates the burst size
func (l *Limiter) SetBurst(burst int) {
	l.limiter.SetBurst(burst)
	l.logger.Info("Rate limiter burst size updated to: %d", burst)
}
