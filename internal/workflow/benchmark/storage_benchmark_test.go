package benchmark

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
)

// BenchmarkStorage benchmarks performance of different storage backends
func BenchmarkStorage(b *testing.B) {
	// Test data sizes
	dataSizes := []int{10, 100, 1000}

	// Benchmark saving workflow data
	b.Run("Save", func(b *testing.B) {
		for _, size := range dataSizes {
			b.Run(fmt.Sprintf("DataSize=%d/JSON", size), func(b *testing.B) {
				benchmarkJSONSave(b, size)
			})

			b.Run(fmt.Sprintf("DataSize=%d/InMemory", size), func(b *testing.B) {
				benchmarkInMemorySave(b, size)
			})
		}
	})

	// Benchmark loading workflow data
	b.Run("Load", func(b *testing.B) {
		for _, size := range dataSizes {
			b.Run(fmt.Sprintf("DataSize=%d/JSON", size), func(b *testing.B) {
				benchmarkJSONLoad(b, size)
			})

			b.Run(fmt.Sprintf("DataSize=%d/InMemory", size), func(b *testing.B) {
				benchmarkInMemoryLoad(b, size)
			})
		}
	})

	// Benchmark snapshots
	b.Run("Snapshot", func(b *testing.B) {
		for _, size := range dataSizes {
			b.Run(fmt.Sprintf("DataSize=%d", size), func(b *testing.B) {
				benchmarkSnapshot(b, size)
			})
		}
	})

	// Benchmark restore from snapshot
	b.Run("RestoreSnapshot", func(b *testing.B) {
		for _, size := range dataSizes {
			b.Run(fmt.Sprintf("DataSize=%d", size), func(b *testing.B) {
				benchmarkRestoreSnapshot(b, size)
			})
		}
	})
}

// Setup temp directory for JSON storage
func setupTempDir(b *testing.B) string {
	tmpDir, err := os.MkdirTemp("", "workflow-benchmark")
	if err != nil {
		b.Fatalf("Failed to create temp directory: %v", err)
	}
	return tmpDir
}

// Create test workflow data with various sizes
func createTestWorkflowData(size int) *workflow.WorkflowData {
	data := workflow.NewWorkflowData(fmt.Sprintf("benchmark-data-%d", size))

	// Add various data types
	for i := 0; i < size; i++ {
		// Add some string data
		data.Set(fmt.Sprintf("string-key-%d", i), fmt.Sprintf("value-%d", i))

		// Add some numeric data
		data.Set(fmt.Sprintf("int-key-%d", i), i)
		data.Set(fmt.Sprintf("float-key-%d", i), float64(i)*1.5)

		// Add some boolean data
		data.Set(fmt.Sprintf("bool-key-%d", i), i%2 == 0)

		// Add some nested data for larger sizes
		if size >= 100 && i < size/10 {
			nestedMap := make(map[string]interface{})
			for j := 0; j < 10; j++ {
				nestedMap[fmt.Sprintf("nested-key-%d", j)] = j
			}
			data.Set(fmt.Sprintf("nested-key-%d", i), nestedMap)
		}
	}

	// Set some node statuses
	nodeCount := size / 10
	if nodeCount < 5 {
		nodeCount = 5
	}

	for i := 0; i < nodeCount; i++ {
		nodeName := fmt.Sprintf("node-%d", i)
		if i < nodeCount/2 {
			data.SetNodeStatus(nodeName, workflow.Completed)
			data.SetOutput(nodeName, fmt.Sprintf("output-%d", i))
		} else {
			data.SetNodeStatus(nodeName, workflow.Pending)
		}
	}

	return data
}

// Benchmark JSON storage backend save performance
func benchmarkJSONSave(b *testing.B, size int) {
	tmpDir := setupTempDir(b)
	defer os.RemoveAll(tmpDir)

	jsonStore, err := NewJSONFileStore(tmpDir)
	if err != nil {
		b.Fatalf("Failed to create JSON store: %v", err)
	}

	data := createTestWorkflowData(size)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		data.Set("iteration", i) // Small change to prevent caching
		err := jsonStore.Save(data)
		if err != nil {
			b.Fatalf("Failed to save data: %v", err)
		}
	}
}

// Benchmark JSON storage backend load performance
func benchmarkJSONLoad(b *testing.B, size int) {
	tmpDir := setupTempDir(b)
	defer os.RemoveAll(tmpDir)

	jsonStore, err := NewJSONFileStore(tmpDir)
	if err != nil {
		b.Fatalf("Failed to create JSON store: %v", err)
	}

	// Save data first
	data := createTestWorkflowData(size)
	err = jsonStore.Save(data)
	if err != nil {
		b.Fatalf("Failed to save data: %v", err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		loadedData, err := jsonStore.Load(data.GetWorkflowID())
		if err != nil {
			b.Fatalf("Failed to load data: %v", err)
		}
		if loadedData == nil {
			b.Fatalf("Loaded data is nil")
		}
	}
}

// Benchmark in-memory storage backend save performance
func benchmarkInMemorySave(b *testing.B, size int) {
	inMemoryStore := NewInMemoryStore()
	data := createTestWorkflowData(size)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		data.Set("iteration", i) // Small change to prevent caching
		err := inMemoryStore.Save(data)
		if err != nil {
			b.Fatalf("Failed to save data: %v", err)
		}
	}
}

// Benchmark in-memory storage backend load performance
func benchmarkInMemoryLoad(b *testing.B, size int) {
	inMemoryStore := NewInMemoryStore()

	// Save data first
	data := createTestWorkflowData(size)
	err := inMemoryStore.Save(data)
	if err != nil {
		b.Fatalf("Failed to save data: %v", err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		loadedData, err := inMemoryStore.Load(data.GetWorkflowID())
		if err != nil {
			b.Fatalf("Failed to load data: %v", err)
		}
		if loadedData == nil {
			b.Fatalf("Loaded data is nil")
		}
	}
}

// Benchmark snapshot creation
func benchmarkSnapshot(b *testing.B, size int) {
	data := createTestWorkflowData(size)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		data.Set("iteration", i) // Small change to prevent caching
		snapshot, err := data.Snapshot()
		if err != nil {
			b.Fatalf("Failed to create snapshot: %v", err)
		}
		if snapshot == nil {
			b.Fatalf("Snapshot is nil")
		}
	}
}

// Benchmark restore from snapshot
func benchmarkRestoreSnapshot(b *testing.B, size int) {
	sourceData := createTestWorkflowData(size)
	snapshot, err := sourceData.Snapshot()
	if err != nil {
		b.Fatalf("Failed to create snapshot: %v", err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		data := workflow.NewWorkflowData(fmt.Sprintf("new-data-%d", i))
		data.LoadSnapshot(snapshot)
	}
}

// Create a simple JSON file store
func NewJSONFileStore(dir string) (workflow.WorkflowStore, error) {
	err := os.MkdirAll(dir, 0755)
	if err != nil {
		return nil, err
	}

	return &jsonFileStore{
		dir: dir,
	}, nil
}

// JSON file store implementation
type jsonFileStore struct {
	dir string
}

// Save workflow data to a JSON file
func (s *jsonFileStore) Save(data *workflow.WorkflowData) error {
	filePath := filepath.Join(s.dir, data.GetWorkflowID()+".json")
	return data.SaveToJSON(filePath)
}

// Load workflow data from a JSON file
func (s *jsonFileStore) Load(id string) (*workflow.WorkflowData, error) {
	filePath := filepath.Join(s.dir, id+".json")
	data := workflow.NewWorkflowData(id)
	err := data.LoadFromJSON(filePath)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// Delete workflow data file
func (s *jsonFileStore) Delete(id string) error {
	filePath := filepath.Join(s.dir, id+".json")
	return os.Remove(filePath)
}

// ListWorkflows returns a list of workflow IDs
func (s *jsonFileStore) ListWorkflows() ([]string, error) {
	files, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}

	workflows := make([]string, 0, len(files))
	for _, file := range files {
		if file.IsDir() {
			continue
		}

		name := file.Name()
		if filepath.Ext(name) == ".json" {
			workflows = append(workflows, name[:len(name)-5]) // Remove .json extension
		}
	}

	return workflows, nil
}

// Create a simple in-memory store
func NewInMemoryStore() workflow.WorkflowStore {
	return &inMemoryStore{
		data: make(map[string]*workflow.WorkflowData),
	}
}

// In-memory store implementation
type inMemoryStore struct {
	data map[string]*workflow.WorkflowData
}

// Save workflow data to memory
func (s *inMemoryStore) Save(data *workflow.WorkflowData) error {
	id := data.GetWorkflowID()

	// Create a snapshot of the data
	snapshot, err := data.Snapshot()
	if err != nil {
		return fmt.Errorf("failed to create snapshot: %w", err)
	}

	// Create a new data instance and load the snapshot
	clonedData := workflow.NewWorkflowData(id)
	clonedData.LoadSnapshot(snapshot)

	// Store the cloned data
	s.data[id] = clonedData
	return nil
}

// Load workflow data from memory
func (s *inMemoryStore) Load(id string) (*workflow.WorkflowData, error) {
	data, ok := s.data[id]
	if !ok {
		return nil, fmt.Errorf("workflow not found: %s", id)
	}

	// Create a snapshot of the stored data
	snapshot, err := data.Snapshot()
	if err != nil {
		return nil, fmt.Errorf("failed to create snapshot: %w", err)
	}

	// Create a new data instance and load the snapshot
	clonedData := workflow.NewWorkflowData(id)
	clonedData.LoadSnapshot(snapshot)

	return clonedData, nil
}

// Delete workflow data from memory
func (s *inMemoryStore) Delete(id string) error {
	delete(s.data, id)
	return nil
}

// ListWorkflows returns a list of workflow IDs
func (s *inMemoryStore) ListWorkflows() ([]string, error) {
	workflows := make([]string, 0, len(s.data))
	for id := range s.data {
		workflows = append(workflows, id)
	}
	return workflows, nil
}
