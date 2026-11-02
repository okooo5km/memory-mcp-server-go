package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"memory-mcp-server-go/auth"
	"memory-mcp-server-go/storage"

	// Pure Go SQLite driver
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
		obsMap[addition.EntityName] = append(obsMap[addition.EntityName], addition.Contents...)
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

func formatListenAddr(host string, port int) string {
	if host == "" {
		return fmt.Sprintf(":%d", port)
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

func main() {
	var fv flagValues
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

	// Core flags
	transportHelp := "Transport type: stdio, http (alias streamable-http) or sse (env: MMS_TRANSPORT; defaults to http instead of stdio when a public URL is set)"
	flag.StringVar(&fv.Transport, "transport", "stdio", transportHelp)
	flag.StringVar(&fv.Transport, "t", "stdio", transportHelp)
	flag.StringVar(&fv.Memory, "memory", "", "Path to memory file")
	flag.StringVar(&fv.Memory, "m", "", "Path to memory file")
	portHelp := "Port for the http and sse transports (env: MMS_PORT)"
	flag.IntVar(&fv.Port, "port", 8080, portHelp)
	flag.IntVar(&fv.Port, "p", 8080, portHelp)
	flag.StringVar(&fv.Host, "host", "", "Host/IP to bind for http/sse transports (env: MMS_HOST; default all interfaces, or 127.0.0.1 when a public URL is set)")
	flag.BoolVar(&showVersion, "version", false, "Show version information and exit")
	flag.BoolVar(&showVersion, "v", false, "Show version information and exit")
	flag.BoolVar(&showHelp, "help", false, "Show this help message and exit")
	flag.BoolVar(&showHelp, "h", false, "Show this help message and exit")

	// Storage-related flags
	flag.StringVar(&storageType, "storage", "", "Storage type (sqlite or jsonl, auto-detected if not specified)")
	flag.BoolVar(&autoMigrate, "auto-migrate", true, "Automatically migrate from JSONL to SQLite")
	flag.StringVar(&migrate, "migrate", "", "Migrate data from JSONL file to SQLite")
	flag.StringVar(&migrateTo, "migrate-to", "", "Destination SQLite file for migration")
	flag.BoolVar(&dryRun, "dry-run", false, "Perform a dry run of migration")
	flag.BoolVar(&force, "force", false, "Force overwrite destination file during migration")

	// HTTP transport flags
	flag.StringVar(&fv.HTTPEndpoint, "http-endpoint", "/mcp", "Streamable HTTP endpoint path (env: MMS_HTTP_ENDPOINT)")
	flag.StringVar(&fv.HTTPEndpoint, "http_ep", "/mcp", "Streamable HTTP endpoint path (alias)")
	flag.StringVar(&fv.Heartbeat, "http-heartbeat", "0", "Interval of server-to-client keepalive pings, e.g. 30s, 1m; 0 (default) disables. Only enable for clients that keep the standalone GET stream open: the SDK closes the session when a ping goes unanswered")
	flag.BoolVar(&fv.Stateless, "http-stateless", false, "Run Streamable HTTP in stateless mode (no session tracking)")

	// Auth flags
	flag.StringVar(&fv.AuthBearer, "auth-bearer", "", "Require Authorization: Bearer <token> for http/sse transports (env: MMS_AUTH_BEARER)")
	flag.StringVar(&fv.PublicURL, "public-url", "", "Public base URL, e.g. https://mcp.example.com (env: MMS_PUBLIC_URL). Sets the OAuth issuer and resource, and defaults to http transport on 127.0.0.1 with the OAuth store next to the memory database")
	flag.StringVar(&fv.OAuthUser, "oauth-user", "", "OAuth login username (env: OAUTH_USER)")
	flag.StringVar(&fv.OAuthPass, "oauth-pass", "", "OAuth login password (env: OAUTH_PASS; prefer the environment, argv is visible in the process list)")
	flag.StringVar(&fv.OAuthIssuer, "oauth-issuer", "", "Legacy: set only the OAuth issuer, with no other --public-url effects (env: OAUTH_ISSUER)")
	flag.StringVar(&fv.OAuthStore, "oauth-store", "", "SQLite path for durable OAuth clients and tokens (env: OAUTH_STORE_PATH)")
	flag.StringVar(&fv.OAuthRefreshReplayGrace, "oauth-refresh-replay-grace", "", "Duration to accept replay of a rotated refresh token (env: OAUTH_REFRESH_REPLAY_GRACE, default 75m)")

	// CORS flags
	flag.StringVar(&fv.CORSOrigin, "cors-origin", "*", "Allowed CORS origins: '*' for all, or comma-separated list (env: MMS_CORS_ORIGIN)")

	flag.StringVar(&fv.TrustedProxies, "trusted-proxies", "", "Comma-separated CIDRs or IPs of reverse proxies whose X-Real-IP header is trusted for rate limiting; loopback is always trusted (env: MMS_TRUSTED_PROXIES)")

	flag.Parse()

	if showVersion {
		printVersion()
		os.Exit(0)
	}
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

	cfg, err := resolveConfig(explicitFlags(flag.CommandLine), os.Getenv, fv)
	if err != nil {
		log.Fatalf("Invalid configuration: %v", err)
	}

	// Logs go to stderr only; in stdio mode stdout carries JSON-RPC.
	log.SetOutput(os.Stderr)

	manager, err := NewKnowledgeGraphManager(cfg.Memory, storageType, autoMigrate)
	if err != nil {
		log.Fatalf("Failed to create knowledge graph manager: %v", err)
	}
	defer manager.Close()

	cfg.setDefaultOAuthStore(manager.memoryPath)
	for _, w := range cfg.Warnings() {
		log.Printf("WARNING: %s", w)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg, manager); err != nil {
		manager.Close()
		log.Fatalf("Server error: %v", err)
	}
}

// run serves the MCP server over the configured transport until ctx is done.
func run(ctx context.Context, cfg *appConfig, manager *KnowledgeGraphManager) error {
	keepAlive := cfg.Heartbeat
	if cfg.Stateless {
		keepAlive = 0 // per-request sessions: nothing to ping
	}
	if cfg.Transport == "stdio" {
		keepAlive = 0 // pings are an HTTP concern
	}
	s := newMCPServerWithKeepAlive(manager, keepAlive)

	if cfg.Transport == "stdio" {
		fmt.Fprintln(os.Stderr, "Knowledge Graph MCP Server running on stdio")
		err := s.Run(ctx, &mcp.StdioTransport{})
		if err != nil && ctx.Err() == nil {
			return err
		}
		return nil
	}

	var oauthSrv *auth.OAuthServer
	if cfg.OAuthEnabled() {
		var err error
		oauthSrv, err = auth.NewOAuthServerWithError(auth.Config{
			Username:           cfg.OAuthUser,
			Password:           cfg.OAuthPass,
			Issuer:             cfg.Issuer,
			ResourcePath:       cfg.MCPPath(),
			StorePath:          cfg.OAuthStore,
			RefreshReplayGrace: cfg.RefreshReplayGrace,
			TrustedProxies:     cfg.TrustedProxies,
		})
		if err != nil {
			return err
		}
		defer oauthSrv.Close()
	}

	handler, err := newHTTPHandler(httpConfig{
		Server:     s,
		Transport:  cfg.Transport,
		Endpoint:   cfg.HTTPEndpoint,
		Stateless:  cfg.Stateless,
		PublicURL:  cfg.PublicURL,
		OAuth:      oauthSrv,
		AuthBearer: cfg.AuthBearer,
		CORSOrigin: cfg.CORSOrigin,
	})
	if err != nil {
		return err
	}

	listenAddr := formatListenAddr(cfg.Host, cfg.Port)
	label := "Streamable HTTP"
	if cfg.Transport == "sse" {
		label = "SSE"
	}
	fmt.Fprintf(os.Stderr, "Knowledge Graph MCP Server running on %s\n", label)
	log.Printf("%s listening on %s%s", label, listenAddr, cfg.MCPPath())
	return serveHTTP(ctx, listenAddr, handler)
}
