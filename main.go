package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"memory-mcp-server-go/storage"

	// Use pure Go SQLite driver
	_ "modernc.org/sqlite"
)

// Legacy types for backward compatibility with JSON marshaling
type ObservationAddition struct {
	EntityName string   `json:"entityName"`
	Contents   []string `json:"contents"`
}

type ObservationAdditionResult struct {
	EntityName        string   `json:"entityName"`
	AddedObservations []string `json:"addedObservations"`
}

// KnowledgeGraphManager manages the knowledge graph using the storage abstraction
type KnowledgeGraphManager struct {
	storage    storage.Storage
	memoryPath string
}

// NewKnowledgeGraphManager creates a new manager with auto-detection of storage type
func NewKnowledgeGraphManager(memoryPath string, storageType string, autoMigrate bool) (*KnowledgeGraphManager, error) {
	// Resolve memory path
	resolvedPath := resolveMemoryPath(memoryPath)
	var finalPath string

	// Auto-detect storage type if not specified
	if storageType == "" {
		storageType, finalPath = detectStorageType(resolvedPath, autoMigrate)
	} else {
		finalPath = resolvedPath
		// Handle SQLite path adjustment for explicit storage type
		if storageType == "sqlite" && !strings.HasSuffix(resolvedPath, ".db") {
			finalPath = strings.TrimSuffix(resolvedPath, filepath.Ext(resolvedPath)) + ".db"
		}
	}

	// Handle auto-migration BEFORE creating storage
	if autoMigrate && storageType == "sqlite" && resolvedPath != finalPath {
		// Check if we need to migrate
		if _, err := os.Stat(resolvedPath); err == nil {
			if _, err := os.Stat(finalPath); os.IsNotExist(err) {
				log.Printf("Performing seamless migration from %s to %s...", resolvedPath, finalPath)
				if err := performSeamlessMigration(resolvedPath, finalPath); err != nil {
					log.Printf("Migration failed, falling back to JSONL: %v", err)
					storageType = "jsonl"
					finalPath = resolvedPath
				} else {
					log.Printf("Migration completed successfully! Now using SQLite for better performance.")
				}
			}
		}
	}

	// Create storage configuration
	config := storage.Config{
		Type:           storageType,
		FilePath:       finalPath,
		AutoMigrate:    autoMigrate,
		MigrationBatch: 1000,
		WALMode:        true,
		CacheSize:      10000,
		BusyTimeout:    5 * time.Second,
	}

	// Create storage instance
	store, err := storage.NewStorage(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create storage: %w", err)
	}

	// Initialize storage
	if err := store.Initialize(); err != nil {
		return nil, fmt.Errorf("failed to initialize storage: %w", err)
	}

	return &KnowledgeGraphManager{
		storage:    store,
		memoryPath: finalPath,
	}, nil
}

// resolveMemoryPath resolves the memory file path using the same logic as the original
func resolveMemoryPath(memory string) string {
	memoryPath := memory

	// If memory parameter is empty, try environment variable
	if memoryPath == "" {
		memoryPath = os.Getenv("MEMORY_FILE_PATH")

		// If env var is also empty, use default path
		if memoryPath == "" {
			// Default to save in current directory
			execPath, err := os.Executable()
			if err != nil {
				execPath = "."
			}
			memoryPath = filepath.Join(filepath.Dir(execPath), "memory.json")
		}
	}

	// If it's a relative path, use current directory as base
	if !filepath.IsAbs(memoryPath) {
		execPath, err := os.Executable()
		if err != nil {
			execPath = "."
		}
		memoryPath = filepath.Join(filepath.Dir(execPath), memoryPath)
	}

	return memoryPath
}

// detectStorageType auto-detects the storage type and handles seamless migration
func detectStorageType(memoryPath string, autoMigrate bool) (storageType string, finalPath string) {
	ext := strings.ToLower(filepath.Ext(memoryPath))

	// If user specified a SQLite file, use it directly
	if ext == ".db" || ext == ".sqlite" || ext == ".sqlite3" {
		return "sqlite", memoryPath
	}

	// Generate SQLite path from JSONL path
	sqlitePath := strings.TrimSuffix(memoryPath, filepath.Ext(memoryPath)) + ".db"

	// Check if SQLite database already exists
	if _, err := os.Stat(sqlitePath); err == nil {
		log.Printf("Found existing SQLite database: %s", sqlitePath)
		return "sqlite", sqlitePath
	}

	// If auto-migrate is enabled and JSONL file exists, migrate to SQLite
	if autoMigrate {
		if _, err := os.Stat(memoryPath); err == nil {
			log.Printf("Auto-migrating %s to SQLite for better performance...", memoryPath)
			return "sqlite", sqlitePath // Return SQLite path for migration
		}
	}

	// Default to JSONL for new installations or when auto-migrate is disabled
	return "jsonl", memoryPath
}

// performSeamlessMigration performs migration with minimal user disruption
func performSeamlessMigration(jsonlPath, sqlitePath string) error {
	config := storage.Config{MigrationBatch: 1000}
	migrator := storage.NewMigrator(config)

	// Only show important progress, not every step
	migrator.SetProgressCallback(func(current, total int, message string) {
		if current == 30 || current == 90 || current == 100 {
			log.Printf("Migration progress: %s", message)
		}
	})

	result, err := migrator.MigrateJSONLToSQLite(jsonlPath, sqlitePath)
	if err != nil {
		return fmt.Errorf("migration failed: %w", err)
	}

	if result.Success {
		log.Printf("Successfully migrated %d entities and %d relations",
			result.EntitiesCount, result.RelationsCount)
	}

	return nil
}

// Close closes the storage
func (m *KnowledgeGraphManager) Close() error {
	if m.storage != nil {
		return m.storage.Close()
	}
	return nil
}

// CreateEntities creates multiple new entities
func (m *KnowledgeGraphManager) CreateEntities(entities []storage.Entity) ([]storage.Entity, error) {
	return m.storage.CreateEntities(entities)
}

// CreateRelations creates multiple new relations
func (m *KnowledgeGraphManager) CreateRelations(relations []storage.Relation) ([]storage.Relation, error) {
	return m.storage.CreateRelations(relations)
}

// AddObservations adds new observations to existing entities
func (m *KnowledgeGraphManager) AddObservations(additions []ObservationAddition) ([]ObservationAdditionResult, error) {
	// Convert to storage format
	obsMap := make(map[string][]string)
	for _, addition := range additions {
		obsMap[addition.EntityName] = addition.Contents
	}

	// Add observations
	added, err := m.storage.AddObservations(obsMap)
	if err != nil {
		return nil, err
	}

	// Convert back to legacy format
	results := make([]ObservationAdditionResult, 0, len(added))
	for entityName, addedObs := range added {
		results = append(results, ObservationAdditionResult{
			EntityName:        entityName,
			AddedObservations: addedObs,
		})
	}

	return results, nil
}

// DeleteEntities deletes multiple entities and their associated relations
func (m *KnowledgeGraphManager) DeleteEntities(entityNames []string) error {
	return m.storage.DeleteEntities(entityNames)
}

// DeleteObservations deletes specific observations from entities
func (m *KnowledgeGraphManager) DeleteObservations(deletions []storage.ObservationDeletion) error {
	return m.storage.DeleteObservations(deletions)
}

// DeleteRelations deletes multiple relations
func (m *KnowledgeGraphManager) DeleteRelations(relations []storage.Relation) error {
	return m.storage.DeleteRelations(relations)
}

// ReadGraph returns the entire knowledge graph
func (m *KnowledgeGraphManager) ReadGraph() (storage.KnowledgeGraph, error) {
	graph, err := m.storage.ReadGraph()
	if err != nil {
		return storage.KnowledgeGraph{}, err
	}
	return *graph, nil
}

// SearchNodes searches for nodes in the knowledge graph based on a query
func (m *KnowledgeGraphManager) SearchNodes(query string) (storage.KnowledgeGraph, error) {
	graph, err := m.storage.SearchNodes(query)
	if err != nil {
		return storage.KnowledgeGraph{}, err
	}
	return *graph, nil
}

// OpenNodes opens specific nodes in the knowledge graph by their names
func (m *KnowledgeGraphManager) OpenNodes(names []string) (storage.KnowledgeGraph, error) {
	graph, err := m.storage.OpenNodes(names)
	if err != nil {
		return storage.KnowledgeGraph{}, err
	}
	return *graph, nil
}

// Version information
const (
	version = "0.2.1"
	appName = "Memory MCP Server"
)

// printVersion prints version information
func printVersion() {
	fmt.Printf("%s version %s\n", appName, version)
}

// printUsage prints a custom usage message
func printUsage() {
	fmt.Fprintf(os.Stderr, "Usage: %s [options]\n\n", os.Args[0])
	fmt.Fprintf(os.Stderr, "%s is a Model Context Protocol server that provides knowledge graph management capabilities.\n\n", appName)
	fmt.Fprintf(os.Stderr, "Options:\n")
	flag.PrintDefaults()
}

func main() {
	var transport string
	var memory string
	var port int = 8080
	var showVersion bool
	var showHelp bool
	var storageType string
	var autoMigrate bool
	var migrate string
	var migrateTo string
	var dryRun bool
	var force bool

	// Override the default usage message
	flag.Usage = printUsage

	// Define command-line flags
	flag.StringVar(&transport, "transport", "stdio", "Transport type (stdio or sse)")
	flag.StringVar(&transport, "t", "stdio", "Transport type (stdio or sse)")
	flag.StringVar(&memory, "memory", "", "Path to memory file")
	flag.StringVar(&memory, "m", "", "Path to memory file")
	flag.IntVar(&port, "port", 8080, "Port for SSE transport")
	flag.IntVar(&port, "p", 8080, "Port for SSE transport")
	flag.BoolVar(&showVersion, "version", false, "Show version information and exit")
	flag.BoolVar(&showVersion, "v", false, "Show version information and exit")
	flag.BoolVar(&showHelp, "help", false, "Show this help message and exit")
	flag.BoolVar(&showHelp, "h", false, "Show this help message and exit")

	// New storage-related flags
	flag.StringVar(&storageType, "storage", "", "Storage type (sqlite or jsonl, auto-detected if not specified)")
	flag.BoolVar(&autoMigrate, "auto-migrate", true, "Automatically migrate from JSONL to SQLite")
	flag.StringVar(&migrate, "migrate", "", "Migrate data from JSONL file to SQLite")
	flag.StringVar(&migrateTo, "migrate-to", "", "Destination SQLite file for migration")
	flag.BoolVar(&dryRun, "dry-run", false, "Perform a dry run of migration")
	flag.BoolVar(&force, "force", false, "Force overwrite destination file during migration")

	flag.Parse()

	// In stdio mode, ensure logging doesn't interfere with MCP JSON-RPC
	if transport == "stdio" {
		// Set environment variable to track stdio mode for suppressing logs
		os.Setenv("MCP_TRANSPORT", "stdio")
		// Log output already goes to stderr by default, which is fine
		// But we should suppress non-critical logging in stdio mode
		log.SetOutput(os.Stderr)
	}

	// Handle version flag
	if showVersion {
		printVersion()
		os.Exit(0)
	}

	// Handle help flag
	if showHelp {
		printUsage()
		os.Exit(0)
	}

	// Handle migration command
	if migrate != "" {
		if migrateTo == "" {
			migrateTo = strings.TrimSuffix(migrate, filepath.Ext(migrate)) + ".db"
		}

		cmd := storage.MigrateCommand{
			Source:      migrate,
			Destination: migrateTo,
			DryRun:      dryRun,
			Force:       force,
			Verbose:     true,
		}

		if err := storage.ExecuteMigration(cmd); err != nil {
			log.Fatalf("Migration failed: %v", err)
		}

		os.Exit(0)
	}

	// Create knowledge graph manager
	manager, err := NewKnowledgeGraphManager(memory, storageType, autoMigrate)
	if err != nil {
		log.Fatalf("Failed to create knowledge graph manager: %v", err)
	}
	defer manager.Close()

	// Create a new MCP server
	s := server.NewMCPServer(
		appName,
		version,
		server.WithResourceCapabilities(true, true),
		server.WithLogging(),
	)

	// Add create_entities tool
	createEntitiesTool := mcp.NewTool("create_entities",
		mcp.WithDescription("Create multiple new entities in the knowledge graph"),
		mcp.WithArray("entities",
			mcp.Required(),
			mcp.Description("An array of entities to create"),
			mcp.Items(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{
						"type":        "string",
						"description": "The name of the entity",
					},
					"entityType": map[string]any{
						"type":        "string",
						"description": "The type of the entity",
					},
					"observations": map[string]any{
						"type":        "array",
						"description": "An array of observation contents associated with the entity",
						"items": map[string]any{
							"type": "string",
						},
					},
				},
				"required": []string{"name", "entityType", "observations"},
			}),
		),
	)

	// Add create_relations tool
	createRelationsTool := mcp.NewTool("create_relations",
		mcp.WithDescription("Create multiple new relations between entities in the knowledge graph. Relations should be in active voice"),
		mcp.WithArray("relations",
			mcp.Required(),
			mcp.Description("An array of relations to create"),
			mcp.Items(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"from": map[string]any{
						"type":        "string",
						"description": "The name of the entity where the relation starts",
					},
					"to": map[string]any{
						"type":        "string",
						"description": "The name of the entity where the relation ends",
					},
					"relationType": map[string]any{
						"type":        "string",
						"description": "The type of the relation",
					},
				},
				"required": []string{"from", "to", "relationType"},
			}),
		),
	)

	// Add add_observations tool
	addObservationsTool := mcp.NewTool("add_observations",
		mcp.WithDescription("Add new observations to existing entities in the knowledge graph"),
		mcp.WithArray("observations",
			mcp.Required(),
			mcp.Description("An array of observations to add to entities"),
			mcp.Items(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"entityName": map[string]any{
						"type":        "string",
						"description": "The name of the entity to add the observations to",
					},
					"contents": map[string]any{
						"type":        "array",
						"description": "An array of observation contents to add",
						"items": map[string]any{
							"type": "string",
						},
					},
				},
				"required": []string{"entityName", "contents"},
			}),
		),
	)

	// Add delete_entities tool
	deleteEntitiesTool := mcp.NewTool("delete_entities",
		mcp.WithDescription("Delete multiple entities and their associated relations from the knowledge graph"),
		mcp.WithArray("entityNames",
			mcp.Required(),
			mcp.Description("An array of entity names to delete"),
			mcp.Items(map[string]any{
				"type": "string",
			}),
		),
	)

	// Add delete_observations tool
	deleteObservationsTool := mcp.NewTool("delete_observations",
		mcp.WithDescription("Delete specific observations from entities in the knowledge graph"),
		mcp.WithArray("deletions",
			mcp.Required(),
			mcp.Description("An array of observations to delete from entities"),
			mcp.Items(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"entityName": map[string]any{
						"type":        "string",
						"description": "The name of the entity containing the observations",
					},
					"observations": map[string]any{
						"type":        "array",
						"description": "An array of observations to delete",
						"items": map[string]any{
							"type": "string",
						},
					},
				},
				"required": []string{"entityName", "observations"},
			}),
		),
	)

	// Add delete_relations tool
	deleteRelationsTool := mcp.NewTool("delete_relations",
		mcp.WithDescription("Delete multiple relations from the knowledge graph"),
		mcp.WithArray("relations",
			mcp.Required(),
			mcp.Description("An array of relations to delete"),
			mcp.Items(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"from": map[string]any{
						"type":        "string",
						"description": "The name of the entity where the relation starts",
					},
					"to": map[string]any{
						"type":        "string",
						"description": "The name of the entity where the relation ends",
					},
					"relationType": map[string]any{
						"type":        "string",
						"description": "The type of the relation",
					},
				},
				"required": []string{"from", "to", "relationType"},
			}),
		),
	)

	// Add read_graph tool
	readGraphTool := mcp.NewTool("read_graph",
		mcp.WithDescription("Read the entire knowledge graph. WARNING: Can be slow and memory-intensive for large graphs. Consider using search_nodes or open_nodes for specific queries instead. Use this when you need a complete overview or full backup of the graph"),
	)

	// Add search_nodes tool
	searchNodesTool := mcp.NewTool("search_nodes",
		mcp.WithDescription("Search nodes in the knowledge graph. IMPORTANT: Multiple words with spaces become exact phrase search (e.g., 'product idea' only finds exact phrase). For broader results, search single core words separately. Best practice: Split compound queries - instead of '产品idea' search 'product' OR 'idea'; instead of '近视参数' search '近视'; instead of 'user feedback' search 'user' OR 'feedback'"),
		mcp.WithString("query",
			mcp.Required(),
			mcp.Description("Search query. BEHAVIOR: Single word = prefix match (e.g., 'prod' finds product*). Multiple words = exact phrase (e.g., 'product idea' requires both words together). STRATEGY: Use single words for broader results. Examples: '产品' (not '产品idea'), 'idea' (finds idea/ideas), '近视' (not '近视参数'), 'feedback' (not 'user feedback')"),
		),
	)

	// Add open_nodes tool
	openNodesTool := mcp.NewTool("open_nodes",
		mcp.WithDescription("Retrieve specific nodes by exact name match. Use this when you know the precise entity names. For fuzzy/partial matching, use search_nodes instead. Example: open_nodes(['Orchard']) retrieves only the entity named 'Orchard', not 'Orchard Inc' or 'Orchard产品'"),
		mcp.WithArray("names",
			mcp.Required(),
			mcp.Description("Array of exact entity names to retrieve. Must match exactly. For partial matches, use search_nodes first to find the exact names"),
			mcp.Items(map[string]any{
				"type": "string",
			}),
		),
	)

	// Add handlers
	s.AddTool(createEntitiesTool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := request.Params.Arguments["entities"]
		if args == nil {
			return nil, errors.New("missing required parameter: entities")
		}

		// Convert parameters to entity list
		var entities []storage.Entity
		data, err := json.Marshal(args)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &entities); err != nil {
			return nil, err
		}

		// Create entities
		newEntities, err := manager.CreateEntities(entities)
		if err != nil {
			return nil, err
		}

		// Convert result to JSON
		resultJSON, err := json.MarshalIndent(newEntities, "", "  ")
		if err != nil {
			return nil, err
		}

		return mcp.NewToolResultText(string(resultJSON)), nil
	})

	s.AddTool(createRelationsTool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := request.Params.Arguments["relations"]
		if args == nil {
			return nil, errors.New("missing required parameter: relations")
		}

		// Convert parameters to relation list
		var relations []storage.Relation
		data, err := json.Marshal(args)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &relations); err != nil {
			return nil, err
		}

		// Create relations
		newRelations, err := manager.CreateRelations(relations)
		if err != nil {
			return nil, err
		}

		// Convert result to JSON
		resultJSON, err := json.MarshalIndent(newRelations, "", "  ")
		if err != nil {
			return nil, err
		}

		return mcp.NewToolResultText(string(resultJSON)), nil
	})

	s.AddTool(addObservationsTool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := request.Params.Arguments["observations"]
		if args == nil {
			return nil, errors.New("missing required parameter: observations")
		}

		// Convert parameters to observation addition list
		var observations []ObservationAddition
		data, err := json.Marshal(args)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &observations); err != nil {
			return nil, err
		}

		// Add observations
		results, err := manager.AddObservations(observations)
		if err != nil {
			return nil, err
		}

		// Convert result to JSON
		resultJSON, err := json.MarshalIndent(results, "", "  ")
		if err != nil {
			return nil, err
		}

		return mcp.NewToolResultText(string(resultJSON)), nil
	})

	s.AddTool(deleteEntitiesTool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := request.Params.Arguments["entityNames"]
		if args == nil {
			return nil, errors.New("missing required parameter: entityNames")
		}

		// Convert parameters to entity name list
		var entityNames []string
		data, err := json.Marshal(args)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &entityNames); err != nil {
			return nil, err
		}

		// Delete entities
		if err := manager.DeleteEntities(entityNames); err != nil {
			return nil, err
		}

		return mcp.NewToolResultText("Entities deleted successfully"), nil
	})

	s.AddTool(deleteObservationsTool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := request.Params.Arguments["deletions"]
		if args == nil {
			return nil, errors.New("missing required parameter: deletions")
		}

		// Convert parameters to observation deletion list
		var deletions []storage.ObservationDeletion
		data, err := json.Marshal(args)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &deletions); err != nil {
			return nil, err
		}

		// Delete observations
		if err := manager.DeleteObservations(deletions); err != nil {
			return nil, err
		}

		return mcp.NewToolResultText("Observations deleted successfully"), nil
	})

	s.AddTool(deleteRelationsTool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := request.Params.Arguments["relations"]
		if args == nil {
			return nil, errors.New("missing required parameter: relations")
		}

		// Convert parameters to relation list
		var relations []storage.Relation
		data, err := json.Marshal(args)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &relations); err != nil {
			return nil, err
		}

		// Delete relations
		if err := manager.DeleteRelations(relations); err != nil {
			return nil, err
		}

		return mcp.NewToolResultText("Relations deleted successfully"), nil
	})

	s.AddTool(readGraphTool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// Read the entire graph
		graph, err := manager.ReadGraph()
		if err != nil {
			return nil, err
		}

		// Convert result to JSON
		resultJSON, err := json.MarshalIndent(graph, "", "  ")
		if err != nil {
			return nil, err
		}

		return mcp.NewToolResultText(string(resultJSON)), nil
	})

	s.AddTool(searchNodesTool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		query, ok := request.Params.Arguments["query"].(string)
		if !ok {
			return nil, errors.New("missing required parameter: query")
		}

		// Search nodes
		results, err := manager.SearchNodes(query)
		if err != nil {
			return nil, err
		}

		// Convert result to JSON
		resultJSON, err := json.MarshalIndent(results, "", "  ")
		if err != nil {
			return nil, err
		}

		return mcp.NewToolResultText(string(resultJSON)), nil
	})

	s.AddTool(openNodesTool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := request.Params.Arguments["names"]
		if args == nil {
			return nil, errors.New("missing required parameter: names")
		}

		// Convert parameters to name list
		var names []string
		data, err := json.Marshal(args)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &names); err != nil {
			return nil, err
		}

		// Open nodes
		results, err := manager.OpenNodes(names)
		if err != nil {
			return nil, err
		}

		// Convert result to JSON
		resultJSON, err := json.MarshalIndent(results, "", "  ")
		if err != nil {
			return nil, err
		}

		return mcp.NewToolResultText(string(resultJSON)), nil
	})

	switch transport {
	case "stdio":
		fmt.Fprintln(os.Stderr, "Knowledge Graph MCP Server running on stdio")
		if err := server.ServeStdio(s); err != nil {
			fmt.Fprintf(os.Stderr, "Server error: %v\n", err)
		}
	case "sse":
		fmt.Fprintln(os.Stderr, "Knowledge Graph MCP Server running on SSE")
		sseServer := server.NewSSEServer(s, server.WithBaseURL(fmt.Sprintf("http://localhost:%d", port)))
		log.Printf("Server started listening on :%d\n", port)
		if err := sseServer.Start(fmt.Sprintf(":%d", port)); err != nil {
			log.Fatalf("Failed to start server: %v", err)
		}
	default:
		log.Fatalf("Invalid transport: %s", transport)
	}
}
