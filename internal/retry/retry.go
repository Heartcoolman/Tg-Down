// Package retry provides retry mechanisms with exponential backoff.
// It handles transient errors and implements smart retry strategies.
package retry

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"time"

	"tg-down/internal/logger"
)

const (
	// DefaultMaxRetries is the default maximum number of retries
	DefaultMaxRetries = 3
	// DefaultBaseDelay is the default base delay for exponential backoff
	DefaultBaseDelay = time.Second
	// DefaultMaxDelay is the default maximum delay between retries
	DefaultMaxDelay = 30 * time.Second
	// DefaultJitterFactor is the default jitter factor to add randomness
	DefaultJitterFactor = 0.1
)

// Config holds retry configuration
type Config struct {
	MaxRetries   int
	BaseDelay    time.Duration
	MaxDelay     time.Duration
	JitterFactor float64
	ShouldRetry  func(error) bool
	OnRetry      func(attempt int, err error, delay time.Duration)
}

// DefaultConfig returns a default retry configuration
func DefaultConfig(logger *logger.Logger) *Config {
	return &Config{
		MaxRetries:   DefaultMaxRetries,
		BaseDelay:    DefaultBaseDelay,
		MaxDelay:     DefaultMaxDelay,
		JitterFactor: DefaultJitterFactor,
		ShouldRetry:  DefaultShouldRetry,
		OnRetry: func(attempt int, err error, delay time.Duration) {
			logger.Warn("Retry attempt %d after error: %v (waiting %v)", attempt, err, delay)
		},
	}
}

// DefaultShouldRetry determines if an error should trigger a retry
func DefaultShouldRetry(err error) bool {
	if err == nil {
		return false
	}

	// Add specific error types that should be retried
	errStr := err.Error()

	// Network-related errors
	if contains(errStr, "connection") ||
		contains(errStr, "timeout") ||
		contains(errStr, "network") ||
		contains(errStr, "temporary") {
		return true
	}

	// Telegram-specific errors that should be retried
	if contains(errStr, "INTERNAL_SERVER_ERROR") ||
		contains(errStr, "NETWORK_MIGRATE") ||
		contains(errStr, "PHONE_MIGRATE") ||
		contains(errStr, "FILE_MIGRATE") ||
		contains(errStr, "USER_MIGRATE") ||
		contains(errStr, "STATS_MIGRATE") {
		return true
	}

	return false
}

// contains checks if a string contains a substring (case-insensitive)
func contains(s, substr string) bool {
	return len(s) >= len(substr) &&
		(s == substr ||
			(len(s) > len(substr) &&
				anySubstring(s, substr)))
}

func anySubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// Retrier handles retry logic with exponential backoff
type Retrier struct {
	config *Config
	logger *logger.Logger
}

// New creates a new retrier with the given configuration
func New(config *Config, logger *logger.Logger) *Retrier {
	if config == nil {
		config = DefaultConfig(logger)
	}
	return &Retrier{
		config: config,
		logger: logger,
	}
}

// NewDefault creates a new retrier with default configuration
func NewDefault(logger *logger.Logger) *Retrier {
	return New(DefaultConfig(logger), logger)
}

// Do executes a function with retry logic
func (r *Retrier) Do(ctx context.Context, fn func() error) error {
	var lastErr error

	for attempt := 0; attempt <= r.config.MaxRetries; attempt++ {
		// Check context cancellation
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Execute the function
		err := fn()
		if err == nil {
			if attempt > 0 {
				r.logger.Info("Operation succeeded after %d retries", attempt)
			}
			return nil
		}

		lastErr = err

		// Check if we should retry this error
		if !r.config.ShouldRetry(err) {
			r.logger.Debug("Error not retryable: %v", err)
			return err
		}

		// Don't wait after the last attempt
		if attempt == r.config.MaxRetries {
			break
		}

		// Calculate delay with exponential backoff and jitter
		delay := r.calculateDelay(attempt)

		// Call retry callback
		if r.config.OnRetry != nil {
			r.config.OnRetry(attempt+1, err, delay)
		}

		// Wait before retry
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}

	return fmt.Errorf("operation failed after %d retries, last error: %w", r.config.MaxRetries, lastErr)
}

// DoWithResult executes a function that returns a result and error with retry logic
func (r *Retrier) DoWithResult(ctx context.Context, fn func() (interface{}, error)) (interface{}, error) {
	var result interface{}
	err := r.Do(ctx, func() error {
		var err error
		result, err = fn()
		return err
	})
	return result, err
}

// calculateDelay calculates the delay for the given attempt using exponential backoff with jitter
func (r *Retrier) calculateDelay(attempt int) time.Duration {
	// Exponential backoff: baseDelay * 2^attempt
	delay := float64(r.config.BaseDelay) * math.Pow(2, float64(attempt))

	// Apply maximum delay limit
	if delay > float64(r.config.MaxDelay) {
		delay = float64(r.config.MaxDelay)
	}

	// Add jitter to avoid thundering herd
	if r.config.JitterFactor > 0 {
		jitter := delay * r.config.JitterFactor * (rand.Float64()*2 - 1) // Random between -jitterFactor and +jitterFactor
		delay += jitter

		// Ensure delay is not negative
		if delay < 0 {
			delay = float64(r.config.BaseDelay)
		}
	}

	return time.Duration(delay)
}

// WithMaxRetries creates a new retrier with updated max retries
func (r *Retrier) WithMaxRetries(maxRetries int) *Retrier {
	newConfig := *r.config
	newConfig.MaxRetries = maxRetries
	return &Retrier{
		config: &newConfig,
		logger: r.logger,
	}
}

// WithBaseDelay creates a new retrier with updated base delay
func (r *Retrier) WithBaseDelay(baseDelay time.Duration) *Retrier {
	newConfig := *r.config
	newConfig.BaseDelay = baseDelay
	return &Retrier{
		config: &newConfig,
		logger: r.logger,
	}
}

// WithMaxDelay creates a new retrier with updated max delay
func (r *Retrier) WithMaxDelay(maxDelay time.Duration) *Retrier {
	newConfig := *r.config
	newConfig.MaxDelay = maxDelay
	return &Retrier{
		config: &newConfig,
		logger: r.logger,
	}
}

// WithShouldRetry creates a new retrier with custom retry logic
func (r *Retrier) WithShouldRetry(shouldRetry func(error) bool) *Retrier {
	newConfig := *r.config
	newConfig.ShouldRetry = shouldRetry
	return &Retrier{
		config: &newConfig,
		logger: r.logger,
	}
}
