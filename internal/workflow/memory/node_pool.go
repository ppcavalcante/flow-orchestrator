package memory

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
)

// NodePool provides a pool of reusable Node objects to reduce allocations
// and improve memory locality by using data-oriented design principles.
type NodePool struct {
	pool sync.Pool

	// Pre-allocated slices to avoid allocations
	dependsOnPool [][]string
	mu            sync.Mutex

	// Statistics for monitoring
	stats struct {
		gets     int64
		puts     int64
		misses   int64
		capacity int
	}
}

// NewNodePool creates a new node pool with the given capacity
func NewNodePool(capacity int) *NodePool {
	p := &NodePool{
		pool: sync.Pool{
			New: func() interface{} {
				return &workflow.Node{
					DependsOn: make([]*workflow.Node, 0, 4), // Pre-allocate with small capacity
				}
			},
		},
		dependsOnPool: make([][]string, 0, capacity),
	}

	// Pre-allocate string slices for dependencies
	for i := 0; i < capacity; i++ {
		p.dependsOnPool = append(p.dependsOnPool, make([]string, 0, 8)) // Typical number of dependencies
	}

	p.stats.capacity = capacity
	return p
}

// Get retrieves a Node from the pool or creates a new one if the pool is empty
func (p *NodePool) Get() *workflow.Node {
	atomic.AddInt64(&p.stats.gets, 1)

	// Get a node from the pool
	nodeVal := p.pool.Get()
	node := nodeVal.(*workflow.Node) // nolint:errcheck // Safe because we only put *workflow.Node in the pool

	// If we got a new node (not from the pool), increment misses
	if node.Name == "" {
		atomic.AddInt64(&p.stats.misses, 1)
	}

	return node
}

// Put returns a Node to the pool after resetting its state
func (p *NodePool) Put(node *workflow.Node) {
	if node == nil {
		return
	}

	// Reset node to zero state
	node.Name = ""
	node.Action = nil
	node.DependsOn = node.DependsOn[:0] // Keep the slice but clear contents
	node.RetryCount = 0
	node.Timeout = 0

	p.pool.Put(node)

	// Update stats
	p.mu.Lock()
	p.stats.puts++
	p.mu.Unlock()
}

// GetStats returns statistics about the pool's usage
func (p *NodePool) GetStats() (gets, puts, misses, capacity int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return int(p.stats.gets), int(p.stats.puts), int(p.stats.misses), p.stats.capacity
}

// Global node pool for package-wide use
var globalNodePool = NewNodePool(1024)

// GetNode gets a node from the global pool
func GetNode() *workflow.Node {
	return globalNodePool.Get()
}

// PutNode returns a node to the global pool
func PutNode(node *workflow.Node) {
	globalNodePool.Put(node)
}

// CreateNode creates a new node with the given name and action using the pool
func CreateNode(name string, action workflow.Action) *workflow.Node {
	node := GetNode()
	node.Name = name
	node.Action = action
	return node
}

// CreateNodeWithDependencies creates a new node with dependencies using the pool
func CreateNodeWithDependencies(name string, action workflow.Action, deps ...*workflow.Node) *workflow.Node {
	node := CreateNode(name, action)

	// Add dependencies
	if len(deps) > 0 {
		// Pre-grow the slice if needed
		if cap(node.DependsOn) < len(deps) {
			node.DependsOn = make([]*workflow.Node, 0, len(deps))
		}
		node.DependsOn = append(node.DependsOn, deps...)
	}

	return node
}

// CreateNodeWithRetries creates a new node with retry configuration using the pool
func CreateNodeWithRetries(name string, action workflow.Action, retryCount int) *workflow.Node {
	node := CreateNode(name, action)
	node.RetryCount = retryCount
	return node
}

// CreateNodeWithTimeout creates a new node with timeout configuration using the pool
func CreateNodeWithTimeout(name string, action workflow.Action, timeout time.Duration) *workflow.Node {
	node := CreateNode(name, action)
	node.Timeout = timeout
	return node
}
