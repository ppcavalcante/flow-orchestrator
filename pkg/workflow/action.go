package workflow

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"time"
)

// Action represents an executable unit of work within a workflow.
// Implementations of this interface can be attached to nodes and will be
// executed when the node is processed during workflow execution.
type Action interface {
	// Execute performs the action with the given workflow data.
	// The context can be used for cancellation and timeouts.
	// Any data produced by the action should be stored in the WorkflowData.
	Execute(ctx context.Context, data *WorkflowData) error
}

// ActionFunc is a function type that implements the Action interface.
// It allows using simple functions as actions without creating a custom type.
type ActionFunc func(ctx context.Context, data *WorkflowData) error

// Execute calls the function, satisfying the Action interface.
func (f ActionFunc) Execute(ctx context.Context, data *WorkflowData) error {
	return f(ctx, data)
}

// CompositeAction combines multiple actions into a single action that
// executes them sequentially.
type CompositeAction struct {
	actions []Action
}

// NewCompositeAction creates a new composite action from the provided actions.
// The actions will be executed in the order they are provided.
func NewCompositeAction(actions ...Action) *CompositeAction {
	return &CompositeAction{
		actions: actions,
	}
}

// Execute runs all actions in the composite sequentially.
// If any action returns an error, execution stops and the error is returned.
func (c *CompositeAction) Execute(ctx context.Context, data *WorkflowData) error {
	for i, action := range c.actions {
		if err := action.Execute(ctx, data); err != nil {
			return fmt.Errorf("action %d failed: %w", i, err)
		}
	}
	return nil
}

// Add appends additional actions to this composite action.
// Returns the composite action for method chaining.
func (c *CompositeAction) Add(actions ...Action) *CompositeAction {
	c.actions = append(c.actions, actions...)
	return c
}

// ValidationAction validates input data before proceeding.
// It can be used to ensure required data is present and valid.
type ValidationAction struct {
	inputKey       string
	validationFn   func(interface{}) error
	outputKey      string
	errorOutputKey string
}

// NewValidationAction creates a new validation action.
// inputKey: the key to validate in the workflow data
// validationFn: function that performs the validation
// outputKey: where to store the validation result (if successful)
// errorOutputKey: where to store error information (if validation fails)
func NewValidationAction(inputKey string, validationFn func(interface{}) error, outputKey, errorOutputKey string) *ValidationAction {
	return &ValidationAction{
		inputKey:       inputKey,
		validationFn:   validationFn,
		outputKey:      outputKey,
		errorOutputKey: errorOutputKey,
	}
}

// Execute performs validation
func (v *ValidationAction) Execute(ctx context.Context, data *WorkflowData) error {
	// Check if context is cancelled
	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Get input data
	input, ok := data.Get(v.inputKey)
	if !ok {
		err := fmt.Errorf("validation failed: input key %s not found", v.inputKey)
		if v.errorOutputKey != "" {
			data.Set(v.errorOutputKey, err.Error())
		}
		return err
	}

	// Perform validation
	err := v.validationFn(input)

	// Store results
	if v.outputKey != "" {
		data.Set(v.outputKey, err == nil)
	}

	if err != nil && v.errorOutputKey != "" {
		data.Set(v.errorOutputKey, err.Error())
	}

	return err
}

// MapAction transforms data from one format to another.
// It applies a mapping function to input data and stores the result.
type MapAction struct {
	inputKey  string
	outputKey string
	mapFn     func(interface{}) (interface{}, error)
}

// NewMapAction creates a new map action that transforms data.
// inputKey: the key of the input data
// outputKey: where to store the transformed data
// mapFn: function that performs the transformation
func NewMapAction(inputKey, outputKey string, mapFn func(interface{}) (interface{}, error)) *MapAction {
	return &MapAction{
		inputKey:  inputKey,
		outputKey: outputKey,
		mapFn:     mapFn,
	}
}

// Execute performs the mapping
func (m *MapAction) Execute(ctx context.Context, data *WorkflowData) error {
	// Check if context is cancelled
	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Get input data
	input, ok := data.Get(m.inputKey)
	if !ok {
		return fmt.Errorf("map action failed: input key %s not found", m.inputKey)
	}

	// Perform mapping
	output, err := m.mapFn(input)
	if err != nil {
		return fmt.Errorf("map action failed: %w", err)
	}

	// Store result
	data.Set(m.outputKey, output)

	return nil
}

// RetryableAction adds retry capability to an action.
// It will retry the wrapped action according to the configured parameters.
type RetryableAction struct {
	action     Action
	maxRetries int
	delay      time.Duration
	backoff    float64
	retryIf    func(error) bool
	maxDelay   time.Duration // backoff cap; 0 (default) = uncapped (pre-existing behavior)
	jitter     float64       // 0..1 fraction of the delay to randomize; 0 (default) = no jitter (pre-existing behavior)
}

// NewRetryableAction creates a new retryable action.
// action: the action to retry
// maxRetries: maximum number of retry attempts; NEGATIVE VALUES ARE CLAMPED TO ZERO,
// so the wrapped action always runs at least once (see below)
// delay: time to wait between retries
//
// F117-T6-01 — WHY maxRetries IS CLAMPED, and it is a correctness fix rather than
// input tidying. Execute's loop is `for attempt := 0; attempt <= r.maxRetries` and the
// function ends `return lastErr`. A negative maxRetries makes the loop body never run,
// so lastErr stays nil and Execute RETURNS SUCCESS HAVING NEVER INVOKED THE ACTION.
// Measured from outside the package before the clamp: maxRetries=0 -> 1 invocation,
// -1 -> 0 invocations, -7 -> 0 invocations, err=<nil> in all three.
//
// In a durable-execution engine "reported success without acting" is the worst
// available failure: the result is journaled, so the lie is persisted. This is blocker
// 117-F1's exact shape one constructor over — there it was a compensation stamped
// Compensated without running, here it is any wrapped action.
//
// Clamped at the BOUNDARY, not at the loop, because this constructor is EXPORTED and
// maxRetries is caller-supplied: the invalid state is made unrepresentable rather than
// tolerated at the reader. That is this phase's thesis. Both in-package callers already
// guarded (node.go behind `retryCount > 0`, fanout.go's policy behind `count <= 0`), so
// no production path reached this — the exposure was consumer-facing.
//
// Guarded by TestRetryableAction_NegativeMaxRetriesStillInvokesAction, which asserts the
// INVOCATION COUNT. Note the returned error is nil on BOTH sides of the defect, so an
// assertion on it is vacuous — the same trap as 117-F1, where the node status was
// Compensated on both sides.
func NewRetryableAction(action Action, maxRetries int, delay time.Duration) *RetryableAction {
	return &RetryableAction{
		action:     action,
		maxRetries: max(0, maxRetries),
		delay:      delay,
		backoff:    2.0,                              // Default exponential backoff
		retryIf:    func(error) bool { return true }, // Retry all errors by default
	}
}

// WithBackoff sets the backoff factor
func (r *RetryableAction) WithBackoff(factor float64) *RetryableAction {
	r.backoff = factor
	return r
}

// WithRetryIf sets a predicate for which errors to retry. This IS the non-retryable
// classification: a predicate returning false for an error makes that error terminal —
// exactly one attempt, no backoff loop (see Execute :226). Use it to mark permanent
// failures (bad input, auth, not-found) as non-retryable while transient errors retry.
func (r *RetryableAction) WithRetryIf(predicate func(error) bool) *RetryableAction {
	r.retryIf = predicate
	return r
}

// WithMaxDelay caps the exponential backoff delay. Once the computed delay reaches d it
// stops growing (bounds a retry storm's per-attempt wait). d <= 0 (default) leaves the
// backoff uncapped — byte-for-byte the pre-existing behavior for callers that never set it.
func (r *RetryableAction) WithMaxDelay(d time.Duration) *RetryableAction {
	r.maxDelay = d
	return r
}

// WithJitter randomizes each backoff delay by up to fraction f (0..1) of its value, so
// concurrent retriers de-correlate rather than thundering-herd. f is clamped to [0,1];
// f == 0 (default) applies no jitter — byte-for-byte the pre-existing behavior. The
// randomness is wall-clock timing ONLY: the retry OUTCOME is journaled via the result,
// never the sleep duration, so this does not touch the determinism moat.
func (r *RetryableAction) WithJitter(f float64) *RetryableAction {
	if f < 0 {
		f = 0
	}
	if f > 1 {
		f = 1
	}
	r.jitter = f
	return r
}

// Execute runs the action with retries
func (r *RetryableAction) Execute(ctx context.Context, data *WorkflowData) error {
	var lastErr error

	for attempt := 0; attempt <= r.maxRetries; attempt++ {
		// Check if context is cancelled
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// Execute the action
		err := r.action.Execute(ctx, data)
		if err == nil {
			return nil // Success
		}

		// A park is not a retryable failure. If the wrapped action suspended,
		// return the sentinel immediately — retrying would re-run the action and
		// could re-park in a loop, and a park is a SUCCESS arm, not an error to
		// recover from. (Declared suspension nodes bypass this wrapper entirely via
		// node.Execute; this is defense-in-depth for a hand-wrapped action.)
		if errors.Is(err, ErrSuspended) {
			return err
		}

		lastErr = err

		// Should we retry?
		if attempt == r.maxRetries || !r.retryIf(err) {
			break
		}

		// Wait before retry with exponential backoff
		backoffMultiplier := math.Pow(r.backoff, float64(attempt))
		delay := time.Duration(float64(r.delay) * backoffMultiplier)

		// Cap the backoff (maxDelay==0 → uncapped, the pre-existing path).
		if r.maxDelay > 0 && delay > r.maxDelay {
			delay = r.maxDelay
		}
		// Apply jitter (jitter==0 → unchanged, the pre-existing path). Scale into
		// [delay*(1-jitter), delay] so jitter only ever SHORTENS — never exceeds the cap
		// above. math/rand/v2 is wall-clock timing only; the retry outcome is journaled
		// via the result, not the sleep, so the determinism moat is untouched.
		if r.jitter > 0 {
			// nolint:gosec // G404: jitter is non-cryptographic backoff timing, not a security value.
			delay = time.Duration(float64(delay) * (1 - r.jitter + r.jitter*rand.Float64()))
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
			// Continue to next attempt
		}
	}

	return lastErr
}

// Predefined error types for better error handling
var (
	ErrInputNotFound   = errors.New("input not found")
	ErrInvalidInput    = errors.New("invalid input")
	ErrExecutionFailed = errors.New("execution failed")
)
