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

// ReadGraph returns either a summary or full graph based on mode
func (m *KnowledgeGraphManager) ReadGraph(mode string, limit int) (interface{}, error) {
	return m.storage.ReadGraph(mode, limit)
}

// SearchNodes searches for nodes in the knowledge graph and returns lightweight summaries
func (m *KnowledgeGraphManager) SearchNodes(query string, limit int) (storage.SearchResult, error) {
	result, err := m.storage.SearchNodes(query, limit)
	if err != nil {
		return storage.SearchResult{}, err
	}
	return *result, nil
}

// OpenNodes opens specific nodes in the knowledge graph by their names
func (m *KnowledgeGraphManager) OpenNodes(names []string) (storage.KnowledgeGraph, error) {
	graph, err := m.storage.OpenNodes(names)
	if err != nil {
		return storage.KnowledgeGraph{}, err
	}
	return *graph, nil
}

func (m *KnowledgeGraphManager) MergeEntities(sourceName, targetName string) (*storage.MergeResult, error) {
	return m.storage.MergeEntities(sourceName, targetName)
}

func (m *KnowledgeGraphManager) UpdateEntityType(name string, newType string) error {
	return m.storage.UpdateEntityType(name, newType)
}

func (m *KnowledgeGraphManager) UpdateObservation(entityName string, oldContent string, newContent string) error {
	return m.storage.UpdateObservation(entityName, oldContent, newContent)
}

func (m *KnowledgeGraphManager) DetectConflicts(entityName string) ([]storage.Conflict, error) {
	return m.storage.DetectConflicts(entityName)
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
	// CORS options
	var corsOrigin string

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

	// CORS flags
	flag.StringVar(&corsOrigin, "cors-origin", "*", "Allowed CORS origins: '*' for all, or comma-separated list")

	flag.Parse()

	// Parse CORS origins
	var allowedOrigins []string
	allowAllOrigins := corsOrigin == "*"
	if !allowAllOrigins {
		for _, o := range strings.Split(corsOrigin, ",") {
			if o = strings.TrimSpace(o); o != "" {
				allowedOrigins = append(allowedOrigins, o)
			}
		}
	}

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
		server.WithPromptCapabilities(true),
		server.WithLogging(),
		server.WithRecovery(),
	)

	// ─── MCP Resources ─────────────────────────────────────────────────
	// Resources allow AI clients to passively load memory context without
	// explicitly calling tools, improving memory awareness and utilization.

	// Resource: Knowledge Graph Summary
	s.AddResource(mcp.NewResource(
		"memory://graph/summary",
		"Knowledge Graph Summary",
		mcp.WithResourceDescription("Overview of the knowledge graph including entity/relation counts, type distribution, and entity name list. Load this at the start of a conversation to understand what memories are available."),
		mcp.WithMIMEType("application/json"),
	), func(ctx context.Context, request mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
		result, err := manager.ReadGraph("summary", 50)
		if err != nil {
			return nil, fmt.Errorf("failed to read graph summary: %w", err)
		}
		data, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("failed to marshal graph summary: %w", err)
		}
		return []mcp.ResourceContents{
			mcp.TextResourceContents{
				URI:      "memory://graph/summary",
				MIMEType: "application/json",
				Text:     string(data),
			},
		}, nil
	})

	// Resource: Entity type and relation type distribution
	s.AddResource(mcp.NewResource(
		"memory://graph/types",
		"Entity & Relation Types",
		mcp.WithResourceDescription("Lists all entity types and relation types currently in the knowledge graph with their counts. Useful for understanding the schema and maintaining consistent naming when creating new entities."),
		mcp.WithMIMEType("application/json"),
	), func(ctx context.Context, request mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
		result, err := manager.ReadGraph("summary", 1)
		if err != nil {
			return nil, fmt.Errorf("failed to read graph types: %w", err)
		}
		// Extract just the type information from the summary
		summary, ok := result.(*storage.GraphSummary)
		if !ok {
			return nil, fmt.Errorf("unexpected result type from ReadGraph")
		}
		typeInfo := map[string]interface{}{
			"entityTypes":   summary.EntityTypes,
			"relationTypes": summary.RelationTypes,
		}
		data, err := json.MarshalIndent(typeInfo, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("failed to marshal type info: %w", err)
		}
		return []mcp.ResourceContents{
			mcp.TextResourceContents{
				URI:      "memory://graph/types",
				MIMEType: "application/json",
				Text:     string(data),
			},
		}, nil
	})

	// Resource Template: Individual entity details
	s.AddResourceTemplate(mcp.NewResourceTemplate(
		"memory://entities/{name}",
		"Entity Details",
		mcp.WithTemplateDescription("Get full details of a specific entity by name, including all observations and relations. Use the entity name from graph summary or search results."),
		mcp.WithTemplateMIMEType("application/json"),
	), func(ctx context.Context, request mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
		// Extract entity name from URI
		uri := request.Params.URI
		prefix := "memory://entities/"
		if !strings.HasPrefix(uri, prefix) {
			return nil, fmt.Errorf("invalid resource URI: %s", uri)
		}
		name := strings.TrimPrefix(uri, prefix)
		if name == "" {
			return nil, fmt.Errorf("entity name is required")
		}

		// Open the entity
		graph, err := manager.OpenNodes([]string{name})
		if err != nil {
			return nil, fmt.Errorf("failed to open entity %q: %w", name, err)
		}
		data, err := json.MarshalIndent(graph, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("failed to marshal entity data: %w", err)
		}
		return []mcp.ResourceContents{
			mcp.TextResourceContents{
				URI:      uri,
				MIMEType: "application/json",
				Text:     string(data),
			},
		}, nil
	})

	// ─── MCP Prompts ────────────────────────────────────────────────────
	// Prompts provide standardized memory operation templates that appear
	// as clickable actions in clients like Claude Desktop and VS Code.

	// Prompt: Recall memories about a topic
	s.AddPrompt(mcp.NewPrompt("memory-recall",
		mcp.WithPromptDescription("Search and recall relevant memories from the knowledge graph based on a topic or question"),
		mcp.WithArgument("topic",
			mcp.ArgumentDescription("The topic, question, or keywords to search memories for"),
			mcp.RequiredArgument(),
		),
	), func(ctx context.Context, request mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		topic := request.Params.Arguments["topic"]
		results, err := manager.SearchNodes(topic, 10)
		if err != nil {
			return nil, fmt.Errorf("failed to search for topic %q: %w", topic, err)
		}

		var promptText string
		if results.Total == 0 {
			promptText = fmt.Sprintf("I searched the knowledge graph for '%s' but found no matching memories. You may want to create new entities to store information about this topic.", topic)
		} else {
			var entityList strings.Builder
			for i, entity := range results.Entities {
				entityList.WriteString(fmt.Sprintf("\n%d. **%s** (%s) - %d observations, %d relations",
					i+1, entity.Name, entity.EntityType, entity.ObservationsCount, entity.RelationsCount))
				for _, snippet := range entity.Snippets {
					entityList.WriteString(fmt.Sprintf("\n   > %s", snippet))
				}
			}

			// Collect entity names for the open_nodes suggestion
			names := make([]string, len(results.Entities))
			for i, e := range results.Entities {
				names[i] = e.Name
			}

			promptText = fmt.Sprintf(`I searched the knowledge graph for '%s' and found %d matching entities:
%s

Please use the open_nodes tool to retrieve full details for the most relevant entities (suggested names: %s), then summarize the relevant memories for the user.`,
				topic, results.Total, entityList.String(), strings.Join(names, ", "))
		}

		return &mcp.GetPromptResult{
			Description: fmt.Sprintf("Recall memories about: %s", topic),
			Messages: []mcp.PromptMessage{
				{
					Role: mcp.RoleUser,
					Content: mcp.TextContent{
						Type: "text",
						Text: promptText,
					},
				},
			},
		}, nil
	})

	// Prompt: Save conversation information as memories
	s.AddPrompt(mcp.NewPrompt("memory-save",
		mcp.WithPromptDescription("Analyze text and extract entities, relations, and observations to save as memories in the knowledge graph"),
		mcp.WithArgument("content",
			mcp.ArgumentDescription("The text or conversation content to extract memories from"),
			mcp.RequiredArgument(),
		),
	), func(ctx context.Context, request mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		content := request.Params.Arguments["content"]

		promptText := fmt.Sprintf(`Analyze the following content and extract structured knowledge to save in the memory graph.

CONTENT TO ANALYZE:
---
%s
---

INSTRUCTIONS:
1. Identify key entities (people, technologies, projects, concepts, preferences, organizations)
2. For each entity, determine:
   - name: Clear, descriptive name
   - entityType: Category (person, technology, project, concept, preference, organization)
   - observations: Atomic facts about this entity from the content
3. Identify relations between entities (use active voice: "works_on", "uses", "belongs_to", etc.)
4. IMPORTANT: First use search_nodes to check if any of these entities already exist
   - If an entity exists, use add_observations to add new facts
   - If an entity is new, use create_entities to create it
5. Use create_relations to connect related entities

Please proceed to extract and save the memories.`, content)

		return &mcp.GetPromptResult{
			Description: "Extract and save memories from content",
			Messages: []mcp.PromptMessage{
				{
					Role: mcp.RoleUser,
					Content: mcp.TextContent{
						Type: "text",
						Text: promptText,
					},
				},
			},
		}, nil
	})

	// Prompt: Review memories about an entity
	s.AddPrompt(mcp.NewPrompt("memory-review",
		mcp.WithPromptDescription("Review and summarize all stored memories about a specific entity, including its observations and connections"),
		mcp.WithArgument("entity_name",
			mcp.ArgumentDescription("The exact name of the entity to review"),
			mcp.RequiredArgument(),
		),
	), func(ctx context.Context, request mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		entityName := request.Params.Arguments["entity_name"]

		graph, err := manager.OpenNodes([]string{entityName})
		if err != nil {
			return nil, fmt.Errorf("failed to open entity %q: %w", entityName, err)
		}

		if len(graph.Entities) == 0 {
			return &mcp.GetPromptResult{
				Description: fmt.Sprintf("No entity found: %s", entityName),
				Messages: []mcp.PromptMessage{
					{
						Role: mcp.RoleUser,
						Content: mcp.TextContent{
							Type: "text",
							Text: fmt.Sprintf("Entity '%s' was not found in the knowledge graph. Use search_nodes to find similar entity names.", entityName),
						},
					},
				},
			}, nil
		}

		data, err := json.MarshalIndent(graph, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("failed to marshal entity data: %w", err)
		}

		promptText := fmt.Sprintf(`Review the following entity data from the knowledge graph and provide a clear summary:

ENTITY DATA:
%s

Please summarize:
1. What is this entity and what type is it?
2. Key facts (observations) — highlight the most important ones
3. How it connects to other entities (relations)
4. Are there any observations that seem outdated or contradictory?
5. Suggest any missing information that might be worth adding`, string(data))

		return &mcp.GetPromptResult{
			Description: fmt.Sprintf("Review memories about: %s", entityName),
			Messages: []mcp.PromptMessage{
				{
					Role: mcp.RoleUser,
					Content: mcp.TextContent{
						Type: "text",
						Text: promptText,
					},
				},
			},
		}, nil
	})

	// ─── MCP Tools ──────────────────────────────────────────────────────

	// Add create_entities tool
	createEntitiesTool := mcp.NewTool("create_entities",
		mcp.WithDescription(`Create new entities in the knowledge graph. Each entity has a unique name, a type, and observations (atomic facts).

BEFORE CREATING: Use search_nodes to check if the entity already exists. If it does, use add_observations to add new facts instead of creating a duplicate.

NAMING CONVENTIONS:
- Use clear, descriptive names (e.g. "TypeScript", "ProjectAlpha", "JohnDoe")
- entityType should be a lowercase category: "person", "technology", "project", "concept", "preference", "organization", "event", "location"
- Each observation should be a single, atomic fact — not a paragraph

EXAMPLE:
  name: "TypeScript", entityType: "technology"
  observations: ["Preferred language for frontend development", "Used with React in current project"]`),
		mcp.WithTitleAnnotation("Create Entities"),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithArray("entities",
			mcp.Required(),
			mcp.Description("An array of entities to create"),
			mcp.Items(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{
						"type":        "string",
						"description": "Unique name for the entity. Check with search_nodes first to avoid duplicates.",
					},
					"entityType": map[string]any{
						"type":        "string",
						"description": "Category of the entity (e.g. person, technology, project, concept, preference, organization)",
					},
					"observations": map[string]any{
						"type":        "array",
						"description": "Atomic facts about the entity. Each observation should be a single, self-contained statement.",
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
		mcp.WithDescription(`Create directed relations (edges) between existing entities in the knowledge graph.

Relations express how entities are connected. Use active voice for relation types.
Both "from" and "to" entities must already exist — create them first if needed.

RELATION TYPE EXAMPLES:
  "works_on", "uses", "belongs_to", "created_by", "depends_on", "manages", "likes", "knows"

EXAMPLE:
  from: "JohnDoe", to: "ProjectAlpha", relationType: "works_on"
  from: "ProjectAlpha", to: "TypeScript", relationType: "uses"`),
		mcp.WithTitleAnnotation("Create Relations"),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithArray("relations",
			mcp.Required(),
			mcp.Description("An array of relations to create"),
			mcp.Items(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"from": map[string]any{
						"type":        "string",
						"description": "Source entity name (must already exist)",
					},
					"to": map[string]any{
						"type":        "string",
						"description": "Target entity name (must already exist)",
					},
					"relationType": map[string]any{
						"type":        "string",
						"description": "Relation label in active voice (e.g. works_on, uses, belongs_to)",
					},
				},
				"required": []string{"from", "to", "relationType"},
			}),
		),
	)

	// Add add_observations tool
	addObservationsTool := mcp.NewTool("add_observations",
		mcp.WithDescription(`Add new observations (facts) to existing entities in the knowledge graph.

Use this to append new information to entities that already exist. If the entity doesn't exist yet, use create_entities first.

Each observation should be a single, atomic fact. Duplicate observations are automatically skipped.

EXAMPLE:
  entityName: "TypeScript", contents: ["Version 5.0 released in 2023", "Supports decorators natively"]`),
		mcp.WithTitleAnnotation("Add Observations"),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithArray("observations",
			mcp.Required(),
			mcp.Description("An array of observations to add to entities"),
			mcp.Items(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"entityName": map[string]any{
						"type":        "string",
						"description": "Exact name of the existing entity to add observations to",
					},
					"contents": map[string]any{
						"type":        "array",
						"description": "New atomic facts to add. Duplicates are automatically skipped.",
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
		mcp.WithDescription("Delete entities and all their associated observations and relations from the knowledge graph. This action is irreversible."),
		mcp.WithTitleAnnotation("Delete Entities"),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithArray("entityNames",
			mcp.Required(),
			mcp.Description("Exact names of entities to delete. All associated observations and relations will also be removed."),
			mcp.Items(map[string]any{
				"type": "string",
			}),
		),
	)

	// Add delete_observations tool
	deleteObservationsTool := mcp.NewTool("delete_observations",
		mcp.WithDescription("Delete specific observations from entities. Use this to remove outdated or incorrect facts while keeping the entity itself."),
		mcp.WithTitleAnnotation("Delete Observations"),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithArray("deletions",
			mcp.Required(),
			mcp.Description("An array of observations to delete from entities"),
			mcp.Items(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"entityName": map[string]any{
						"type":        "string",
						"description": "Exact name of the entity containing the observations to delete",
					},
					"observations": map[string]any{
						"type":        "array",
						"description": "Exact observation text strings to remove. Must match existing observations exactly.",
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
		mcp.WithDescription("Delete specific relations from the knowledge graph. All three fields (from, to, relationType) must match exactly."),
		mcp.WithTitleAnnotation("Delete Relations"),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithArray("relations",
			mcp.Required(),
			mcp.Description("An array of relations to delete (exact match required for all fields)"),
			mcp.Items(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"from": map[string]any{
						"type":        "string",
						"description": "Source entity name",
					},
					"to": map[string]any{
						"type":        "string",
						"description": "Target entity name",
					},
					"relationType": map[string]any{
						"type":        "string",
						"description": "Relation type to delete",
					},
				},
				"required": []string{"from", "to", "relationType"},
			}),
		),
	)

	// Add read_graph tool
	readGraphTool := mcp.NewTool("read_graph",
		mcp.WithDescription(`Read the knowledge graph to understand what memories are stored.

MODES:
- "summary" (default): Returns statistics (entity/relation counts, type distribution) and a list of entity names. Use this to get an overview of available memories.
- "full": Returns the complete graph with all entities, observations, and relations. Use for backup or comprehensive analysis. Can be large.

RECOMMENDED WORKFLOW: Start with summary mode to see what's available, then use search_nodes for specific topics.`),
		mcp.WithTitleAnnotation("Read Graph"),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithString("mode",
			mcp.Description("'summary' (default): statistics + entity name list; 'full': complete graph export with all data"),
		),
		mcp.WithNumber("limit",
			mcp.Description("Max entity names in summary mode (default: 50, max: 200). Ignored in full mode."),
		),
	)

	// Add search_nodes tool
	searchNodesTool := mcp.NewTool("search_nodes",
		mcp.WithDescription(`Search the knowledge graph for entities matching your query. This should be your FIRST step when looking for stored information.

Returns lightweight results (name, type, matched snippets, counts) — NOT full entity details. Use open_nodes with entity names from results to get complete information.

SEARCH BEHAVIOR:
- Single keyword: "React" matches entities with "React" in name, type, or observations
- Multiple keywords (space-separated OR): "React Vue" finds entities matching EITHER keyword
- Results are ranked: name matches first, then type matches, then observation content matches

WORKFLOW: search_nodes (find relevant entities) → open_nodes (get full details)`),
		mcp.WithTitleAnnotation("Search Nodes"),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithString("query",
			mcp.Required(),
			mcp.Description("Search keywords. Space-separated words are treated as OR search. Matches against entity names, types, and observation content."),
		),
		mcp.WithNumber("limit",
			mcp.Description("Max entities to return. Omit or set to 0 for all matches."),
		),
	)

	// Add open_nodes tool
	openNodesTool := mcp.NewTool("open_nodes",
		mcp.WithDescription(`Get FULL details of specific entities by their exact names.

Returns complete entity data including ALL observations and ALL relations (both incoming and outgoing). Use search_nodes first to find entity names if you're unsure of the exact name.

REQUIRES: Exact entity names (case-sensitive). Get these from search_nodes results.
RETURNS: Complete entities with all observations, plus all relations connected to these entities.`),
		mcp.WithTitleAnnotation("Open Nodes"),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithArray("names",
			mcp.Required(),
			mcp.Description("Exact entity names to retrieve (case-sensitive). Use search_nodes first if unsure."),
			mcp.Items(map[string]any{
				"type": "string",
			}),
		),
	)

	// Add merge_entities tool
	mergeEntitiesTool := mcp.NewTool("merge_entities",
		mcp.WithDescription(`Merge two entities into one. All observations and relations from the source entity are migrated to the target entity, then the source is deleted.

USE WHEN: You discover duplicate entities (e.g. "React.js" and "React" refer to the same thing).

BEHAVIOR:
- Source observations are added to target (duplicates skipped)
- Source relations are redirected to target (duplicates skipped)
- Source entity is deleted after migration

EXAMPLE: sourceName: "React.js", targetName: "React" → merges React.js into React`),
		mcp.WithTitleAnnotation("Merge Entities"),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithString("sourceName",
			mcp.Required(),
			mcp.Description("Entity to merge FROM (will be deleted)"),
		),
		mcp.WithString("targetName",
			mcp.Required(),
			mcp.Description("Entity to merge INTO (will receive observations and relations)"),
		),
	)

	// Add update_entities tool
	updateEntitiesTool := mcp.NewTool("update_entities",
		mcp.WithDescription(`Update the type of an existing entity.

USE WHEN: An entity was created with the wrong type and needs correction.

EXAMPLE: name: "React", entityType: "framework" (was previously "library")`),
		mcp.WithTitleAnnotation("Update Entity Type"),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithString("name",
			mcp.Required(),
			mcp.Description("Exact name of the entity to update"),
		),
		mcp.WithString("entityType",
			mcp.Required(),
			mcp.Description("New entity type to set"),
		),
	)

	// Add update_observations tool
	updateObservationsTool := mcp.NewTool("update_observations",
		mcp.WithDescription(`Replace an existing observation with updated content. Use this to correct outdated or inaccurate facts.

USE WHEN: An observation needs correction (e.g. "Uses React 17" → "Uses React 18").

REQUIRES: The exact old observation text. Use open_nodes first to get the current text.`),
		mcp.WithTitleAnnotation("Update Observation"),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithString("entityName",
			mcp.Required(),
			mcp.Description("Exact name of the entity containing the observation"),
		),
		mcp.WithString("oldContent",
			mcp.Required(),
			mcp.Description("Exact current observation text to replace"),
		),
		mcp.WithString("newContent",
			mcp.Required(),
			mcp.Description("New observation text"),
		),
	)

	// Add detect_conflicts tool
	detectConflictsTool := mcp.NewTool("detect_conflicts",
		mcp.WithDescription(`Detect potential duplicate or contradictory observations within entities.

Analyzes observation pairs for:
- Potential duplicates: observations with high prefix overlap (>60% of words match at start)
- Potential contradictions: observations containing antonym keyword pairs (e.g. "likes X" vs "dislikes X")

USE WHEN: Reviewing memory quality, or after bulk imports to find inconsistencies.

RETURNS: List of conflicts with entity name, both observations, and conflict type.`),
		mcp.WithTitleAnnotation("Detect Conflicts"),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithString("entityName",
			mcp.Description("Optional: check only this entity. Omit to check all entities."),
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
		var arg struct {
			Mode  *string `json:"mode"`
			Limit *int    `json:"limit"`
		}
		if err := request.BindArguments(&arg); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}

		// Default mode is "summary"
		mode := "summary"
		if arg.Mode != nil && *arg.Mode == "full" {
			mode = "full"
		}

		// Apply default and max limits (only relevant for summary mode)
		limit := 50
		if arg.Limit != nil {
			limit = *arg.Limit
			if limit > 200 {
				limit = 200
			}
			if limit < 1 {
				limit = 50
			}
		}

		// Get graph data
		result, err := manager.ReadGraph(mode, limit)
		if err != nil {
			return nil, err
		}

		// Convert result to JSON
		resultJSON, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return nil, err
		}

		return mcp.NewToolResultText(string(resultJSON)), nil
	})

	s.AddTool(searchNodesTool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var arg struct {
			Query string `json:"query"`
			Limit *int   `json:"limit"`
		}
		if err := request.BindArguments(&arg); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
		if arg.Query == "" {
			return nil, errors.New("missing required parameter: query")
		}

		// If limit not specified, use 0 to indicate "all results"
		// If specified, apply reasonable bounds
		limit := 0
		if arg.Limit != nil {
			limit = *arg.Limit
			if limit < 1 {
				limit = 0 // treat invalid as "all"
			}
		}

		// Search nodes
		results, err := manager.SearchNodes(arg.Query, limit)
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

	s.AddTool(mergeEntitiesTool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var arg struct {
			SourceName string `json:"sourceName"`
			TargetName string `json:"targetName"`
		}
		if err := request.BindArguments(&arg); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
		if arg.SourceName == "" || arg.TargetName == "" {
			return nil, errors.New("missing required parameters: sourceName and targetName")
		}

		result, err := manager.MergeEntities(arg.SourceName, arg.TargetName)
		if err != nil {
			return nil, err
		}

		resultJSON, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return nil, err
		}
		return mcp.NewToolResultText(string(resultJSON)), nil
	})

	s.AddTool(updateEntitiesTool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var arg struct {
			Name       string `json:"name"`
			EntityType string `json:"entityType"`
		}
		if err := request.BindArguments(&arg); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
		if arg.Name == "" || arg.EntityType == "" {
			return nil, errors.New("missing required parameters: name and entityType")
		}

		if err := manager.UpdateEntityType(arg.Name, arg.EntityType); err != nil {
			return nil, err
		}
		return mcp.NewToolResultText(fmt.Sprintf("Entity %q type updated to %q", arg.Name, arg.EntityType)), nil
	})

	s.AddTool(updateObservationsTool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var arg struct {
			EntityName string `json:"entityName"`
			OldContent string `json:"oldContent"`
			NewContent string `json:"newContent"`
		}
		if err := request.BindArguments(&arg); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
		if arg.EntityName == "" || arg.OldContent == "" || arg.NewContent == "" {
			return nil, errors.New("missing required parameters: entityName, oldContent, and newContent")
		}

		if err := manager.UpdateObservation(arg.EntityName, arg.OldContent, arg.NewContent); err != nil {
			return nil, err
		}
		return mcp.NewToolResultText("Observation updated successfully"), nil
	})

	s.AddTool(detectConflictsTool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var arg struct {
			EntityName *string `json:"entityName"`
		}
		if err := request.BindArguments(&arg); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}

		entityName := ""
		if arg.EntityName != nil {
			entityName = *arg.EntityName
		}

		conflicts, err := manager.DetectConflicts(entityName)
		if err != nil {
			return nil, err
		}

		if len(conflicts) == 0 {
			return mcp.NewToolResultText("No conflicts detected"), nil
		}

		resultJSON, err := json.MarshalIndent(conflicts, "", "  ")
		if err != nil {
			return nil, err
		}
		return mcp.NewToolResultText(string(resultJSON)), nil
	})

	// Shared auth middleware for SSE/HTTP transports
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

	// Shared CORS middleware for SSE/HTTP transports
	corsWrap := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")

			// Origin validation (MCP spec): reject disallowed origins
			if origin != "" && !allowAllOrigins {
				allowed := false
				for _, o := range allowedOrigins {
					if o == origin {
						allowed = true
						break
					}
				}
				if !allowed {
					http.Error(w, "Forbidden", http.StatusForbidden)
					return
				}
			}

			// Set CORS headers
			if allowAllOrigins {
				w.Header().Set("Access-Control-Allow-Origin", "*")
			} else if origin != "" {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Add("Vary", "Origin")
			}
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Mcp-Session-Id, Mcp-Protocol-Version, Authorization, Last-Event-ID")
			w.Header().Set("Access-Control-Expose-Headers", "Mcp-Session-Id")
			w.Header().Set("Access-Control-Max-Age", "86400")

			// Handle OPTIONS preflight
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}

	switch transport {
	case "stdio":
		fmt.Fprintln(os.Stderr, "Knowledge Graph MCP Server running on stdio")
		if err := server.ServeStdio(s); err != nil {
			fmt.Fprintf(os.Stderr, "Server error: %v\n", err)
		}
	case "sse":
		fmt.Fprintln(os.Stderr, "Knowledge Graph MCP Server running on SSE")

		mux := http.NewServeMux()
		customSrv := &http.Server{Handler: mux}
		// Build SSE server using custom http.Server so Start() uses our mux
		sseServer := server.NewSSEServer(
			s,
			server.WithBaseURL(fmt.Sprintf("http://localhost:%d", port)),
			server.WithKeepAliveInterval(30*time.Second),
			server.WithHTTPServer(customSrv),
		)
		mux.Handle("/sse", corsWrap(authWrap(sseServer.SSEHandler())))
		mux.Handle("/message", corsWrap(authWrap(sseServer.MessageHandler())))

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

		mux := http.NewServeMux()
		customSrv := &http.Server{Handler: mux}
		streamSrv := server.NewStreamableHTTPServer(s, append(httpOpts, server.WithStreamableHTTPServer(customSrv))...)
		mux.Handle(httpEndpoint, corsWrap(authWrap(streamSrv)))

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
