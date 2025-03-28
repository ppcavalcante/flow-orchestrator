# Middleware System

## Package Organization

The Middleware System is part of the public API in the `pkg/workflow` package. All middleware types and functions are accessible through this package:

```go
import "github.com/yourusername/flow-orchestrator/pkg/workflow"
```

## Overview

The middleware system provides a way to add cross-cutting concerns like logging, retries, timeouts, and metrics to workflow actions.

## Understanding the Middleware Pattern

### What is Middleware?

In Flow Orchestrator, middleware is a function that wraps an `Action` to add behavior before and/or after its execution. Middleware follows a functional composition pattern that enables:

1. **Separation of concerns** - Keep core action logic separate from cross-cutting concerns
2. **Reusability** - Apply the same middleware across different actions
3. **Composability** - Stack multiple middleware in any order
4. **Non-invasive enhancement** - Add functionality without modifying existing code

### Middleware Architecture

The middleware pattern in Flow Orchestrator uses a three-layer functional approach:

```go
func SomeMiddleware(config ...interface{}) workflow.Middleware {
    // Layer 1: Configuration layer - returns a configured middleware
    return func(next workflow.Action) workflow.Action {
        // Layer 2: Wrapping layer - takes an action and returns a wrapped action
        return workflow.ActionFunc(func(ctx context.Context, data *workflow.WorkflowData) error {
            // Layer 3: Execution layer - the actual execution logic
            
            // Pre-execution logic
            // ...
            
            // Execute the wrapped action
            err := next.Execute(ctx, data)
            
            // Post-execution logic
            // ...
            
            return err
        })
    }
}
```

This structure provides several benefits:
- The outer function can accept configuration parameters
- The middle function enables composition of multiple middleware
- The inner function maintains the same signature as the original action

### Why This Design?

This middleware pattern was chosen for Flow Orchestrator for several reasons:

1. **Functional Composition** - Allows clean chaining of behaviors
2. **Type Safety** - Maintains strong typing throughout the middleware chain
3. **Performance** - Direct function calls without reflection or dynamic dispatch
4. **Flexibility** - Middleware can be applied selectively to specific actions
5. **Testability** - Each middleware can be tested in isolation

## Core Components

### Middleware Function Signature

```go
type Middleware func(Action) Action
```

A middleware is a function that takes an `Action` and returns a new `Action` with enhanced behavior.

### MiddlewareStack

The `MiddlewareStack` provides a way to group and apply multiple middleware:

```go
stack := workflow.NewMiddlewareStack()
stack.Use(workflow.LoggingMiddleware())
stack.Use(workflow.RetryMiddleware(3, time.Second))

// Apply all middleware to an action
wrappedAction := stack.Apply(myAction)
```

## Built-in Middleware

Flow Orchestrator includes several built-in middleware components for common needs:

### Logging Middleware

Adds logging before and after action execution.

```go
func LoggingMiddleware() Middleware {
    return func(next Action) Action {
        return ActionFunc(func(ctx context.Context, data *WorkflowData) error {
            nodeName := "unknown"
            if name, ok := ctx.Value("node_name").(string); ok {
                nodeName = name
            }
            
            log.Printf("Starting execution of node: %s", nodeName)
            start := time.Now()
            
            err := next.Execute(ctx, data)
            
            duration := time.Since(start)
            if err != nil {
                log.Printf("Node %s failed after %v: %v", nodeName, duration, err)
            } else {
                log.Printf("Node %s completed successfully in %v", nodeName, duration)
            }
            
            return err
        })
    }
}
```

**Usage:**

```go
builder.AddNode("process-data")
    .WithAction(workflow.LoggingMiddleware()(processDataAction))
```

### Timeout Middleware

Enforces a maximum execution time for an action.

```go
func TimeoutMiddleware(timeout time.Duration) Middleware {
    return func(next Action) Action {
        return ActionFunc(func(ctx context.Context, data *WorkflowData) error {
            ctx, cancel := context.WithTimeout(ctx, timeout)
            defer cancel()
            
            done := make(chan error, 1)
            go func() {
                done <- next.Execute(ctx, data)
            }()
            
            select {
            case err := <-done:
                return err
            case <-ctx.Done():
                if ctx.Err() == context.DeadlineExceeded {
                    return fmt.Errorf("action timed out after %v", timeout)
                }
                return ctx.Err()
            }
        })
    }
}
```

**Usage:**

```go
builder.AddNode("api-call")
    .WithAction(workflow.TimeoutMiddleware(5 * time.Second)(apiCallAction))
```

### Retry Middleware

Automatically retries failed actions with configurable backoff.

```go
func RetryMiddleware(maxRetries int, backoff time.Duration) Middleware {
    return func(next Action) Action {
        return ActionFunc(func(ctx context.Context, data *WorkflowData) error {
            var lastErr error
            
            for attempt := 0; attempt <= maxRetries; attempt++ {
                if attempt > 0 {
                    log.Printf("Retry attempt %d/%d after error: %v", 
                        attempt, maxRetries, lastErr)
                    time.Sleep(backoff * time.Duration(attempt))
                }
                
                err := next.Execute(ctx, data)
                if err == nil {
                    return nil // Success
                }
                
                lastErr = err
                
                // Check if context was canceled
                if ctx.Err() != nil {
                    return ctx.Err()
                }
            }
            
            return fmt.Errorf("action failed after %d retries: %w", 
                maxRetries, lastErr)
        })
    }
}
```

**Usage:**

```go
builder.AddNode("flaky-service")
    .WithAction(workflow.RetryMiddleware(3, 1 * time.Second)(callServiceAction))
```

### Metrics Middleware

Collects execution metrics for monitoring and performance analysis.

```go
func MetricsMiddleware() Middleware {
    return func(next Action) Action {
        return ActionFunc(func(ctx context.Context, data *WorkflowData) error {
            nodeName := "unknown"
            if name, ok := ctx.Value("node_name").(string); ok {
                nodeName = name
            }
            
            start := time.Now()
            
            err := next.Execute(ctx, data)
            
            duration := time.Since(start)
            metrics.RecordNodeExecution(nodeName, duration, err == nil)
            
            return err
        })
    }
}
```

**Usage:**

```go
builder.AddNode("process-data")
    .WithAction(workflow.MetricsMiddleware()(processDataAction))
```

### Validation Middleware

Validates workflow data before and/or after action execution.

```go
func ValidationMiddleware(validator func(*WorkflowData) error) Middleware {
    return func(next Action) Action {
        return ActionFunc(func(ctx context.Context, data *WorkflowData) error {
            if err := validator(data); err != nil {
                return fmt.Errorf("validation failed: %w", err)
            }
            
            return next.Execute(ctx, data)
        })
    }
}
```

**Usage:**

```go
validator := func(data *workflow.WorkflowData) error {
    value, exists := data.Get("user_id")
    if !exists {
        return errors.New("missing required user_id")
    }
    
    // Additional validation logic
    return nil
}

builder.AddNode("process-user")
    .WithAction(workflow.ValidationMiddleware(validator)(processUserAction))
```

### Conditional Retry Middleware

Retries actions only when specific error conditions are met.

```go
func ConditionalRetryMiddleware(maxRetries int, backoff time.Duration, 
                               predicate func(error) bool) Middleware {
    return func(next Action) Action {
        return ActionFunc(func(ctx context.Context, data *WorkflowData) error {
            var lastErr error
            
            for attempt := 0; attempt <= maxRetries; attempt++ {
                if attempt > 0 {
                    // Only sleep if we're going to retry
                    time.Sleep(backoff * time.Duration(attempt))
                }
                
                err := next.Execute(ctx, data)
                if err == nil {
                    return nil // Success
                }
                
                lastErr = err
                
                // Check if context was canceled
                if ctx.Err() != nil {
                    return ctx.Err()
                }
                
                // Check if we should retry this error
                if !predicate(err) {
                    return err // Don't retry this type of error
                }
            }
            
            return fmt.Errorf("action failed after %d retries: %w", 
                maxRetries, lastErr)
        })
    }
}
```

**Usage:**

```go
// Only retry network-related errors
shouldRetry := func(err error) bool {
    return strings.Contains(err.Error(), "connection") || 
           strings.Contains(err.Error(), "timeout")
}

builder.AddNode("external-api")
    .WithAction(workflow.ConditionalRetryMiddleware(3, 1 * time.Second, shouldRetry)(apiCallAction))
```

## Using Middleware

### Basic Usage

The simplest way to use middleware is to apply it directly to an action when defining a node:

```go
builder.AddNode("process-data")
    .WithAction(workflow.LoggingMiddleware()(processDataAction))
```

### Combining Multiple Middleware

Multiple middleware can be combined by nesting the function calls:

```go
builder.AddNode("api-call")
    .WithAction(
        workflow.LoggingMiddleware()(
            workflow.RetryMiddleware(3, time.Second)(
                workflow.TimeoutMiddleware(5 * time.Second)(
                    apiCallAction
                )
            )
        )
    )
```

The execution order is from outside to inside:
1. Logging middleware executes first (pre) and last (post)
2. Retry middleware executes second (pre) and second-to-last (post)
3. Timeout middleware executes third (pre) and third-to-last (post)
4. The original action executes in the middle

### Using MiddlewareStack

For cleaner code when applying multiple middleware, use a `MiddlewareStack`:

```go
// Create and configure a middleware stack
stack := workflow.NewMiddlewareStack()
stack.Use(workflow.LoggingMiddleware())
stack.Use(workflow.RetryMiddleware(3, time.Second))
stack.Use(workflow.TimeoutMiddleware(5 * time.Second))

// Apply the stack to an action
wrappedAction := stack.Apply(apiCallAction)

// Use in a node
builder.AddNode("api-call")
    .WithAction(wrappedAction)
```

### Order of Middleware Application

The order in which middleware is applied matters. Consider these principles:

1. **Outermost middleware** sees the entire execution, including other middleware
2. **Innermost middleware** is closest to the actual action
3. **Error handling middleware** (like retry) should usually be closer to the action than logging

Recommended order:
1. Logging/Metrics (outermost)
2. Timeout
3. Retry/Error handling
4. Validation (innermost)

## Creating Custom Middleware

### Basic Custom Middleware

Creating custom middleware follows the same pattern as the built-in middleware:

```go
func CustomMiddleware() workflow.Middleware {
    return func(next workflow.Action) workflow.Action {
        return workflow.ActionFunc(func(ctx context.Context, data *workflow.WorkflowData) error {
            // Pre-execution logic
            fmt.Println("Before action execution")
            
            // Execute the wrapped action
            err := next.Execute(ctx, data)
            
            // Post-execution logic
            fmt.Println("After action execution")
            
            return err
        })
    }
}
```

### Configurable Middleware

For middleware that needs configuration:

```go
func RateLimitMiddleware(rps int, burstSize int) workflow.Middleware {
    return func(next workflow.Action) workflow.Action {
        // Create a rate limiter with the specified parameters
        limiter := rate.NewLimiter(rate.Limit(rps), burstSize)
        
        return workflow.ActionFunc(func(ctx context.Context, data *workflow.WorkflowData) error {
            // Wait for rate limiter
            if err := limiter.Wait(ctx); err != nil {
                return err
            }
            
            // Execute the action
            return next.Execute(ctx, data)
        })
    }
}
```

### Stateful Middleware

For middleware that needs to maintain state:

```go
func CircuitBreakerMiddleware(failureThreshold int, resetTimeout time.Duration) workflow.Middleware {
    return func(next workflow.Action) workflow.Action {
        // State shared across all wrapped actions
        var failures int32
        var lastFailure atomic.Value
        lastFailure.Store(time.Time{})
        
        return workflow.ActionFunc(func(ctx context.Context, data *workflow.WorkflowData) error {
            // Check if circuit is open
            failCount := atomic.LoadInt32(&failures)
            lastFailTime := lastFailure.Load().(time.Time)
            
            if failCount >= int32(failureThreshold) {
                // Circuit is open, check if reset timeout has passed
                if time.Since(lastFailTime) < resetTimeout {
                    return fmt.Errorf("circuit breaker open: too many failures")
                }
                
                // Reset failure count after timeout
                atomic.StoreInt32(&failures, 0)
            }
            
            // Execute the action
            err := next.Execute(ctx, data)
            
            // Update circuit state
            if err != nil {
                atomic.AddInt32(&failures, 1)
                lastFailure.Store(time.Now())
            } else if failCount > 0 {
                // Success resets failure count
                atomic.StoreInt32(&failures, 0)
            }
            
            return err
        })
    }
}
```

## Advanced Middleware Patterns

### Middleware Manager

For more complex applications, you might want to organize middleware by phase:

```go
type MiddlewareManager struct {
    preMiddleware  []workflow.Middleware
    postMiddleware []workflow.Middleware
}

func NewMiddlewareManager() *MiddlewareManager {
    return &MiddlewareManager{
        preMiddleware:  make([]workflow.Middleware, 0),
        postMiddleware: make([]workflow.Middleware, 0),
    }
}

func (m *MiddlewareManager) Use(phase string, middleware workflow.Middleware) {
    switch phase {
    case "pre":
        m.preMiddleware = append(m.preMiddleware, middleware)
    case "post":
        m.postMiddleware = append(m.postMiddleware, middleware)
    }
}

func (m *MiddlewareManager) Apply(phase string, action workflow.Action) workflow.Action {
    var middlewares []workflow.Middleware
    
    switch phase {
    case "pre":
        middlewares = m.preMiddleware
    case "post":
        middlewares = m.postMiddleware
    default:
        return action
    }
    
    result := action
    for i := len(middlewares) - 1; i >= 0; i-- {
        result = middlewares[i](result)
    }
    
    return result
}
```

### Context-Aware Middleware

Middleware that uses context values for configuration:

```go
func ContextAwareMiddleware() workflow.Middleware {
    return func(next workflow.Action) workflow.Action {
        return workflow.ActionFunc(func(ctx context.Context, data *workflow.WorkflowData) error {
            // Extract values from context
            nodeName, _ := ctx.Value("node_name").(string)
            priority, _ := ctx.Value("priority").(int)
            
            // Use context values to customize behavior
            if priority > 5 {
                // High priority handling
            }
            
            return next.Execute(ctx, data)
        })
    }
}
```

### Conditional Middleware

Middleware that only applies under certain conditions:

```go
func ConditionalMiddleware(condition func(*workflow.WorkflowData) bool, 
                          middleware workflow.Middleware) workflow.Middleware {
    return func(next workflow.Action) workflow.Action {
        return workflow.ActionFunc(func(ctx context.Context, data *workflow.WorkflowData) error {
            if condition(data) {
                // Apply middleware if condition is met
                return middleware(next).Execute(ctx, data)
            }
            
            // Otherwise, execute the action directly
            return next.Execute(ctx, data)
        })
    }
}
```

## Best Practices

### Middleware Design

1. **Single Responsibility**: Each middleware should focus on one concern
2. **Composability**: Design middleware to work well with others
3. **Error Handling**: Properly handle and propagate errors
4. **Context Awareness**: Use context for cancellation and timeouts
5. **Performance**: Minimize overhead, especially for frequently executed actions

### Middleware Composition

1. **Order Matters**: Apply middleware in a logical order
2. **Avoid Deep Nesting**: Use `MiddlewareStack` for cleaner code
3. **Consistent Application**: Apply the same middleware stack to similar actions

### Error Handling

1. **Preserve Original Errors**: Wrap errors but don't lose the original cause
2. **Contextual Information**: Add context to errors when appropriate
3. **Selective Retry**: Only retry errors that might be transient

### Testing

1. **Test Each Middleware**: Unit test middleware in isolation
2. **Test Compositions**: Test combinations of middleware
3. **Mock Dependencies**: Use mocks for external dependencies
4. **Test Error Cases**: Ensure middleware handles errors correctly

## Conclusion

The middleware system in Flow Orchestrator provides a powerful way to extend and enhance workflow actions without modifying their core implementation. By understanding the middleware pattern and following best practices, you can create maintainable, reusable, and composable workflows that handle cross-cutting concerns elegantly.

Whether you're using the built-in middleware or creating custom implementations, the functional composition pattern enables a clean separation of concerns and promotes code reuse across your workflows.

## Package Organization

The Middleware System is part of the public API in the `pkg/workflow` package. All middleware types and functions are accessible through this package:

```go
import "github.com/yourusername/flow-orchestrator/pkg/workflow"
```

The internal implementation details of the middleware system are encapsulated in the internal packages and not exposed to users. 