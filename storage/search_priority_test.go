// Created by okooo5km(十里)
// Tests for search_nodes name priority improvements

package storage

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSearchPriorityJSONL tests that name matches have higher priority than content matches
// for JSONL storage backend
func TestSearchPriorityJSONL(t *testing.T) {
	// Create temp directory for test
	tempDir, err := os.MkdirTemp("", "search_priority_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Create JSONL storage
	config := Config{
		FilePath: filepath.Join(tempDir, "test.jsonl"),
	}
	storage, err := NewJSONLStorage(config)
	if err != nil {
		t.Fatalf("Failed to create JSONL storage: %v", err)
	}

	if err := storage.Initialize(); err != nil {
		t.Fatalf("Failed to initialize storage: %v", err)
	}

	// Create test entities:
	// - "Claude Code" has "Claude" in name (should have higher priority)
	// - "VSCode" has "Claude" only in observations (should have lower priority)
	testEntities := []Entity{
		{
			Name:         "Claude Code",
			EntityType:   "tool",
			Observations: []string{"A CLI tool for coding assistance"},
		},
		{
			Name:         "VSCode",
			EntityType:   "editor",
			Observations: []string{"Supports Claude plugin for AI assistance"},
		},
		{
			Name:         "Vim",
			EntityType:   "editor",
			Observations: []string{"A classic text editor"},
		},
	}

	_, err = storage.CreateEntities(testEntities)
	if err != nil {
		t.Fatalf("Failed to create entities: %v", err)
	}

	// Search for "Claude"
	result, err := storage.SearchNodes("Claude", 10)
	if err != nil {
		t.Fatalf("Failed to search nodes: %v", err)
	}

	// Verify results
	if result.Total != 2 {
		t.Errorf("Expected 2 results, got %d", result.Total)
	}

	if len(result.Entities) < 2 {
		t.Fatalf("Expected at least 2 entities in results, got %d", len(result.Entities))
	}

	// "Claude Code" (name match) should be first
	if result.Entities[0].Name != "Claude Code" {
		t.Errorf("Expected first result to be 'Claude Code' (name match), got '%s'", result.Entities[0].Name)
	}

	// "VSCode" (content match) should be second
	if result.Entities[1].Name != "VSCode" {
		t.Errorf("Expected second result to be 'VSCode' (content match), got '%s'", result.Entities[1].Name)
	}

	t.Logf("Search priority test passed: '%s' (name match) ranked before '%s' (content match)",
		result.Entities[0].Name, result.Entities[1].Name)
}

// TestSearchPriorityExactVsPartial tests that exact name matches rank higher than partial matches
func TestSearchPriorityExactVsPartial(t *testing.T) {
	// Create temp directory for test
	tempDir, err := os.MkdirTemp("", "search_priority_exact_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Create JSONL storage
	config := Config{
		FilePath: filepath.Join(tempDir, "test.jsonl"),
	}
	storage, err := NewJSONLStorage(config)
	if err != nil {
		t.Fatalf("Failed to create JSONL storage: %v", err)
	}

	if err := storage.Initialize(); err != nil {
		t.Fatalf("Failed to initialize storage: %v", err)
	}

	// Create test entities:
	// - "Go" exact name match
	// - "Golang" partial name match
	// - "GoLand" partial name match
	testEntities := []Entity{
		{
			Name:         "GoLand",
			EntityType:   "IDE",
			Observations: []string{"JetBrains IDE for Go development"},
		},
		{
			Name:         "Go",
			EntityType:   "language",
			Observations: []string{"A programming language by Google"},
		},
		{
			Name:         "Golang",
			EntityType:   "language",
			Observations: []string{"Another name for Go language"},
		},
	}

	_, err = storage.CreateEntities(testEntities)
	if err != nil {
		t.Fatalf("Failed to create entities: %v", err)
	}

	// Search for "Go" (exact match should rank highest)
	result, err := storage.SearchNodes("Go", 10)
	if err != nil {
		t.Fatalf("Failed to search nodes: %v", err)
	}

	// Verify results
	if result.Total != 3 {
		t.Errorf("Expected 3 results, got %d", result.Total)
	}

	if len(result.Entities) < 1 {
		t.Fatalf("Expected at least 1 entity in results, got %d", len(result.Entities))
	}

	// "Go" (exact name match) should be first
	if result.Entities[0].Name != "Go" {
		t.Errorf("Expected first result to be 'Go' (exact match), got '%s'", result.Entities[0].Name)
	}

	t.Logf("Exact match priority test passed: '%s' (exact match) ranked first",
		result.Entities[0].Name)
}

// TestSearchPrioritySQLite tests search priority for SQLite backend
func TestSearchPrioritySQLite(t *testing.T) {
	// Create temp directory for test
	tempDir, err := os.MkdirTemp("", "search_priority_sqlite_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Create SQLite storage
	config := Config{
		FilePath:    filepath.Join(tempDir, "test.db"),
		WALMode:     true,
		CacheSize:   1000,
		BusyTimeout: 5000,
	}
	storage, err := NewSQLiteStorage(config)
	if err != nil {
		t.Fatalf("Failed to create SQLite storage: %v", err)
	}

	if err := storage.Initialize(); err != nil {
		t.Fatalf("Failed to initialize storage: %v", err)
	}
	defer storage.Close()

	// Create test entities:
	// - "Claude Code" has "Claude" in name (should have higher priority)
	// - "VSCode" has "Claude" only in observations (should have lower priority)
	testEntities := []Entity{
		{
			Name:         "Claude Code",
			EntityType:   "tool",
			Observations: []string{"A CLI tool for coding assistance"},
		},
		{
			Name:         "VSCode",
			EntityType:   "editor",
			Observations: []string{"Supports Claude plugin for AI assistance"},
		},
		{
			Name:         "Vim",
			EntityType:   "editor",
			Observations: []string{"A classic text editor"},
		},
	}

	_, err = storage.CreateEntities(testEntities)
	if err != nil {
		t.Fatalf("Failed to create entities: %v", err)
	}

	// Search for "Claude"
	result, err := storage.SearchNodes("Claude", 10)
	if err != nil {
		t.Fatalf("Failed to search nodes: %v", err)
	}

	// Verify results
	if result.Total != 2 {
		t.Errorf("Expected 2 results, got %d", result.Total)
	}

	if len(result.Entities) < 2 {
		t.Fatalf("Expected at least 2 entities in results, got %d", len(result.Entities))
	}

	// "Claude Code" (name match) should be first
	if result.Entities[0].Name != "Claude Code" {
		t.Errorf("Expected first result to be 'Claude Code' (name match), got '%s'", result.Entities[0].Name)
	}

	// "VSCode" (content match) should be second
	if result.Entities[1].Name != "VSCode" {
		t.Errorf("Expected second result to be 'VSCode' (content match), got '%s'", result.Entities[1].Name)
	}

	t.Logf("SQLite search priority test passed: '%s' (name match) ranked before '%s' (content match)",
		result.Entities[0].Name, result.Entities[1].Name)
}

// TestSearchPriorityTypeMatch tests that type matches rank between name and content matches
func TestSearchPriorityTypeMatch(t *testing.T) {
	// Create temp directory for test
	tempDir, err := os.MkdirTemp("", "search_priority_type_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Create JSONL storage
	config := Config{
		FilePath: filepath.Join(tempDir, "test.jsonl"),
	}
	storage, err := NewJSONLStorage(config)
	if err != nil {
		t.Fatalf("Failed to create JSONL storage: %v", err)
	}

	if err := storage.Initialize(); err != nil {
		t.Fatalf("Failed to initialize storage: %v", err)
	}

	// Create test entities:
	// - "MyTool" has "tool" in name -> name match (highest)
	// - "VSCode" has "tool" in type -> type match (medium)
	// - "Project" has "tool" only in observations -> content match (lowest)
	testEntities := []Entity{
		{
			Name:         "Project",
			EntityType:   "software",
			Observations: []string{"This project uses various tool chains"},
		},
		{
			Name:         "VSCode",
			EntityType:   "tool",
			Observations: []string{"A code editor"},
		},
		{
			Name:         "MyTool",
			EntityType:   "application",
			Observations: []string{"A custom application"},
		},
	}

	_, err = storage.CreateEntities(testEntities)
	if err != nil {
		t.Fatalf("Failed to create entities: %v", err)
	}

	// Search for "tool"
	result, err := storage.SearchNodes("tool", 10)
	if err != nil {
		t.Fatalf("Failed to search nodes: %v", err)
	}

	// Verify results
	if result.Total != 3 {
		t.Errorf("Expected 3 results, got %d", result.Total)
	}

	if len(result.Entities) < 3 {
		t.Fatalf("Expected 3 entities in results, got %d", len(result.Entities))
	}

	// Verify priority order: name match > type match > content match
	// "MyTool" (name match) should be first
	if result.Entities[0].Name != "MyTool" {
		t.Errorf("Expected first result to be 'MyTool' (name match), got '%s'", result.Entities[0].Name)
	}

	// "VSCode" (type match) should be second
	if result.Entities[1].Name != "VSCode" {
		t.Errorf("Expected second result to be 'VSCode' (type match), got '%s'", result.Entities[1].Name)
	}

	// "Project" (content match) should be third
	if result.Entities[2].Name != "Project" {
		t.Errorf("Expected third result to be 'Project' (content match), got '%s'", result.Entities[2].Name)
	}

	t.Logf("Type match priority test passed: '%s' (name) > '%s' (type) > '%s' (content)",
		result.Entities[0].Name, result.Entities[1].Name, result.Entities[2].Name)
}
