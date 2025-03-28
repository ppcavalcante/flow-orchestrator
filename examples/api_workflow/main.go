package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"time"

	"github.com/pparaujo/flow-orchestrator/pkg/workflow"
)

// Configuration flags for testing scenarios
var (
	// Failure scenarios
	forceFetchFailure   bool
	forceProcessFailure bool
	forceAPIFailure     bool
	forceDBFailure      bool

	// Failure rates (0.0-1.0)
	fetchFailureRate float64
	apiFailureRate   float64
	dbFailureRate    float64

	// Retry visibility
	verboseRetries bool

	// Workflow resumption
	resumeFromState string

	// Delay simulation
	simulateSlowAPI bool

	// Worker pool configuration
	useCustomWorkerPool bool
	workerPoolSize      int
	maxQueueSize        int

	// Retry settings
	maxRetries int
)

// Data structures for our workflow
type UserData struct {
	ID       int       `json:"id"`
	Name     string    `json:"name"`
	Email    string    `json:"email"`
	Created  time.Time `json:"created"`
	LastSeen time.Time `json:"last_seen"`
}

type ProcessedUserData struct {
	ID            int       `json:"id"`
	Name          string    `json:"name"`
	Email         string    `json:"email"`
	DaysSinceJoin int       `json:"days_since_join"`
	LastActive    time.Time `json:"last_active"`
	IsActive      bool      `json:"is_active"`
}

type APIResponse struct {
	Success bool   `json:"success"`
	ID      string `json:"id"`
}

type DBSaveResult struct {
	RecordID     int  `json:"record_id"`
	Success      bool `json:"success"`
	RowsAffected int  `json:"rows_affected"`
}

type WorkflowResult struct {
	TotalProcessed   int      `json:"total_processed"`
	APISuccess       bool     `json:"api_success"`
	DBSuccess        bool     `json:"db_success"`
	ProcessingTimeMs int      `json:"processing_time_ms"`
	Errors           []string `json:"errors"`
}

// Mock API client for user data
type UserAPIClient struct {
	BaseURL string
}

func NewUserAPIClient(baseURL string) *UserAPIClient {
	return &UserAPIClient{
		BaseURL: baseURL,
	}
}

func (c *UserAPIClient) FetchUsers(ctx context.Context) ([]UserData, error) {
	// Check for forced failure
	if forceFetchFailure {
		return nil, fmt.Errorf("forced fetch failure")
	}

	// Check for random failure based on rate
	if rand.Float64() < fetchFailureRate {
		return nil, fmt.Errorf("random fetch failure (rate: %.2f)", fetchFailureRate)
	}

	// Simulate API call delay
	delay := 300 * time.Millisecond
	if simulateSlowAPI {
		delay = 1 * time.Second
	}
	time.Sleep(delay)

	// Simulate fetch from API
	users := []UserData{
		{
			ID:       1,
			Name:     "Alice Smith",
			Email:    "alice@example.com",
			Created:  time.Now().AddDate(0, -3, 0), // 3 months ago
			LastSeen: time.Now().AddDate(0, 0, -2), // 2 days ago
		},
		{
			ID:       2,
			Name:     "Bob Johnson",
			Email:    "bob@example.com",
			Created:  time.Now().AddDate(0, -6, 0),  // 6 months ago
			LastSeen: time.Now().AddDate(0, 0, -10), // 10 days ago
		},
		{
			ID:       3,
			Name:     "Carol Williams",
			Email:    "carol@example.com",
			Created:  time.Now().AddDate(0, -1, 0), // 1 month ago
			LastSeen: time.Now().AddDate(0, 0, -1), // 1 day ago
		},
	}

	return users, nil
}

// Mock Analytics API client
type AnalyticsAPIClient struct {
	BaseURL string
}

func NewAnalyticsAPIClient(baseURL string) *AnalyticsAPIClient {
	return &AnalyticsAPIClient{
		BaseURL: baseURL,
	}
}

func (c *AnalyticsAPIClient) SendUserActivity(ctx context.Context, data []ProcessedUserData) (*APIResponse, error) {
	// Check for forced failure
	if forceAPIFailure {
		return nil, fmt.Errorf("forced API failure")
	}

	// Check for random failure based on rate
	if rand.Float64() < apiFailureRate {
		return nil, fmt.Errorf("analytics API connection timeout (rate: %.2f)", apiFailureRate)
	}

	// Simulate API call delay
	delay := 200 * time.Millisecond
	if simulateSlowAPI {
		delay = 800 * time.Millisecond
	}
	time.Sleep(delay)

	// Simulate random failure (10% chance)
	if rand.Float32() < 0.1 {
		return nil, fmt.Errorf("analytics API connection timeout")
	}

	return &APIResponse{
		Success: true,
		ID:      "txn-" + fmt.Sprintf("%d", rand.Intn(100000)),
	}, nil
}

// Mock Database client
type DBClient struct {
	ConnectionString string
}

func NewDBClient(connectionString string) *DBClient {
	return &DBClient{
		ConnectionString: connectionString,
	}
}

func (c *DBClient) SaveUserStats(ctx context.Context, data []ProcessedUserData) (*DBSaveResult, error) {
	// Check for forced failure
	if forceDBFailure {
		return nil, fmt.Errorf("forced DB failure")
	}

	// Check for random failure based on rate
	if rand.Float64() < dbFailureRate {
		return nil, fmt.Errorf("database connection error (rate: %.2f)", dbFailureRate)
	}

	// Simulate DB operation delay
	delay := 150 * time.Millisecond
	if simulateSlowAPI {
		delay = 500 * time.Millisecond
	}
	time.Sleep(delay)

	// Simulate random failure (5% chance)
	if rand.Float32() < 0.05 {
		return nil, fmt.Errorf("database connection error")
	}

	return &DBSaveResult{
		RecordID:     rand.Intn(1000),
		Success:      true,
		RowsAffected: len(data),
	}, nil
}

// Workflow Node Actions
func fetchDataAction(ctx context.Context, data *workflow.WorkflowData) error {
	fmt.Println("📡 Fetching user data from API...")

	// In a real implementation, we would use the client to fetch data
	// client := NewUserAPIClient("https://api.example.com/users")

	// Check for forced failure when testing
	if forceFetchFailure {
		fmt.Println("❌ Forced fetch failure")
		return fmt.Errorf("forced fetch failure")
	}

	// Check for random failure based on rate
	if rand.Float64() < fetchFailureRate {
		fmt.Printf("❌ Random fetch failure (rate: %.2f)", fetchFailureRate)
		return fmt.Errorf("random fetch failure (rate: %.2f)", fetchFailureRate)
	}

	// Simulate delay
	delay := 300 * time.Millisecond
	if simulateSlowAPI {
		delay = 1 * time.Second
	}
	time.Sleep(delay)

	// Generate mock user data
	users := []UserData{
		{
			ID:       1,
			Name:     "Alice Smith",
			Email:    "alice@example.com",
			Created:  time.Now().Add(-30 * 24 * time.Hour),
			LastSeen: time.Now().Add(-2 * 24 * time.Hour),
		},
		{
			ID:       2,
			Name:     "Bob Johnson",
			Email:    "bob@example.com",
			Created:  time.Now().Add(-15 * 24 * time.Hour),
			LastSeen: time.Now().Add(-1 * 24 * time.Hour),
		},
		{
			ID:       3,
			Name:     "Carol Williams",
			Email:    "carol@example.com",
			Created:  time.Now().Add(-60 * 24 * time.Hour),
			LastSeen: time.Now().Add(-10 * 24 * time.Hour),
		},
	}

	// Store the fetched data in the workflow state
	usersJSON, err := json.Marshal(users)
	if err != nil {
		return fmt.Errorf("failed to marshal user data: %w", err)
	}

	// Store in workflow data
	data.Set("users", string(usersJSON))
	data.Set("fetch_timestamp", time.Now().Format(time.RFC3339))
	data.Set("user_count", len(users))

	fmt.Printf("Fetched %d users\n", len(users))
	return nil
}

func processDataAction(ctx context.Context, data *workflow.WorkflowData) error {
	fmt.Println("Processing user data...")

	// Check for forced failure when testing
	if forceProcessFailure {
		fmt.Println("❌ Forced process failure")
		return fmt.Errorf("forced process failure")
	}

	// Get user data from state
	usersJSON, ok := data.Get("users")
	if !ok {
		return fmt.Errorf("no user data found in workflow state")
	}

	var users []UserData
	if err := json.Unmarshal([]byte(usersJSON.(string)), &users); err != nil {
		return fmt.Errorf("failed to unmarshal user data: %w", err)
	}

	// Process the data
	now := time.Now()
	processed := make([]ProcessedUserData, 0, len(users))

	for _, user := range users {
		daysSinceJoin := int(now.Sub(user.Created).Hours() / 24)
		isActive := now.Sub(user.LastSeen) < 7*24*time.Hour

		processed = append(processed, ProcessedUserData{
			ID:            user.ID,
			Name:          user.Name,
			Email:         user.Email,
			DaysSinceJoin: daysSinceJoin,
			LastActive:    user.LastSeen,
			IsActive:      isActive,
		})
	}

	// Store processed data
	processedJSON, err := json.Marshal(processed)
	if err != nil {
		return fmt.Errorf("failed to marshal processed data: %w", err)
	}

	// Update workflow data
	data.Set("processed_users", string(processedJSON))
	data.Set("processing_timestamp", time.Now().Format(time.RFC3339))
	data.Set("active_users", countActiveUsers(processed))

	fmt.Printf("Processed %d users (%d active)\n",
		len(processed),
		countActiveUsers(processed))

	return nil
}

func countActiveUsers(users []ProcessedUserData) int {
	count := 0
	for _, user := range users {
		if user.IsActive {
			count++
		}
	}
	return count
}

func sendToAPIAction(ctx context.Context, data *workflow.WorkflowData) error {
	fmt.Println("📤 Sending data to analytics API...")

	// Check for forced failure when testing
	if forceAPIFailure {
		fmt.Println("❌ Forced API failure")
		return fmt.Errorf("forced API failure")
	}

	// Check for random failure based on rate
	if rand.Float64() < apiFailureRate {
		fmt.Printf("❌ Random API failure (rate: %.2f)", apiFailureRate)
		return fmt.Errorf("analytics API connection timeout (rate: %.2f)", apiFailureRate)
	}

	// Get processed data from state
	processedJSON, ok := data.Get("processed_users")
	if !ok {
		return fmt.Errorf("no processed data found in workflow state")
	}

	var processed []ProcessedUserData
	if err := json.Unmarshal([]byte(processedJSON.(string)), &processed); err != nil {
		return fmt.Errorf("failed to unmarshal processed data: %w", err)
	}

	// In a real implementation, we would use the client to send data
	// client := NewAnalyticsAPIClient("https://analytics.example.com/ingest")

	// Simulate delay
	delay := 200 * time.Millisecond
	if simulateSlowAPI {
		delay = 800 * time.Millisecond
	}
	time.Sleep(delay)

	// Simulate successful response
	response := &APIResponse{
		Success: true,
		ID:      fmt.Sprintf("txn_%d", rand.Intn(1000000)),
	}

	// Store the response in workflow data
	responseJSON, err := json.Marshal(response)
	if err != nil {
		return fmt.Errorf("failed to marshal API response: %w", err)
	}

	// Update workflow data
	data.Set("api_response", string(responseJSON))
	data.Set("api_timestamp", time.Now().Format(time.RFC3339))
	data.Set("api_success", response.Success)
	data.Set("api_transaction_id", response.ID)

	fmt.Printf("Sent to API (Transaction ID: %s)\n", response.ID)
	return nil
}

func saveToDBAction(ctx context.Context, data *workflow.WorkflowData) error {
	fmt.Println("Saving data to database...")

	// Check for forced failure when testing
	if forceDBFailure {
		fmt.Println("❌ Forced DB failure")
		return fmt.Errorf("forced DB failure")
	}

	// Check for random failure based on rate
	if rand.Float64() < dbFailureRate {
		fmt.Printf("❌ Random DB failure (rate: %.2f)", dbFailureRate)
		return fmt.Errorf("database connection error (rate: %.2f)", dbFailureRate)
	}

	// Get processed data from state
	processedJSON, ok := data.Get("processed_users")
	if !ok {
		return fmt.Errorf("no processed data found in workflow state")
	}

	var processed []ProcessedUserData
	if err := json.Unmarshal([]byte(processedJSON.(string)), &processed); err != nil {
		return fmt.Errorf("failed to unmarshal processed data: %w", err)
	}

	// In a real implementation, we would use the client to save data
	// client := NewDBClient("postgres://user:pass@localhost:5432/analytics")

	// Simulate delay
	delay := 150 * time.Millisecond
	if simulateSlowAPI {
		delay = 500 * time.Millisecond
	}
	time.Sleep(delay)

	// Simulate successful save
	result := &DBSaveResult{
		RecordID:     rand.Intn(1000),
		Success:      true,
		RowsAffected: len(processed),
	}

	// Store the result in workflow data
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("failed to marshal DB result: %w", err)
	}

	// Update workflow data
	data.Set("db_result", string(resultJSON))
	data.Set("db_timestamp", time.Now().Format(time.RFC3339))
	data.Set("db_success", result.Success)
	data.Set("db_record_id", result.RecordID)

	fmt.Printf("Saved to database (Record ID: %d, Rows: %d)\n",
		result.RecordID,
		result.RowsAffected)

	return nil
}

func finalizeAction(ctx context.Context, data *workflow.WorkflowData) error {
	fmt.Println("Finalizing workflow...")

	// Get data from workflow state
	userCount, _ := data.Get("user_count")
	activeUsers, _ := data.Get("active_users")
	apiSuccess, _ := data.Get("api_success")
	dbSuccess, _ := data.Get("db_success")

	// Get timestamps for calculating duration
	startTimeStr, ok := data.Get("fetch_timestamp")
	if !ok {
		return fmt.Errorf("missing start timestamp in workflow state")
	}

	startTime, err := time.Parse(time.RFC3339, startTimeStr.(string))
	if err != nil {
		return fmt.Errorf("invalid start timestamp: %w", err)
	}

	// Calculate processing duration
	duration := time.Since(startTime)

	// Collect any errors
	var errors []string

	if apiSuccess == nil || !apiSuccess.(bool) {
		errors = append(errors, "API processing failed")
	}

	if dbSuccess == nil || !dbSuccess.(bool) {
		errors = append(errors, "Database save failed")
	}

	// Create final result
	result := WorkflowResult{
		TotalProcessed:   userCount.(int),
		APISuccess:       apiSuccess != nil && apiSuccess.(bool),
		DBSuccess:        dbSuccess != nil && dbSuccess.(bool),
		ProcessingTimeMs: int(duration.Milliseconds()),
		Errors:           errors,
	}

	// Store the final result
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("failed to marshal workflow result: %w", err)
	}

	// Update workflow data
	data.Set("workflow_result", string(resultJSON))

	// Store boolean values as strings for compatibility with FlatBuffers
	if len(errors) == 0 {
		data.Set("workflow_completed", "true")
		data.Set("workflow_success", "true")
	} else {
		data.Set("workflow_completed", "true")
		data.Set("workflow_success", "false")
	}

	// Print summary
	fmt.Println("\nWorkflow Summary:")
	fmt.Printf("  Total Users: %d\n", userCount)
	fmt.Printf("  Active Users: %d\n", activeUsers)
	fmt.Printf("  API Success: %v\n", apiSuccess)
	fmt.Printf("  DB Success: %v\n", dbSuccess)
	fmt.Printf("  Processing Time: %d ms\n", duration.Milliseconds())

	if len(errors) > 0 {
		fmt.Println("  Errors:")
		for _, err := range errors {
			fmt.Printf("    - %s\n", err)
		}
	}

	return nil
}

func init() {
	// Failure flags
	flag.BoolVar(&forceFetchFailure, "fetch-fail", false, "Force failure in fetch step")
	flag.BoolVar(&forceProcessFailure, "process-fail", false, "Force failure in process step")
	flag.BoolVar(&forceAPIFailure, "api-fail", false, "Force failure in API step")
	flag.BoolVar(&forceDBFailure, "db-fail", false, "Force failure in DB step")

	// Failure rates
	flag.Float64Var(&fetchFailureRate, "fetch-fail-rate", 0.0, "Failure rate for fetch operation (0.0-1.0)")
	flag.Float64Var(&apiFailureRate, "api-fail-rate", 0.0, "Failure rate for API operation (0.0-1.0)")
	flag.Float64Var(&dbFailureRate, "db-fail-rate", 0.0, "Failure rate for DB operation (0.0-1.0)")

	// Retry visibility
	flag.BoolVar(&verboseRetries, "verbose-retries", false, "Print detailed retry information")

	// Workflow resumption
	flag.StringVar(&resumeFromState, "resume-state", "", "Resume from state file")

	// Delay simulation
	flag.BoolVar(&simulateSlowAPI, "slow-api", false, "Simulate slow API responses")

	// Worker pool configuration
	flag.BoolVar(&useCustomWorkerPool, "use-worker-pool", false, "Use custom worker pool")
	flag.IntVar(&workerPoolSize, "worker-pool-size", 4, "Size of worker pool")
	flag.IntVar(&maxQueueSize, "max-queue-size", 100, "Maximum queue size")

	// Retry settings
	flag.IntVar(&maxRetries, "retries", 3, "Maximum retries for each node")
}

func main() {
	// Parse command line flags
	flag.Parse()

	// Seed random number generator
	rand.Seed(time.Now().UnixNano())

	fmt.Println("🚀 Starting API Workflow")

	// Create workflow store using FlatBuffers - fully compatible with WorkflowStore interface
	var workflowStore workflow.WorkflowStore

	// Make sure the directory exists for FlatBuffers files
	flatBuffersDir := "api_workflow_state"
	if err := os.MkdirAll(flatBuffersDir, 0755); err != nil {
		fmt.Printf("❌ Failed to create directory for FlatBuffers: %v\n", err)
		os.Exit(1)
	}

	flatBuffersStore, err := workflow.NewFlatBuffersStore(flatBuffersDir)
	if err != nil {
		fmt.Printf("❌ Failed to create FlatBuffers store: %v\n", err)

		// Fallback to JSON store if FlatBuffers fails
		fmt.Println("⚠️ Falling back to JSON store")
		jsonStore, err := workflow.NewJSONFileStore("api_workflow_state.json")
		if err != nil {
			fmt.Printf("❌ Failed to create workflow store: %v\n", err)
			os.Exit(1)
		}
		workflowStore = jsonStore
	} else {
		workflowStore = flatBuffersStore
	}

	// Create workflow using the builder API
	builder := workflow.NewWorkflowBuilder().
		WithStateStore(workflowStore)

	// Build workflow DAG with fluent API
	builder.AddStartNode("fetch").
		WithAction(fetchDataAction).
		WithRetries(maxRetries).
		WithTimeout(10 * time.Second)

	builder.AddNode("process").
		WithAction(processDataAction).
		WithRetries(maxRetries).
		DependsOn("fetch").
		WithTimeout(5 * time.Second)

	// Parallel API and DB operations
	builder.AddNode("api").
		WithAction(sendToAPIAction).
		WithRetries(maxRetries).
		DependsOn("process").
		WithTimeout(8 * time.Second)

	builder.AddNode("db").
		WithAction(saveToDBAction).
		WithRetries(maxRetries).
		DependsOn("process").
		WithTimeout(5 * time.Second)

	// Final aggregation
	builder.AddNode("finalize").
		WithAction(finalizeAction).
		DependsOn("api", "db").
		WithTimeout(3 * time.Second)

	// Build the DAG
	dag, err := builder.Build()
	if err != nil {
		fmt.Printf("❌ Failed to build workflow: %v\n", err)
		os.Exit(1)
	}

	// Execute the workflow
	fmt.Println("\n⚡ Executing workflow...")
	workflowID := "api-workflow"
	workflowData := workflow.NewWorkflowData(workflowID)
	err = dag.Execute(context.Background(), workflowData)
	if err != nil {
		fmt.Printf("\n❌ Workflow execution failed: %v\n", err)
		os.Exit(1)
	}

	// Explicitly set workflow success flag after execution
	// Store booleans as strings since FlatBuffers only handles string values
	workflowData.Set("workflow_completed", "true")
	workflowData.Set("workflow_success", "true")

	// Save the workflow data to the store after execution
	fmt.Println("Saving workflow data to store...")
	err = workflowStore.Save(workflowData)
	if err != nil {
		fmt.Printf("⚠️ Failed to save workflow data: %v\n", err)
		// Continue execution even if saving fails
	}

	// Try to load final state for reporting from store first
	var loadedWorkflowData *workflow.WorkflowData
	loadedWorkflowData, err = workflowStore.Load(workflowID)
	if err != nil {
		fmt.Printf("\n⚠️ Could not load workflow data from store: %v\n", err)
		fmt.Println("Using in-memory workflow data instead")
		loadedWorkflowData = workflowData // Use the in-memory data we still have
	}

	if loadedWorkflowData == nil {
		fmt.Println("\n❌ Failed to get workflow data")
		os.Exit(1)
	}

	// Check if workflow completed successfully
	workflowSuccess, ok := loadedWorkflowData.Get("workflow_success")
	if !ok {
		fmt.Println("\n⚠️ Workflow success flag not found in data")
		fmt.Println("✅ However, workflow completed all nodes successfully")
	} else if workflowSuccess == "false" {
		fmt.Println("\n⚠️ Workflow completed with errors")
	} else {
		fmt.Println("\n✅ Workflow completed successfully")
	}

	// Print final result
	resultJSON, ok := loadedWorkflowData.Get("workflow_result")
	if ok {
		fmt.Println("\n Final Result:")
		fmt.Println(resultJSON)
	}
}
