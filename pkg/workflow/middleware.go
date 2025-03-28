// Package workflow provides a workflow engine following mathematical principles
package workflow

import (
	"context"
	"crypto/rand"
	"fmt"
	"log"
	"time"
)

// Middleware is a function that wraps an Action with additional behavior.
// It can be used to add cross-cutting concerns like logging, retries, and timeouts.
type Middleware func(Action) Action

// MiddlewareStack manages a collection of middleware and applies them to actions.
type MiddlewareStack struct {
	middlewares []Middleware
}

// NewMiddlewareStack creates a new middleware stack.
func NewMiddlewareStack() *MiddlewareStack {
	return &MiddlewareStack{
		middlewares: make([]Middleware, 0),
	}
}

// Use adds a middleware to the stack.
// Returns the stack for method chaining.
func (s *MiddlewareStack) Use(m Middleware) *MiddlewareStack {
	s.middlewares = append(s.middlewares, m)
	return s
}

// Apply applies all middleware in the stack to the given action.
// Middleware is applied in the order it was added to the stack.
func (s *MiddlewareStack) Apply(action Action) Action {
	result := action
	// Apply in reverse order (last middleware added is executed first)
	for i := len(s.middlewares) - 1; i >= 0; i-- {
		result = s.middlewares[i](result)
	}
	return result
}

// LoggingMiddleware creates middleware that logs the execution of actions.
// It logs when an action starts and completes, including any errors.
func LoggingMiddleware() Middleware {
	return func(next Action) Action {
		return ActionFunc(func(ctx context.Context, data *WorkflowData) error {
			log.Printf("Starting action")
			startTime := time.Now()

			// Execute the wrapped action
			err := next.Execute(ctx, data)

			duration := time.Since(startTime)
			if err != nil {
				log.Printf("Action failed after %v: %v", duration, err)
			} else {
				log.Printf("Action completed in %v", duration)
			}

			return err
		})
	}
}

// TimeoutMiddleware creates middleware that adds a timeout to actions.
// If the action doesn't complete within the timeout, it is cancelled.
func TimeoutMiddleware(timeout time.Duration) Middleware {
	return func(next Action) Action {
		return ActionFunc(func(ctx context.Context, data *WorkflowData) error {
			// Create a timeout context
			timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()

			// Use channels to handle timeout
			resultChan := make(chan error, 1)

			// Execute in a goroutine
			go func() {
				resultChan <- next.Execute(timeoutCtx, data)
			}()

			// Wait for result or timeout
			select {
			case err := <-resultChan:
				return err
			case <-timeoutCtx.Done():
				if ctx.Err() == context.DeadlineExceeded {
					return fmt.Errorf("action timed out after %v: %w", timeout, ctx.Err())
				}
				return ctx.Err()
			}
		})
	}
}

// RetryMiddleware creates middleware that retries actions on failure.
// It will retry up to maxRetries times with the specified backoff between attempts.
func RetryMiddleware(maxRetries int, backoff time.Duration) Middleware {
	return func(next Action) Action {
		return ActionFunc(func(ctx context.Context, data *WorkflowData) error {
			var lastErr error
			for attempt := 0; attempt <= maxRetries; attempt++ {
				// First attempt isn't a retry
				if attempt > 0 {
					// Log the retry attempt (commented out for production)
					// log.Printf("Attempt %d failed: %v", attempt, lastErr)

					// Apply backoff with some jitter
					var jitterBytes [4]byte
					if _, err := rand.Read(jitterBytes[:]); err != nil {
						// If we can't get random bytes, just use a simple calculation
						jitterInt := int64(attempt * 7919) // Use a prime number
						jitter := time.Duration(jitterInt % (int64(backoff) / 4))
						select {
						case <-time.After(backoff + jitter):
						case <-ctx.Done():
							return fmt.Errorf("retry aborted: %w", ctx.Err())
						}
					} else {
						jitterInt := int64(jitterBytes[0]) | int64(jitterBytes[1])<<8 | int64(jitterBytes[2])<<16 | int64(jitterBytes[3])<<24
						jitter := time.Duration(jitterInt % (int64(backoff) / 4))
						select {
						case <-time.After(backoff + jitter):
						case <-ctx.Done():
							return fmt.Errorf("retry aborted: %w", ctx.Err())
						}
					}
				}

				err := next.Execute(ctx, data)
				if err == nil {
					return nil // Success
				}

				lastErr = err
			}

			return fmt.Errorf("max retries (%d) exceeded: %w", maxRetries, lastErr)
		})
	}
}

// NoDelayRetryMiddleware creates middleware that retries the action immediately on failure
// This is useful for benchmarking and testing where we don't want to wait for backoff delays
// Setting verbose to false will disable retry logging, which is useful for benchmarks
func NoDelayRetryMiddleware(maxRetries int, verbose ...bool) Middleware {
	isVerbose := false
	if len(verbose) > 0 {
		isVerbose = verbose[0]
	}

	return func(next Action) Action {
		return ActionFunc(func(ctx context.Context, data *WorkflowData) error {
			var lastErr error

			// If maxRetries is 0, just run once with no retries
			if maxRetries <= 0 {
				return next.Execute(ctx, data)
			}

			// Try initial attempt plus up to maxRetries retries
			for attempt := 0; attempt <= maxRetries; attempt++ {
				// Check for context cancellation
				if ctx.Err() != nil {
					return fmt.Errorf("retry aborted: %w", ctx.Err())
				}

				// Execute the action
				err := next.Execute(ctx, data)

				// If successful, return immediately
				if err == nil {
					return nil
				}

				lastErr = err

				// Only log if verbose and we're going to retry
				if isVerbose && attempt < maxRetries {
					log.Printf("Fast retry attempt %d failed: %v", attempt+1, lastErr)
				}
			}

			return fmt.Errorf("max retries (%d) exceeded: %w", maxRetries, lastErr)
		})
	}
}

// MetricsMiddleware creates middleware that collects metrics about action execution.
// It records execution time and success/failure counts.
func MetricsMiddleware() Middleware {
	return func(next Action) Action {
		return ActionFunc(func(ctx context.Context, data *WorkflowData) error {
			startTime := time.Now()

			// Execute the wrapped action
			err := next.Execute(ctx, data)

			// Record metrics (in a real implementation, this would use a metrics system)
			duration := time.Since(startTime)
			_ = duration // Use metrics system instead

			return err
		})
	}
}

// ValidationMiddleware creates middleware that validates workflow data.
// It runs the validator function before executing the action.
func ValidationMiddleware(validator func(*WorkflowData) error) Middleware {
	return func(next Action) Action {
		return ActionFunc(func(ctx context.Context, data *WorkflowData) error {
			// Execute the wrapped action
			err := next.Execute(ctx, data)
			if err != nil {
				return err
			}

			// Validate the result
			if err := validator(data); err != nil {
				return fmt.Errorf("validation failed: %w", err)
			}

			return nil
		})
	}
}

// ConditionalRetryMiddleware creates middleware that retries based on a predicate
func ConditionalRetryMiddleware(maxRetries int, backoff time.Duration, predicate func(error) bool) Middleware {
	return func(next Action) Action {
		return ActionFunc(func(ctx context.Context, data *WorkflowData) error {
			var lastErr error

			for attempt := 0; attempt <= maxRetries; attempt++ {
				// Skip first backoff
				if attempt > 0 && backoff > 0 {
					// Wait with exponential backoff
					select {
					case <-time.After(backoff * time.Duration(attempt)):
						// Continue to next attempt
					case <-ctx.Done():
						return ctx.Err()
					}
				}

				// Execute the action
				err := next.Execute(ctx, data)
				if err == nil {
					return nil // Success!
				}

				lastErr = err

				// Check if we should retry this error
				if !predicate(err) {
					return err
				}

				// log.Printf("Attempt %d failed: %v", attempt+1, err)
			}

			return fmt.Errorf("all %d retries failed: %w", maxRetries, lastErr)
		})
	}
}
