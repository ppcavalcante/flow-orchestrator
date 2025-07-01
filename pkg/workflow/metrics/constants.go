// Package metrics provides functionality for collecting and reporting performance metrics
// for workflow execution.
package metrics

import (
	internal "github.com/ppcavalcante/flow-orchestrator/internal/workflow/metrics"
)

// Operation types for data operations
const (
	// OpSet represents a data set operation
	OpSet = internal.OpSet
	// OpGet represents a data get operation
	OpGet = internal.OpGet
	// OpDelete represents a data delete operation
	OpDelete = internal.OpDelete
	// OpGetBool represents a boolean get operation
	OpGetBool = internal.OperationType("get_bool")
	// OpGetString represents a string get operation
	OpGetString = internal.OperationType("get_string")
	// OpGetFloat64 represents a float64 get operation
	OpGetFloat64 = internal.OperationType("get_float64")
	// OpGetInt represents an int get operation
	OpGetInt = internal.OperationType("get_int")
)

// Operation types for node status operations
const (
	// OpSetStatus represents a node status set operation
	OpSetStatus = internal.OpSetStatus
	// OpGetStatus represents a node status get operation
	OpGetStatus = internal.OpGetStatus
)

// Operation types for node output operations
const (
	// OpSetOutput represents a node output set operation
	OpSetOutput = internal.OpSetOutput
	// OpGetOutput represents a node output get operation
	OpGetOutput = internal.OpGetOutput
)

// Operation types for dependency resolution
const (
	// OpIsNodeRunnable represents a node dependency check operation
	OpIsNodeRunnable = internal.OpIsNodeRunnable
)

// Operation types for serialization
const (
	// OpSnapshot represents a workflow snapshot operation
	OpSnapshot = internal.OpSnapshot
	// OpLoadSnapshot represents a workflow snapshot load operation
	OpLoadSnapshot = internal.OpLoadSnapshot
)

// Operation types for lock operations
const (
	// OpLockAcquire represents a mutex lock acquire operation
	OpLockAcquire = internal.OpLockAcquire
	// OpLockRelease represents a mutex lock release operation
	OpLockRelease = internal.OpLockRelease
	// OpRLockAcquire represents a read lock acquire operation
	OpRLockAcquire = internal.OpRLockAcquire
	// OpRLockRelease represents a read lock release operation
	OpRLockRelease = internal.OpRLockRelease
)
