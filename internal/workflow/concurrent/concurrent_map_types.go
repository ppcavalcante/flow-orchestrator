package concurrent

// ConcurrentMapI defines the interface for thread-safe maps
// This allows us to abstract implementation details and use
// different map implementations based on workload characteristics
type ConcurrentMapI interface {
	// Core operations
	Set(key string, value interface{})
	Get(key string) (interface{}, bool)
	Delete(key string)
	Clear()

	// Query operations
	Has(key string) bool
	Count() int
	Keys() []string
	Items() map[string]interface{}

	// Iteration
	ForEach(fn func(key string, value interface{}))
}

// MapType specifies the type of concurrent map to use
type MapType int

const (
	// MapTypeSharded uses a sharded map with multiple locks for general-purpose use
	MapTypeSharded MapType = iota

	// MapTypeReadOptimized uses a lock-free read map optimized for read-heavy workloads
	MapTypeReadOptimized
)

// MapConfig defines configuration options for concurrent maps
type MapConfig struct {
	// Initial capacity hint
	Capacity int

	// Type of map to create
	Type MapType
}

// DefaultMapConfig returns a default map configuration
func DefaultMapConfig() MapConfig {
	return MapConfig{
		Capacity: 16,
		Type:     MapTypeSharded, // Default to sharded for balance
	}
}

// ReadOptimizedMapConfig returns a configuration optimized for read-heavy workloads
func ReadOptimizedMapConfig(capacity int) MapConfig {
	return MapConfig{
		Capacity: capacity,
		Type:     MapTypeReadOptimized,
	}
}

// NewConcurrentMapFromConfig creates the appropriate concurrent map based on the config
func NewConcurrentMapFromConfig(config MapConfig) ConcurrentMapI {
	switch config.Type {
	case MapTypeReadOptimized:
		return NewReadMapWithCapacity(config.Capacity)
	default: // MapTypeSharded
		return NewConcurrentMapWithCapacity(config.Capacity)
	}
}
