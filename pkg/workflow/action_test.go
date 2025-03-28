package workflow

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestActionFunc(t *testing.T) {
	// Create a simple action function
	called := false
	actionFn := ActionFunc(func(ctx context.Context, data *WorkflowData) error {
		called = true
		return nil
	})

	// Execute the action
	data := NewWorkflowData("test")
	err := actionFn.Execute(context.Background(), data)

	// Check results
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if !called {
		t.Error("Action function was not called")
	}
}

func TestNewCompositeAction(t *testing.T) {
	// Create some test actions
	action1 := ActionFunc(func(ctx context.Context, data *WorkflowData) error {
		return nil
	})

	action2 := ActionFunc(func(ctx context.Context, data *WorkflowData) error {
		return nil
	})

	// Create a composite action
	composite := NewCompositeAction(action1, action2)

	// Check that the actions were added
	if len(composite.actions) != 2 {
		t.Errorf("Expected 2 actions, got %d", len(composite.actions))
	}
}

func TestCompositeActionExecute(t *testing.T) {
	// Create some test actions
	executionOrder := []string{}

	action1 := ActionFunc(func(ctx context.Context, data *WorkflowData) error {
		executionOrder = append(executionOrder, "action1")
		return nil
	})

	action2 := ActionFunc(func(ctx context.Context, data *WorkflowData) error {
		executionOrder = append(executionOrder, "action2")
		return nil
	})

	// Create a composite action
	composite := NewCompositeAction(action1, action2)

	// Execute the composite action
	data := NewWorkflowData("test")
	err := composite.Execute(context.Background(), data)

	// Check results
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}

	// Check execution order
	if len(executionOrder) != 2 {
		t.Errorf("Expected 2 actions to be executed, got %d", len(executionOrder))
	}
	if executionOrder[0] != "action1" || executionOrder[1] != "action2" {
		t.Errorf("Actions executed in wrong order: %v", executionOrder)
	}
}

func TestCompositeActionExecuteWithError(t *testing.T) {
	// Create some test actions
	executionOrder := []string{}

	action1 := ActionFunc(func(ctx context.Context, data *WorkflowData) error {
		executionOrder = append(executionOrder, "action1")
		return nil
	})

	expectedErr := errors.New("action2 error")
	action2 := ActionFunc(func(ctx context.Context, data *WorkflowData) error {
		executionOrder = append(executionOrder, "action2")
		return expectedErr
	})

	action3 := ActionFunc(func(ctx context.Context, data *WorkflowData) error {
		executionOrder = append(executionOrder, "action3")
		return nil
	})

	// Create a composite action
	composite := NewCompositeAction(action1, action2, action3)

	// Execute the composite action
	data := NewWorkflowData("test")
	err := composite.Execute(context.Background(), data)

	// Check results
	if err == nil {
		t.Error("Expected an error, got nil")
	}

	// Check execution order - action3 should not have been executed
	if len(executionOrder) != 2 {
		t.Errorf("Expected 2 actions to be executed, got %d", len(executionOrder))
	}
	if executionOrder[0] != "action1" || executionOrder[1] != "action2" {
		t.Errorf("Actions executed in wrong order: %v", executionOrder)
	}
}

func TestCompositeActionAdd(t *testing.T) {
	// Create some test actions
	action1 := ActionFunc(func(ctx context.Context, data *WorkflowData) error {
		return nil
	})

	action2 := ActionFunc(func(ctx context.Context, data *WorkflowData) error {
		return nil
	})

	// Create a composite action with one action
	composite := NewCompositeAction(action1)

	// Add another action
	composite.Add(action2)

	// Check that both actions were added
	if len(composite.actions) != 2 {
		t.Errorf("Expected 2 actions, got %d", len(composite.actions))
	}

	// Test method chaining
	action3 := ActionFunc(func(ctx context.Context, data *WorkflowData) error {
		return nil
	})

	composite.Add(action3)

	// Check that all actions were added
	if len(composite.actions) != 3 {
		t.Errorf("Expected 3 actions, got %d", len(composite.actions))
	}
}

func TestNewValidationAction(t *testing.T) {
	// Create a validation function
	validationFn := func(value interface{}) error {
		str, ok := value.(string)
		if !ok {
			return errors.New("not a string")
		}
		if len(str) == 0 {
			return errors.New("empty string")
		}
		return nil
	}

	// Create a validation action
	action := NewValidationAction("input", validationFn, "valid", "error")

	// Check that the action was created correctly
	if action.inputKey != "input" {
		t.Errorf("Expected inputKey to be 'input', got %q", action.inputKey)
	}
	if action.outputKey != "valid" {
		t.Errorf("Expected outputKey to be 'valid', got %q", action.outputKey)
	}
	if action.errorOutputKey != "error" {
		t.Errorf("Expected errorOutputKey to be 'error', got %q", action.errorOutputKey)
	}
}

func TestValidationActionExecute(t *testing.T) {
	// Create workflow data
	data := NewWorkflowData("test")

	// Define validation function
	validateNonEmpty := func(input interface{}) error {
		str, ok := input.(string)
		if !ok {
			return fmt.Errorf("input must be a string")
		}
		if str == "" {
			return fmt.Errorf("input cannot be empty")
		}
		return nil
	}

	// Create validation action
	action := NewValidationAction("test_input", validateNonEmpty, "valid", "error")

	// Test valid input
	data.Set("test_input", "valid")
	err := action.Execute(context.Background(), data)
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	valid, ok := data.Get("valid")
	if !ok || !valid.(bool) {
		t.Error("Expected valid to be true")
	}

	// Test invalid input (empty string)
	data.Set("test_input", "")
	err = action.Execute(context.Background(), data)
	if err == nil {
		t.Error("Expected an error")
	}
	errMsg, ok := data.Get("error")
	if !ok || errMsg.(string) != "input cannot be empty" {
		t.Errorf("Expected error message 'input cannot be empty', got %v", errMsg)
	}

	// Test missing input
	data.Delete("test_input")
	err = action.Execute(context.Background(), data)
	if err == nil {
		t.Error("Expected an error")
	}
	errMsg, ok = data.Get("error")
	if !ok || errMsg.(string) != "validation failed: input key test_input not found" {
		t.Errorf("Expected error message 'validation failed: input key test_input not found', got %v", errMsg)
	}
}

func TestNewMapAction(t *testing.T) {
	// Create a mapping function
	mapFn := func(value interface{}) (interface{}, error) {
		str, ok := value.(string)
		if !ok {
			return nil, errors.New("not a string")
		}
		return len(str), nil
	}

	// Create a map action
	action := NewMapAction("input", "output", mapFn)

	// Check that the action was created correctly
	if action.inputKey != "input" {
		t.Errorf("Expected inputKey to be 'input', got %q", action.inputKey)
	}
	if action.outputKey != "output" {
		t.Errorf("Expected outputKey to be 'output', got %q", action.outputKey)
	}
}

func TestMapActionExecute(t *testing.T) {
	// Create a mapping function
	mapFn := func(value interface{}) (interface{}, error) {
		str, ok := value.(string)
		if !ok {
			return nil, errors.New("not a string")
		}
		return len(str), nil
	}

	// Create a map action
	action := NewMapAction("input", "output", mapFn)

	// Test with valid input
	data := NewWorkflowData("test")
	data.Set("input", "hello")

	err := action.Execute(context.Background(), data)

	// Check results
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}

	output, ok := data.Get("output")
	if !ok {
		t.Error("Expected 'output' key to be set")
	}
	if output != 5 {
		t.Errorf("Expected output to be 5, got %v", output)
	}

	// Test with invalid input
	data = NewWorkflowData("test")
	data.Set("input", 123) // Not a string

	err = action.Execute(context.Background(), data)

	// Check results
	if err == nil {
		t.Error("Expected an error, got nil")
	}

	// Test with missing input
	data = NewWorkflowData("test")

	err = action.Execute(context.Background(), data)

	// Check results
	if err == nil {
		t.Error("Expected an error, got nil")
	}
}

func TestNewRetryableAction(t *testing.T) {
	// Create a test action
	innerAction := ActionFunc(func(ctx context.Context, data *WorkflowData) error {
		return nil
	})

	// Create a retryable action
	maxRetries := 3
	delay := 10 * time.Millisecond
	action := NewRetryableAction(innerAction, maxRetries, delay)

	// Check that the action was created correctly
	if action.action == nil {
		t.Error("Inner action not set")
	}
	if action.maxRetries != maxRetries {
		t.Errorf("Expected maxRetries to be %d, got %d", maxRetries, action.maxRetries)
	}
	if action.delay != delay {
		t.Errorf("Expected delay to be %v, got %v", delay, action.delay)
	}
	if action.backoff != 2.0 {
		t.Errorf("Expected default backoff to be 2.0, got %f", action.backoff)
	}
}

func TestRetryableActionWithBackoff(t *testing.T) {
	// Create a test action
	innerAction := ActionFunc(func(ctx context.Context, data *WorkflowData) error {
		return nil
	})

	// Create a retryable action
	action := NewRetryableAction(innerAction, 3, 10*time.Millisecond)

	// Set custom backoff
	backoff := 1.5
	returnedAction := action.WithBackoff(backoff)

	// Check that the backoff was set correctly
	if action.backoff != backoff {
		t.Errorf("Expected backoff to be %f, got %f", backoff, action.backoff)
	}

	// Check that the method returns the action for chaining
	if returnedAction != action {
		t.Error("WithBackoff did not return the action for chaining")
	}
}

func TestRetryableActionWithRetryIf(t *testing.T) {
	// Create a test action
	innerAction := ActionFunc(func(ctx context.Context, data *WorkflowData) error {
		return nil
	})

	// Create a retryable action
	action := NewRetryableAction(innerAction, 3, 10*time.Millisecond)

	// Set custom retry predicate
	predicate := func(err error) bool {
		return err != nil && err.Error() == "retryable error"
	}
	returnedAction := action.WithRetryIf(predicate)

	// Check that the predicate was set correctly
	if action.retryIf == nil {
		t.Error("RetryIf predicate not set")
	}

	// Check that the method returns the action for chaining
	if returnedAction != action {
		t.Error("WithRetryIf did not return the action for chaining")
	}
}

func TestRetryableActionExecute(t *testing.T) {
	// Create a test action that fails a few times then succeeds
	attempts := 0
	innerAction := ActionFunc(func(ctx context.Context, data *WorkflowData) error {
		attempts++
		if attempts < 3 {
			return errors.New("temporary error")
		}
		return nil
	})

	// Create a retryable action
	action := NewRetryableAction(innerAction, 3, 10*time.Millisecond)

	// Execute the action
	data := NewWorkflowData("test")
	err := action.Execute(context.Background(), data)

	// Check results
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if attempts != 3 {
		t.Errorf("Expected 3 attempts, got %d", attempts)
	}

	// Test with an action that always fails
	attempts = 0
	alwaysFailAction := ActionFunc(func(ctx context.Context, data *WorkflowData) error {
		attempts++
		return errors.New("persistent error")
	})

	// Create a retryable action with fewer retries
	action = NewRetryableAction(alwaysFailAction, 2, 10*time.Millisecond)

	// Execute the action
	data = NewWorkflowData("test")
	err = action.Execute(context.Background(), data)

	// Check results
	if err == nil {
		t.Error("Expected an error, got nil")
	}
	if attempts != 3 { // Initial attempt + 2 retries
		t.Errorf("Expected 3 attempts, got %d", attempts)
	}

	// Test with a selective retry predicate
	attempts = 0
	mixedErrorAction := ActionFunc(func(ctx context.Context, data *WorkflowData) error {
		attempts++
		if attempts == 1 {
			return errors.New("retryable error")
		}
		return errors.New("non-retryable error")
	})

	// Create a retryable action with a selective predicate
	action = NewRetryableAction(mixedErrorAction, 3, 10*time.Millisecond)
	action.WithRetryIf(func(err error) bool {
		return err != nil && err.Error() == "retryable error"
	})

	// Execute the action
	data = NewWorkflowData("test")
	err = action.Execute(context.Background(), data)

	// Check results
	if err == nil {
		t.Error("Expected an error, got nil")
	}
	if attempts != 2 { // Should stop after the non-retryable error
		t.Errorf("Expected 2 attempts, got %d", attempts)
	}
}
