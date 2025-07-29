// Package retry provides retry mechanisms with exponential backoff.
// It handles transient errors and implements smart retry strategies.
package retry

import (
	"context"
	"crypto/rand"
	"fmt"
	"math"
	"math/big"
	"strings"
	"time"

	"tg-down/internal/logger"
)

const (
	// DefaultMaxRetries is the default maximum number of retries
	DefaultMaxRetries = 3
	// DefaultBaseDelay is the default base delay for exponential backoff
	DefaultBaseDelay = 1 * time.Second
	// DefaultMaxDelay is the default maximum delay between retries
	DefaultMaxDelay = 30 * time.Second
	// DefaultJitterFactor is the default jitter factor to add randomness
	DefaultJitterFactor = 0.1

	// ExponentialBackoffBase is the base for exponential backoff calculation
	ExponentialBackoffBase = 2.0
	// JitterMultiplier is used for jitter calculation
	JitterMultiplier = 2.0
	// JitterOffset is used to center jitter around zero
	JitterOffset = 1.0
	// RandomPrecision is the precision for random number generation
	RandomPrecision = 1000
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
	if strings.Contains(errStr, "connection") ||
		strings.Contains(errStr, "timeout") ||
		strings.Contains(errStr, "network") ||
		strings.Contains(errStr, "temporary") {
		return true
	}

	// Telegram-specific errors that should be retried
	if strings.Contains(errStr, "INTERNAL_SERVER_ERROR") ||
		strings.Contains(errStr, "NETWORK_MIGRATE") ||
		strings.Contains(errStr, "PHONE_MIGRATE") ||
		strings.Contains(errStr, "FILE_MIGRATE") ||
		strings.Contains(errStr, "USER_MIGRATE") ||
		strings.Contains(errStr, "STATS_MIGRATE") {
		return true
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
	delay := float64(r.config.BaseDelay) * math.Pow(ExponentialBackoffBase, float64(attempt))

	// Apply maximum delay limit
	if delay > float64(r.config.MaxDelay) {
		delay = float64(r.config.MaxDelay)
	}

	// Add jitter to avoid thundering herd
	if r.config.JitterFactor > 0 {
		// Use crypto/rand for secure random number generation
		randomBig, err := rand.Int(rand.Reader, big.NewInt(RandomPrecision))
		if err != nil {
			// Fallback to no jitter if crypto/rand fails
			return time.Duration(delay)
		}
		randomFloat := float64(randomBig.Int64()) / RandomPrecision                             // Convert to 0.0-1.0 range
		jitter := delay * r.config.JitterFactor * (randomFloat*JitterMultiplier - JitterOffset) // Random between -jitterFactor and +jitterFactor
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
