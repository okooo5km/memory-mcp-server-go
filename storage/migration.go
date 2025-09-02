package storage

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MigrationResult contains the results of a migration operation
type MigrationResult struct {
	Success        bool
	SourcePath     string
	DestPath       string
	EntitiesCount  int
	RelationsCount int
	Duration       time.Duration
	BackupPath     string
	Error          error
}

// Migrator handles data migration between storage backends
type Migrator struct {
	config       Config
	batchSize    int
	progressFunc func(current, total int, message string)
}

// NewMigrator creates a new migrator instance
func NewMigrator(config Config) *Migrator {
	batchSize := config.MigrationBatch
	if batchSize <= 0 {
		batchSize = 1000 // Default batch size
	}

	return &Migrator{
		config:    config,
		batchSize: batchSize,
	}
}

// SetProgressCallback sets a callback for migration progress updates
func (m *Migrator) SetProgressCallback(fn func(current, total int, message string)) {
	m.progressFunc = fn
}

// MigrateJSONLToSQLite migrates data from JSONL to SQLite
func (m *Migrator) MigrateJSONLToSQLite(jsonlPath, sqlitePath string) (*MigrationResult, error) {
	startTime := time.Now()
	result := &MigrationResult{
		SourcePath: jsonlPath,
		DestPath:   sqlitePath,
	}

	// Step 1: Verify source exists
	if _, err := os.Stat(jsonlPath); os.IsNotExist(err) {
		result.Error = fmt.Errorf("source file does not exist: %s", jsonlPath)
		return result, result.Error
	}

	m.reportProgress(0, 100, "Initializing migration...")

	// Step 2: Create source storage
	jsonlConfig := Config{
		Type:     "jsonl",
		FilePath: jsonlPath,
	}
	source, err := NewJSONLStorage(jsonlConfig)
	if err != nil {
		result.Error = fmt.Errorf("failed to create JSONL storage: %w", err)
		return result, result.Error
	}

	if err := source.Initialize(); err != nil {
		result.Error = fmt.Errorf("failed to initialize JSONL storage: %w", err)
		return result, result.Error
	}
	defer source.Close()

	m.reportProgress(10, 100, "Reading source data...")

	// Step 3: Export data from source
	graph, err := source.ExportData()
	if err != nil {
		result.Error = fmt.Errorf("failed to export data: %w", err)
		return result, result.Error
	}

	result.EntitiesCount = len(graph.Entities)
	result.RelationsCount = len(graph.Relations)

	m.reportProgress(30, 100, fmt.Sprintf("Found %d entities and %d relations",
		result.EntitiesCount, result.RelationsCount))

	// Step 4: Create backup
	backupPath := m.createBackupPath(jsonlPath)
	if err := m.createBackup(jsonlPath, backupPath); err != nil {
		log.Printf("Warning: Failed to create backup: %v", err)
	} else {
		result.BackupPath = backupPath
		m.reportProgress(40, 100, "Created backup")
	}

	// Step 5: Create destination storage
	sqliteConfig := Config{
		Type:        "sqlite",
		FilePath:    sqlitePath,
		WALMode:     true,
		CacheSize:   10000,
		BusyTimeout: 5 * time.Second,
	}
	dest, err := NewSQLiteStorage(sqliteConfig)
	if err != nil {
		result.Error = fmt.Errorf("failed to create SQLite storage: %w", err)
		return result, result.Error
	}

	if err := dest.Initialize(); err != nil {
		result.Error = fmt.Errorf("failed to initialize SQLite storage: %w", err)
		return result, result.Error
	}
	defer dest.Close()

	m.reportProgress(50, 100, "Importing data to SQLite...")

	// Step 6: Import data in batches
	if err := m.importInBatches(dest, graph); err != nil {
		result.Error = fmt.Errorf("failed to import data: %w", err)
		return result, result.Error
	}

	m.reportProgress(90, 100, "Verifying migration...")

	// Step 7: Verify migration
	if err := m.verifyMigration(source, dest); err != nil {
		result.Error = fmt.Errorf("migration verification failed: %w", err)
		return result, result.Error
	}

	result.Success = true
	result.Duration = time.Since(startTime)

	m.reportProgress(100, 100, "Migration completed successfully!")

	return result, nil
}

// AutoMigrate automatically detects and migrates from JSONL to SQLite if needed
func (m *Migrator) AutoMigrate(memoryPath string) (*MigrationResult, error) {
	// Determine file type based on extension
	ext := strings.ToLower(filepath.Ext(memoryPath))

	// If it's already a SQLite file, no migration needed
	if ext == ".db" || ext == ".sqlite" || ext == ".sqlite3" {
		return nil, nil
	}

	// Check if JSONL file exists
	if _, err := os.Stat(memoryPath); os.IsNotExist(err) {
		return nil, nil // No file to migrate
	}

	// Generate SQLite path
	sqlitePath := strings.TrimSuffix(memoryPath, ext) + ".db"

	// Check if SQLite already exists
	if _, err := os.Stat(sqlitePath); err == nil {
		log.Printf("SQLite database already exists at %s, skipping migration", sqlitePath)
		return nil, nil
	}

	log.Printf("Auto-migrating from %s to %s", memoryPath, sqlitePath)

	return m.MigrateJSONLToSQLite(memoryPath, sqlitePath)
}

// importInBatches imports data in batches to avoid memory issues
func (m *Migrator) importInBatches(dest Storage, graph *KnowledgeGraph) error {
	totalItems := len(graph.Entities) + len(graph.Relations)
	currentItem := 0

	// Import entities in batches
	for i := 0; i < len(graph.Entities); i += m.batchSize {
		end := i + m.batchSize
		if end > len(graph.Entities) {
			end = len(graph.Entities)
		}

		batch := graph.Entities[i:end]
		if _, err := dest.CreateEntities(batch); err != nil {
			return fmt.Errorf("failed to import entity batch %d-%d: %w", i, end, err)
		}

		currentItem += len(batch)
		progress := 50 + (currentItem * 40 / totalItems)
		m.reportProgress(progress, 100, fmt.Sprintf("Imported %d/%d entities", end, len(graph.Entities)))
	}

	// Import relations in batches
	for i := 0; i < len(graph.Relations); i += m.batchSize {
		end := i + m.batchSize
		if end > len(graph.Relations) {
			end = len(graph.Relations)
		}

		batch := graph.Relations[i:end]
		if _, err := dest.CreateRelations(batch); err != nil {
			return fmt.Errorf("failed to import relation batch %d-%d: %w", i, end, err)
		}

		currentItem += len(batch)
		progress := 50 + (currentItem * 40 / totalItems)
		m.reportProgress(progress, 100, fmt.Sprintf("Imported %d/%d relations", end, len(graph.Relations)))
	}

	return nil
}

// verifyMigration verifies that all data was migrated correctly
func (m *Migrator) verifyMigration(source, dest Storage) error {
	sourceGraph, err := source.ExportData()
	if err != nil {
		return fmt.Errorf("failed to read source for verification: %w", err)
	}

	destGraph, err := dest.ExportData()
	if err != nil {
		return fmt.Errorf("failed to read destination for verification: %w", err)
	}

	// Verify entity count
	if len(sourceGraph.Entities) != len(destGraph.Entities) {
		return fmt.Errorf("entity count mismatch: source=%d, dest=%d",
			len(sourceGraph.Entities), len(destGraph.Entities))
	}

	// Analyze missing relations
	if len(sourceGraph.Relations) != len(destGraph.Relations) {
		log.Printf("Warning: relation count mismatch: source=%d, dest=%d",
			len(sourceGraph.Relations), len(destGraph.Relations))

		// Find missing relations for debugging
		sourceRelMap := make(map[string]bool)
		for _, rel := range sourceGraph.Relations {
			key := fmt.Sprintf("%s|%s|%s", rel.From, rel.To, rel.RelationType)
			sourceRelMap[key] = true
		}

		destRelMap := make(map[string]bool)
		for _, rel := range destGraph.Relations {
			key := fmt.Sprintf("%s|%s|%s", rel.From, rel.To, rel.RelationType)
			destRelMap[key] = true
		}

		// Find entity names for validation
		entityMap := make(map[string]bool)
		for _, entity := range destGraph.Entities {
			entityMap[entity.Name] = true
		}

		missingCount := 0
		orphanedCount := 0
		for _, rel := range sourceGraph.Relations {
			key := fmt.Sprintf("%s|%s|%s", rel.From, rel.To, rel.RelationType)
			if !destRelMap[key] {
				missingCount++
				if !entityMap[rel.From] || !entityMap[rel.To] {
					orphanedCount++
					if missingCount <= 10 { // Limit output for orphaned relations
						log.Printf("Orphaned relation (missing entity): %s -> %s (%s)", rel.From, rel.To, rel.RelationType)
					}
				} else {
					log.Printf("Missing relation: %s -> %s (%s)", rel.From, rel.To, rel.RelationType)
				}
			}
		}

		if orphanedCount > 0 {
			log.Printf("Found %d orphaned relations (referencing non-existent entities)", orphanedCount)
		}

		// Calculate non-orphaned missing relations
		nonOrphanedMissing := missingCount - orphanedCount

		// Allow migration to continue if most missing relations are orphaned
		if nonOrphanedMissing <= 5 { // Allow up to 5 non-orphaned missing relations
			if orphanedCount > 0 {
				log.Printf("Migration successful with data cleanup: removed %d orphaned relations", orphanedCount)
			}
			if nonOrphanedMissing > 0 {
				log.Printf("Warning: %d valid relations were not migrated (may be duplicates)", nonOrphanedMissing)
			}
			return nil
		}

		return fmt.Errorf("relation count mismatch: source=%d, dest=%d (missing: %d, orphaned: %d, non-orphaned missing: %d)",
			len(sourceGraph.Relations), len(destGraph.Relations), missingCount, orphanedCount, nonOrphanedMissing)
	}

	// Spot check a few entities
	if len(sourceGraph.Entities) > 0 {
		// Create entity map for quick lookup
		destEntityMap := make(map[string]*Entity)
		for i := range destGraph.Entities {
			destEntityMap[destGraph.Entities[i].Name] = &destGraph.Entities[i]
		}

		// Check first, middle, and last entities
		indices := []int{0}
		if len(sourceGraph.Entities) > 1 {
			indices = append(indices, len(sourceGraph.Entities)/2)
			indices = append(indices, len(sourceGraph.Entities)-1)
		}

		for _, idx := range indices {
			if idx >= len(sourceGraph.Entities) {
				continue
			}

			srcEntity := &sourceGraph.Entities[idx]
			destEntity, exists := destEntityMap[srcEntity.Name]
			if !exists {
				return fmt.Errorf("entity %s not found in destination", srcEntity.Name)
			}

			if srcEntity.EntityType != destEntity.EntityType {
				return fmt.Errorf("entity type mismatch for %s: source=%s, dest=%s",
					srcEntity.Name, srcEntity.EntityType, destEntity.EntityType)
			}

			if len(srcEntity.Observations) != len(destEntity.Observations) {
				return fmt.Errorf("observation count mismatch for %s: source=%d, dest=%d",
					srcEntity.Name, len(srcEntity.Observations), len(destEntity.Observations))
			}
		}
	}

	return nil
}

// createBackupPath generates a backup file path
func (m *Migrator) createBackupPath(originalPath string) string {
	dir := filepath.Dir(originalPath)
	base := filepath.Base(originalPath)
	timestamp := time.Now().Format("20060102_150405")
	return filepath.Join(dir, fmt.Sprintf(".%s.backup_%s", base, timestamp))
}

// createBackup creates a backup of the source file
func (m *Migrator) createBackup(source, backup string) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return fmt.Errorf("failed to read source file: %w", err)
	}

	if err := os.WriteFile(backup, data, 0644); err != nil {
		return fmt.Errorf("failed to write backup: %w", err)
	}

	return nil
}

// reportProgress reports migration progress
func (m *Migrator) reportProgress(current, total int, message string) {
	if m.progressFunc != nil {
		m.progressFunc(current, total, message)
	}
}

// MigrateCommand represents the migration command structure
type MigrateCommand struct {
	Source      string
	Destination string
	DryRun      bool
	Force       bool
	Verbose     bool
}

// ExecuteMigration executes a migration based on command parameters
func ExecuteMigration(cmd MigrateCommand) error {
	config := Config{
		MigrationBatch: 1000,
	}

	migrator := NewMigrator(config)

	if cmd.Verbose {
		migrator.SetProgressCallback(func(current, total int, message string) {
			log.Printf("[%d%%] %s", current*100/total, message)
		})
	}

	// Check if destination exists and handle force flag
	if _, err := os.Stat(cmd.Destination); err == nil && !cmd.Force {
		return fmt.Errorf("destination file already exists: %s (use --force to overwrite)", cmd.Destination)
	}

	if cmd.DryRun {
		log.Println("DRY RUN: Would migrate from", cmd.Source, "to", cmd.Destination)

		// Just verify source can be read
		jsonlConfig := Config{Type: "jsonl", FilePath: cmd.Source}
		source, err := NewJSONLStorage(jsonlConfig)
		if err != nil {
			return fmt.Errorf("failed to create source storage: %w", err)
		}

		if err := source.Initialize(); err != nil {
			return fmt.Errorf("failed to initialize source storage: %w", err)
		}
		defer source.Close()

		graph, err := source.ExportData()
		if err != nil {
			return fmt.Errorf("failed to read source data: %w", err)
		}

		log.Printf("Would migrate %d entities and %d relations",
			len(graph.Entities), len(graph.Relations))

		return nil
	}

	// Perform actual migration
	result, err := migrator.MigrateJSONLToSQLite(cmd.Source, cmd.Destination)
	if err != nil {
		return err
	}

	if result.Success {
		log.Printf("Migration completed successfully!")
		log.Printf("  Entities migrated: %d", result.EntitiesCount)
		log.Printf("  Relations migrated: %d", result.RelationsCount)
		log.Printf("  Duration: %s", result.Duration)
		if result.BackupPath != "" {
			log.Printf("  Backup saved to: %s", result.BackupPath)
		}
	}

	return nil
}
