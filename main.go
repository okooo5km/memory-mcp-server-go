package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
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
var (
	// version can be overridden by -ldflags "-X main.version=..."
	version = "dev"
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
	// HTTP transport options
	var httpEndpoint string
	var httpHeartbeat string
	var httpStateless bool
	// Auth options
	var authBearer string

	// Override the default usage message
	flag.Usage = printUsage

	// Define command-line flags
	flag.StringVar(&transport, "transport", "stdio", "Transport type (stdio, sse, or http)")
	flag.StringVar(&transport, "t", "stdio", "Transport type (stdio, sse, or http)")
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

	// HTTP transport flags
	flag.StringVar(&httpEndpoint, "http-endpoint", "/mcp", "Streamable HTTP endpoint path (e.g. /mcp)")
	flag.StringVar(&httpEndpoint, "http_ep", "/mcp", "Streamable HTTP endpoint path (alias)")
	flag.StringVar(&httpHeartbeat, "http-heartbeat", "30s", "Streamable HTTP heartbeat interval, e.g. 30s, 1m")
	flag.BoolVar(&httpStateless, "http-stateless", false, "Run Streamable HTTP in stateless mode (no session tracking)")

	// Auth flags
	flag.StringVar(&authBearer, "auth-bearer", "", "Require Authorization: Bearer <token> for SSE/HTTP transports")

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
		server.WithRecovery(),
	)

	// Declare sampling capability (optional, harmless if unused)
	s.EnableSampling()

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
		// Bind arguments using new mcp-go helpers
		var arg struct {
			Entities []storage.Entity `json:"entities"`
		}
		if err := request.BindArguments(&arg); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
		if len(arg.Entities) == 0 {
			return nil, errors.New("missing required parameter: entities")
		}

		// Create entities
		newEntities, err := manager.CreateEntities(arg.Entities)
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
		var arg struct {
			Relations []storage.Relation `json:"relations"`
		}
		if err := request.BindArguments(&arg); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
		if len(arg.Relations) == 0 {
			return nil, errors.New("missing required parameter: relations")
		}

		// Create relations
		newRelations, err := manager.CreateRelations(arg.Relations)
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
		var arg struct {
			Observations []ObservationAddition `json:"observations"`
		}
		if err := request.BindArguments(&arg); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
		if len(arg.Observations) == 0 {
			return nil, errors.New("missing required parameter: observations")
		}

		// Add observations
		results, err := manager.AddObservations(arg.Observations)
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
		var arg struct {
			EntityNames []string `json:"entityNames"`
		}
		if err := request.BindArguments(&arg); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
		if len(arg.EntityNames) == 0 {
			return nil, errors.New("missing required parameter: entityNames")
		}

		// Delete entities
		if err := manager.DeleteEntities(arg.EntityNames); err != nil {
			return nil, err
		}

		return mcp.NewToolResultText("Entities deleted successfully"), nil
	})

	s.AddTool(deleteObservationsTool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var arg struct {
			Deletions []storage.ObservationDeletion `json:"deletions"`
		}
		if err := request.BindArguments(&arg); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
		if len(arg.Deletions) == 0 {
			return nil, errors.New("missing required parameter: deletions")
		}

		// Delete observations
		if err := manager.DeleteObservations(arg.Deletions); err != nil {
			return nil, err
		}

		return mcp.NewToolResultText("Observations deleted successfully"), nil
	})

	s.AddTool(deleteRelationsTool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var arg struct {
			Relations []storage.Relation `json:"relations"`
		}
		if err := request.BindArguments(&arg); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
		if len(arg.Relations) == 0 {
			return nil, errors.New("missing required parameter: relations")
		}

		// Delete relations
		if err := manager.DeleteRelations(arg.Relations); err != nil {
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
		query, err := request.RequireString("query")
		if err != nil {
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
		var arg struct {
			Names []string `json:"names"`
		}
		if err := request.BindArguments(&arg); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
		if len(arg.Names) == 0 {
			return nil, errors.New("missing required parameter: names")
		}

		// Open nodes
		results, err := manager.OpenNodes(arg.Names)
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

		// Wrap handlers with optional bearer auth
		authWrap := func(next http.Handler) http.Handler {
			if authBearer == "" {
				return next
			}
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				expected := "Bearer " + authBearer
				if h := strings.TrimSpace(r.Header.Get("Authorization")); h == expected {
					next.ServeHTTP(w, r)
					return
				}
				w.Header().Set("WWW-Authenticate", "Bearer")
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
			})
		}

		mux := http.NewServeMux()
		customSrv := &http.Server{Handler: mux}
		// Build SSE server using custom http.Server so Start() uses our mux
		sseServer := server.NewSSEServer(
			s,
			server.WithBaseURL(fmt.Sprintf("http://localhost:%d", port)),
			server.WithKeepAliveInterval(30*time.Second),
			server.WithHTTPServer(customSrv),
		)
		mux.Handle("/sse", authWrap(sseServer.SSEHandler()))
		mux.Handle("/message", authWrap(sseServer.MessageHandler()))

		log.Printf("SSE listening on :%d\n", port)
		// Start in background and handle graceful shutdown
		errCh := make(chan error, 1)
		go func() { errCh <- sseServer.Start(fmt.Sprintf(":%d", port)) }()
		// Wait for signal or server error
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		select {
		case sig := <-sigCh:
			log.Printf("Received %s, shutting down SSE...", sig)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := sseServer.Shutdown(ctx); err != nil {
				log.Printf("SSE shutdown error: %v", err)
			}
		case err := <-errCh:
			if err != nil {
				log.Fatalf("SSE server error: %v", err)
			}
		}
	case "http", "streamable-http":
		fmt.Fprintln(os.Stderr, "Knowledge Graph MCP Server running on Streamable HTTP")
		// Parse heartbeat duration
		hb := 30 * time.Second
		if d, err := time.ParseDuration(httpHeartbeat); err == nil {
			hb = d
		}
		// Build options (endpointPath not used when mounting with custom mux)
		httpOpts := []server.StreamableHTTPOption{
			server.WithHeartbeatInterval(hb),
		}
		if httpStateless {
			httpOpts = append(httpOpts, server.WithStateLess(true))
		}

		// Auth wrapper
		authWrap := func(next http.Handler) http.Handler {
			if authBearer == "" {
				return next
			}
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				expected := "Bearer " + authBearer
				if h := strings.TrimSpace(r.Header.Get("Authorization")); h == expected {
					next.ServeHTTP(w, r)
					return
				}
				w.Header().Set("WWW-Authenticate", "Bearer")
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
			})
		}

		mux := http.NewServeMux()
		customSrv := &http.Server{Handler: mux}
		streamSrv := server.NewStreamableHTTPServer(s, append(httpOpts, server.WithStreamableHTTPServer(customSrv))...)
		mux.Handle(httpEndpoint, authWrap(streamSrv))

		log.Printf("Streamable HTTP listening on http://localhost:%d%s\n", port, httpEndpoint)

		// Start in background and handle graceful shutdown
		errCh := make(chan error, 1)
		go func() { errCh <- streamSrv.Start(fmt.Sprintf(":%d", port)) }()
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		select {
		case sig := <-sigCh:
			log.Printf("Received %s, shutting down HTTP...", sig)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := streamSrv.Shutdown(ctx); err != nil {
				log.Printf("HTTP shutdown error: %v", err)
			}
		case err := <-errCh:
			if err != nil {
				log.Fatalf("HTTP server error: %v", err)
			}
		}
	default:
		log.Fatalf("Invalid transport: %s", transport)
	}
}
