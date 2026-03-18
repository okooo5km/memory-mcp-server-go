package main

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"memory-mcp-server-go/storage"
)

func TestAddObservationsDuplicateEntity(t *testing.T) {
	// Setup: create a KnowledgeGraphManager with temp SQLite storage
	tempDir, err := os.MkdirTemp("", "add_obs_dedup_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	dbPath := filepath.Join(tempDir, "test.db")
	mgr, err := NewKnowledgeGraphManager(dbPath, "sqlite", false)
	if err != nil {
		t.Fatalf("Failed to create manager: %v", err)
	}
	defer mgr.Close()

	// Create the entity first
	_, err = mgr.CreateEntities([]storage.Entity{{
		Name:       "TestEntity",
		EntityType: "test",
	}})
	if err != nil {
		t.Fatalf("Failed to create entity: %v", err)
	}

	// Call AddObservations with TWO entries for the same entity
	results, err := mgr.AddObservations([]ObservationAddition{
		{EntityName: "TestEntity", Contents: []string{"obs-a", "obs-b"}},
		{EntityName: "TestEntity", Contents: []string{"obs-c", "obs-d"}},
	})
	if err != nil {
		t.Fatalf("AddObservations failed: %v", err)
	}

	// Collect all added observations across results
	var allAdded []string
	for _, r := range results {
		if r.EntityName == "TestEntity" {
			allAdded = append(allAdded, r.AddedObservations...)
		}
	}

	// All four observations must be present
	sort.Strings(allAdded)
	expected := []string{"obs-a", "obs-b", "obs-c", "obs-d"}
	if len(allAdded) != len(expected) {
		t.Fatalf("Expected %d observations added, got %d: %v", len(expected), len(allAdded), allAdded)
	}
	for i, exp := range expected {
		if allAdded[i] != exp {
			t.Errorf("Observation[%d]: expected %q, got %q", i, exp, allAdded[i])
		}
	}
}
