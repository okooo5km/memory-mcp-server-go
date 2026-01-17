package storage

import (
	"fmt"
	"time"
)

// Entity represents a node in the knowledge graph
type Entity struct {
	Name         string   `json:"name"`
	EntityType   string   `json:"entityType"`
	Observations []string `json:"observations"`
}

// Relation represents an edge between entities
type Relation struct {
	From         string `json:"from"`
	To           string `json:"to"`
	RelationType string `json:"relationType"`
}

// KnowledgeGraph represents the entire graph structure
type KnowledgeGraph struct {
	Entities  []Entity   `json:"entities"`
	Relations []Relation `json:"relations"`
	Truncated bool       `json:"truncated,omitempty"` // true if any data was truncated
}

// ObservationDeletion specifies which observations to delete
type ObservationDeletion struct {
	EntityName   string   `json:"entityName"`
	Observations []string `json:"observations"`
}

// EntitySummary is a lightweight entity representation for list results
type EntitySummary struct {
	Name       string `json:"name"`
	EntityType string `json:"entityType"`
}

// EntitySearchHit represents a search result with preview snippets
type EntitySearchHit struct {
	Name              string   `json:"name"`
	EntityType        string   `json:"entityType"`
	Snippets          []string `json:"snippets"`          // matched observation snippets (max 2)
	ObservationsCount int      `json:"observationsCount"` // total observations count
	RelationsCount    int      `json:"relationsCount"`    // related relations count
}

// SearchResult holds search results with pagination info
type SearchResult struct {
	Entities []EntitySearchHit `json:"entities"`
	Total    int               `json:"total"`
	Limit    int               `json:"limit"`
	HasMore  bool              `json:"hasMore"`
}

// GraphSummary holds a lightweight summary of the entire graph
type GraphSummary struct {
	// Statistics
	TotalEntities  int            `json:"totalEntities"`
	TotalRelations int            `json:"totalRelations"`
	EntityTypes    map[string]int `json:"entityTypes"`   // type -> count
	RelationTypes  map[string]int `json:"relationTypes"` // type -> count

	// Entity list (limited)
	Entities []EntitySummary `json:"entities"`
	Limit    int             `json:"limit"`
	HasMore  bool            `json:"hasMore"`
}

// Storage defines the interface for knowledge graph persistence
type Storage interface {
	// Initialize sets up the storage backend
	Initialize() error

	// Close cleans up resources
	Close() error

	// Entity operations
	CreateEntities(entities []Entity) ([]Entity, error)
	DeleteEntities(names []string) error

	// Relation operations
	CreateRelations(relations []Relation) ([]Relation, error)
	DeleteRelations(relations []Relation) error

	// Observation operations
	AddObservations(observations map[string][]string) (map[string][]string, error)
	DeleteObservations(deletions []ObservationDeletion) error

	// Query operations
	ReadGraph(mode string, limit int) (interface{}, error) // mode: "summary" or "full"
	SearchNodes(query string, limit int) (*SearchResult, error)
	OpenNodes(names []string) (*KnowledgeGraph, error)

	// Migration support
	ExportData() (*KnowledgeGraph, error)
	ImportData(graph *KnowledgeGraph) error
}

// Config holds storage configuration
type Config struct {
	Type           string        // "sqlite" or "jsonl"
	FilePath       string        // Path to database or JSONL file
	AutoMigrate    bool          // Auto-migrate from JSONL to SQLite
	MigrationBatch int           // Batch size for migration
	WALMode        bool          // Enable WAL mode for SQLite
	CacheSize      int           // SQLite cache size in pages
	BusyTimeout    time.Duration // SQLite busy timeout
}

// Factory creates storage instances based on configuration
func NewStorage(config Config) (Storage, error) {
	switch config.Type {
	case "sqlite":
		return NewSQLiteStorage(config)
	case "jsonl":
		return NewJSONLStorage(config)
	default:
		return nil, fmt.Errorf("unknown storage type: %s", config.Type)
	}
}
