package workflow

import (
	"context"
	"errors"
	"fmt"
	"math"
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
}

// NewRetryableAction creates a new retryable action.
// action: the action to retry
// maxRetries: maximum number of retry attempts
// delay: time to wait between retries
func NewRetryableAction(action Action, maxRetries int, delay time.Duration) *RetryableAction {
	return &RetryableAction{
		action:     action,
		maxRetries: maxRetries,
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

// WithRetryIf sets a predicate for which errors to retry
func (r *RetryableAction) WithRetryIf(predicate func(error) bool) *RetryableAction {
	r.retryIf = predicate
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

		lastErr = err

		// Should we retry?
		if attempt == r.maxRetries || !r.retryIf(err) {
			break
		}

		// Log retry (in real implementation, you'd want better logging)
		fmt.Printf("Attempt %d failed: %v. Retrying...\n", attempt+1, err)

		// Wait before retry with exponential backoff
		backoffMultiplier := math.Pow(r.backoff, float64(attempt))
		delay := time.Duration(float64(r.delay) * backoffMultiplier)

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
