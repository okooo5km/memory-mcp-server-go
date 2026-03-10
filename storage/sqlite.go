package storage

import (
	"database/sql"
	"fmt"
	"strings"

	_ "modernc.org/sqlite"
)

// SQLiteStorage implements Storage interface using SQLite
type SQLiteStorage struct {
	db     *sql.DB // write connection (single conn)
	dbRead *sql.DB // read connection pool (multiple conns)
	config Config
}

// NewSQLiteStorage creates a new SQLite storage instance
func NewSQLiteStorage(config Config) (*SQLiteStorage, error) {
	s := &SQLiteStorage{config: config}
	return s, nil
}

// Initialize sets up the SQLite database
func (s *SQLiteStorage) Initialize() error {
	var err error
	s.db, err = sql.Open("sqlite", s.config.FilePath)
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}

	// Configure SQLite for better performance
	if s.config.WALMode {
		_, err = s.db.Exec("PRAGMA journal_mode=WAL")
		if err != nil {
			return fmt.Errorf("failed to enable WAL mode: %w", err)
		}
	}

	if s.config.CacheSize > 0 {
		_, err = s.db.Exec(fmt.Sprintf("PRAGMA cache_size=%d", s.config.CacheSize))
		if err != nil {
			return fmt.Errorf("failed to set cache size: %w", err)
		}
	}

	if s.config.BusyTimeout > 0 {
		_, err = s.db.Exec(fmt.Sprintf("PRAGMA busy_timeout=%d", s.config.BusyTimeout.Milliseconds()))
		if err != nil {
			return fmt.Errorf("failed to set busy timeout: %w", err)
		}
	}

	// Limit write connection to 1 (SQLite serializes writes anyway)
	s.db.SetMaxOpenConns(1)

	// Create schema
	if err = s.createSchema(); err != nil {
		return fmt.Errorf("failed to create schema: %w", err)
	}

	// Run schema migrations for new columns
	if err = s.migrateSchema(); err != nil {
		return fmt.Errorf("failed to migrate schema: %w", err)
	}

	// Try to create FTS schema (optional, will fallback to regular search if it fails)
	if err = s.createFTSSchema(); err != nil {
		// Log warning but don't fail initialization
		// Silently fallback - don't print to stdout in MCP mode
		// FTS5 is optional, basic search will work fine
	}

	// Open a separate read connection pool to leverage WAL concurrency
	s.dbRead, err = sql.Open("sqlite", s.config.FilePath)
	if err != nil {
		return fmt.Errorf("failed to open read database: %w", err)
	}
	s.dbRead.SetMaxOpenConns(4) // Allow concurrent reads

	// Configure read connection with same pragmas (minus WAL which is db-level)
	if s.config.CacheSize > 0 {
		s.dbRead.Exec(fmt.Sprintf("PRAGMA cache_size=%d", s.config.CacheSize))
	}
	if s.config.BusyTimeout > 0 {
		s.dbRead.Exec(fmt.Sprintf("PRAGMA busy_timeout=%d", s.config.BusyTimeout.Milliseconds()))
	}
	// Mark read connections as query-only for safety
	s.dbRead.Exec("PRAGMA query_only=ON")

	return nil
}

// createSchema creates the database schema
func (s *SQLiteStorage) createSchema() error {
	schema := `
	-- Entities table
	CREATE TABLE IF NOT EXISTS entities (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE,
		entity_type TEXT NOT NULL,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_entities_type ON entities(entity_type);
	
	-- Observations table
	CREATE TABLE IF NOT EXISTS observations (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		entity_id INTEGER NOT NULL,
		content TEXT NOT NULL,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		FOREIGN KEY (entity_id) REFERENCES entities(id) ON DELETE CASCADE,
		UNIQUE(entity_id, content)
	);
	CREATE INDEX IF NOT EXISTS idx_observations_entity ON observations(entity_id);
	
	-- Relations table
	CREATE TABLE IF NOT EXISTS relations (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		from_entity_id INTEGER NOT NULL,
		to_entity_id INTEGER NOT NULL,
		relation_type TEXT NOT NULL,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		FOREIGN KEY (from_entity_id) REFERENCES entities(id) ON DELETE CASCADE,
		FOREIGN KEY (to_entity_id) REFERENCES entities(id) ON DELETE CASCADE,
		UNIQUE(from_entity_id, to_entity_id, relation_type)
	);
	CREATE INDEX IF NOT EXISTS idx_relations_from ON relations(from_entity_id);
	CREATE INDEX IF NOT EXISTS idx_relations_to ON relations(to_entity_id);
	CREATE INDEX IF NOT EXISTS idx_relations_type ON relations(relation_type);
	
	-- Metadata table
	CREATE TABLE IF NOT EXISTS metadata (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL
	);
	
	-- Insert schema version
	INSERT OR IGNORE INTO metadata (key, value) VALUES ('schema_version', '1.0');
	`

	_, err := s.db.Exec(schema)
	return err
}

// migrateSchema adds new columns to existing tables (idempotent)
func (s *SQLiteStorage) migrateSchema() error {
	// Each migration is a column addition that silently succeeds if column already exists
	migrations := []string{
		// Time awareness: track last access and access frequency for decay-based ranking
		"ALTER TABLE entities ADD COLUMN last_accessed_at TIMESTAMP",
		"ALTER TABLE entities ADD COLUMN access_count INTEGER DEFAULT 0",
		// Observation metadata: source tracking, confidence scoring, tagging
		"ALTER TABLE observations ADD COLUMN source TEXT DEFAULT ''",
		"ALTER TABLE observations ADD COLUMN confidence REAL DEFAULT 1.0",
		"ALTER TABLE observations ADD COLUMN tags TEXT DEFAULT '[]'",
	}

	for _, m := range migrations {
		_, err := s.db.Exec(m)
		if err != nil {
			// Ignore "duplicate column" errors — column already exists
			if !strings.Contains(err.Error(), "duplicate column") {
				return fmt.Errorf("migration failed (%s): %w", m, err)
			}
		}
	}

	// Create synonyms table for query expansion
	_, _ = s.db.Exec(`CREATE TABLE IF NOT EXISTS synonyms (
		term TEXT PRIMARY KEY,
		expanded TEXT NOT NULL
	)`)

	// Seed common tech synonyms
	defaultSynonyms := [][2]string{
		{"js", "javascript"}, {"ts", "typescript"}, {"py", "python"},
		{"rb", "ruby"}, {"rs", "rust"}, {"kt", "kotlin"},
		{"react", "reactjs"}, {"vue", "vuejs"}, {"ng", "angular"},
		{"k8s", "kubernetes"}, {"tf", "terraform"}, {"gh", "github"},
		{"db", "database"}, {"api", "interface"}, {"cli", "command"},
		{"ui", "interface"}, {"ml", "machine learning"}, {"ai", "artificial intelligence"},
	}
	synonymStmt, _ := s.db.Prepare("INSERT OR IGNORE INTO synonyms (term, expanded) VALUES (?, ?)")
	if synonymStmt != nil {
		for _, syn := range defaultSynonyms {
			synonymStmt.Exec(syn[0], syn[1])
		}
		synonymStmt.Close()
	}

	// Update schema version
	_, _ = s.db.Exec("INSERT OR REPLACE INTO metadata (key, value) VALUES ('schema_version', '3.0')")

	return nil
}

// Close closes both read and write database connections
func (s *SQLiteStorage) Close() error {
	var errs []error
	if s.dbRead != nil {
		if err := s.dbRead.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if s.db != nil {
		if err := s.db.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return errs[0]
	}
	return nil
}

// rdb returns the read database connection, falling back to write connection
// if the read pool is not initialized (e.g., during schema setup).
func (s *SQLiteStorage) rdb() *sql.DB {
	if s.dbRead != nil {
		return s.dbRead
	}
	return s.db
}

// batchThreshold is the entity count above which bulk optimizations are applied
const batchThreshold = 20

// CreateEntities creates new entities in the database.
// For large batches (>20 entities), FTS triggers are temporarily disabled
// and the FTS index is rebuilt after insertion for better performance.
func (s *SQLiteStorage) CreateEntities(entities []Entity) ([]Entity, error) {
	if len(entities) == 0 {
		return []Entity{}, nil
	}

	useBulk := len(entities) > batchThreshold && s.isFTSAvailable()

	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// For large batches, disable FTS triggers during insertion
	if useBulk {
		tx.Exec("DROP TRIGGER IF EXISTS entities_fts_insert")
		tx.Exec("DROP TRIGGER IF EXISTS observations_fts_insert")
	}

	// Prepare statements
	entityStmt, err := tx.Prepare(`
		INSERT INTO entities (name, entity_type)
		VALUES (?, ?)
		ON CONFLICT(name) DO UPDATE SET
			entity_type = excluded.entity_type,
			updated_at = CURRENT_TIMESTAMP
		RETURNING id
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare entity statement: %w", err)
	}
	defer entityStmt.Close()

	obsStmt, err := tx.Prepare(`
		INSERT INTO observations (entity_id, content)
		VALUES (?, ?)
		ON CONFLICT(entity_id, content) DO NOTHING
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare observation statement: %w", err)
	}
	defer obsStmt.Close()

	created := make([]Entity, 0, len(entities))

	for _, entity := range entities {
		var entityID int64
		err = entityStmt.QueryRow(entity.Name, entity.EntityType).Scan(&entityID)
		if err != nil {
			return nil, fmt.Errorf("failed to insert entity %s: %w", entity.Name, err)
		}

		// Insert observations
		for _, obs := range entity.Observations {
			_, err = obsStmt.Exec(entityID, obs)
			if err != nil {
				return nil, fmt.Errorf("failed to insert observation for %s: %w", entity.Name, err)
			}
		}

		created = append(created, entity)
	}

	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("failed to commit transaction: %w", err)
	}

	// Rebuild FTS index after bulk insertion
	if useBulk {
		s.createFTSSchema() // re-create triggers
		s.rebuildFTSIndex() // populate FTS with new data
	}

	return created, nil
}

// DeleteEntities deletes entities by name
func (s *SQLiteStorage) DeleteEntities(names []string) error {
	if len(names) == 0 {
		return nil
	}

	placeholders := make([]string, len(names))
	args := make([]interface{}, len(names))
	for i, name := range names {
		placeholders[i] = "?"
		args[i] = name
	}

	query := fmt.Sprintf("DELETE FROM entities WHERE name IN (%s)", strings.Join(placeholders, ","))
	_, err := s.db.Exec(query, args...)
	if err != nil {
		return fmt.Errorf("failed to delete entities: %w", err)
	}

	return nil
}

// CreateRelations creates new relations
func (s *SQLiteStorage) CreateRelations(relations []Relation) ([]Relation, error) {
	if len(relations) == 0 {
		return []Relation{}, nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT INTO relations (from_entity_id, to_entity_id, relation_type)
		SELECT 
			(SELECT id FROM entities WHERE name = ? LIMIT 1),
			(SELECT id FROM entities WHERE name = ? LIMIT 1),
			?
		WHERE EXISTS(SELECT 1 FROM entities WHERE name = ?)
		  AND EXISTS(SELECT 1 FROM entities WHERE name = ?)
		ON CONFLICT(from_entity_id, to_entity_id, relation_type) DO NOTHING
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare statement: %w", err)
	}
	defer stmt.Close()

	created := make([]Relation, 0, len(relations))

	for _, rel := range relations {
		result, err := stmt.Exec(rel.From, rel.To, rel.RelationType, rel.From, rel.To)
		if err != nil {
			return nil, fmt.Errorf("failed to insert relation: %w", err)
		}

		if rows, _ := result.RowsAffected(); rows > 0 {
			created = append(created, rel)
		}
	}

	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("failed to commit transaction: %w", err)
	}

	return created, nil
}

// DeleteRelations deletes specific relations
func (s *SQLiteStorage) DeleteRelations(relations []Relation) error {
	if len(relations) == 0 {
		return nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		DELETE FROM relations 
		WHERE from_entity_id = (SELECT id FROM entities WHERE name = ?)
		AND to_entity_id = (SELECT id FROM entities WHERE name = ?)
		AND relation_type = ?
	`)
	if err != nil {
		return fmt.Errorf("failed to prepare statement: %w", err)
	}
	defer stmt.Close()

	for _, rel := range relations {
		_, err = stmt.Exec(rel.From, rel.To, rel.RelationType)
		if err != nil {
			return fmt.Errorf("failed to delete relation: %w", err)
		}
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	return nil
}

// AddObservations adds observations to entities
func (s *SQLiteStorage) AddObservations(observations map[string][]string) (map[string][]string, error) {
	if len(observations) == 0 {
		return map[string][]string{}, nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT INTO observations (entity_id, content)
		SELECT id, ? FROM entities WHERE name = ?
		ON CONFLICT(entity_id, content) DO NOTHING
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare statement: %w", err)
	}
	defer stmt.Close()

	added := make(map[string][]string)

	for entityName, obsList := range observations {
		added[entityName] = []string{}
		for _, obs := range obsList {
			result, err := stmt.Exec(obs, entityName)
			if err != nil {
				return nil, fmt.Errorf("failed to add observation: %w", err)
			}

			if rows, _ := result.RowsAffected(); rows > 0 {
				added[entityName] = append(added[entityName], obs)
			}
		}
	}

	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("failed to commit transaction: %w", err)
	}

	return added, nil
}

// DeleteObservations deletes specific observations
func (s *SQLiteStorage) DeleteObservations(deletions []ObservationDeletion) error {
	if len(deletions) == 0 {
		return nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		DELETE FROM observations 
		WHERE entity_id = (SELECT id FROM entities WHERE name = ?)
		AND content = ?
	`)
	if err != nil {
		return fmt.Errorf("failed to prepare statement: %w", err)
	}
	defer stmt.Close()

	for _, del := range deletions {
		for _, obs := range del.Observations {
			_, err = stmt.Exec(del.EntityName, obs)
			if err != nil {
				return fmt.Errorf("failed to delete observation: %w", err)
			}
		}
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	return nil
}

// ReadGraph returns either a lightweight summary or full graph based on mode
func (s *SQLiteStorage) ReadGraph(mode string, limit int) (interface{}, error) {
	if mode == "full" {
		return s.readGraphFull()
	}
	return s.readGraphSummary(limit)
}

// readGraphSummary returns a lightweight summary of the knowledge graph
func (s *SQLiteStorage) readGraphSummary(limit int) (*GraphSummary, error) {
	summary := &GraphSummary{
		EntityTypes:   make(map[string]int),
		RelationTypes: make(map[string]int),
		Entities:      []EntitySummary{},
		Limit:         limit,
	}

	// Get total entity count
	err := s.rdb().QueryRow("SELECT COUNT(*) FROM entities").Scan(&summary.TotalEntities)
	if err != nil {
		return nil, fmt.Errorf("failed to count entities: %w", err)
	}

	// Get total relation count
	err = s.rdb().QueryRow("SELECT COUNT(*) FROM relations").Scan(&summary.TotalRelations)
	if err != nil {
		return nil, fmt.Errorf("failed to count relations: %w", err)
	}

	// Get entity type distribution
	rows, err := s.rdb().Query("SELECT entity_type, COUNT(*) FROM entities GROUP BY entity_type ORDER BY COUNT(*) DESC")
	if err != nil {
		return nil, fmt.Errorf("failed to query entity types: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var entityType string
		var count int
		if err := rows.Scan(&entityType, &count); err != nil {
			return nil, fmt.Errorf("failed to scan entity type: %w", err)
		}
		summary.EntityTypes[entityType] = count
	}

	// Get relation type distribution
	rows, err = s.rdb().Query("SELECT relation_type, COUNT(*) FROM relations GROUP BY relation_type ORDER BY COUNT(*) DESC")
	if err != nil {
		return nil, fmt.Errorf("failed to query relation types: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var relationType string
		var count int
		if err := rows.Scan(&relationType, &count); err != nil {
			return nil, fmt.Errorf("failed to scan relation type: %w", err)
		}
		summary.RelationTypes[relationType] = count
	}

	// Get entity list (limited)
	rows, err = s.rdb().Query(`
		SELECT name, entity_type 
		FROM entities 
		ORDER BY created_at DESC 
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to query entities: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var name, entityType string
		if err := rows.Scan(&name, &entityType); err != nil {
			return nil, fmt.Errorf("failed to scan entity: %w", err)
		}
		summary.Entities = append(summary.Entities, EntitySummary{
			Name:       name,
			EntityType: entityType,
		})
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating entities: %w", err)
	}

	summary.HasMore = summary.TotalEntities > limit

	return summary, nil
}

// readGraphFull reads the entire knowledge graph (internal use for export/migration)
func (s *SQLiteStorage) readGraphFull() (*KnowledgeGraph, error) {
	graph := &KnowledgeGraph{
		Entities:  []Entity{},
		Relations: []Relation{},
	}

	// Load entities with observations
	rows, err := s.rdb().Query(`
		SELECT e.name, e.entity_type,
		       GROUP_CONCAT(o.content, '|||') as observations
		FROM entities e
		LEFT JOIN observations o ON e.id = o.entity_id
		GROUP BY e.id, e.name, e.entity_type
		ORDER BY e.created_at
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to query entities: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var name, entityType string
		var obsStr sql.NullString

		if err := rows.Scan(&name, &entityType, &obsStr); err != nil {
			return nil, fmt.Errorf("failed to scan entity: %w", err)
		}

		entity := Entity{
			Name:         name,
			EntityType:   entityType,
			Observations: []string{},
		}

		if obsStr.Valid && obsStr.String != "" {
			entity.Observations = strings.Split(obsStr.String, "|||")
		}

		graph.Entities = append(graph.Entities, entity)
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating entities: %w", err)
	}

	// Load relations
	rows, err = s.rdb().Query(`
		SELECT f.name, t.name, r.relation_type
		FROM relations r
		JOIN entities f ON r.from_entity_id = f.id
		JOIN entities t ON r.to_entity_id = t.id
		ORDER BY r.created_at
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to query relations: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var from, to, relType string
		if err := rows.Scan(&from, &to, &relType); err != nil {
			return nil, fmt.Errorf("failed to scan relation: %w", err)
		}

		graph.Relations = append(graph.Relations, Relation{
			From:         from,
			To:           to,
			RelationType: relType,
		})
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating relations: %w", err)
	}

	return graph, nil
}

// SearchNodes searches for nodes containing the query string and returns lightweight summaries
func (s *SQLiteStorage) SearchNodes(query string, limit int) (*SearchResult, error) {
	// Try FTS search first if available
	if s.isFTSAvailable() {
		result, err := s.SearchNodesWithFTS(query, limit)
		if err == nil {
			return result, nil
		}
		// Log FTS error but continue with basic search
		// Silently fallback - don't print to stdout in MCP mode
	}

	// Always use basic search as fallback
	return s.searchNodesBasic(query, limit)
}

// isFTSAvailable checks if FTS5 tables are available
func (s *SQLiteStorage) isFTSAvailable() bool {
	var count int
	err := s.rdb().QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='entities_fts'").Scan(&count)
	return err == nil && count > 0
}

// Match priority constants for search ranking
// Higher values indicate higher priority
const (
	PriorityNameExact   = 100 // Exact name match
	PriorityNamePartial = 80  // Partial name match
	PriorityType        = 50  // Entity type match
	PriorityContent     = 20  // Observations content match
)

// searchNodesBasic performs basic LIKE-based search and returns search hits with snippets
// Multiple space-separated words are treated as OR search
// Results are sorted by match priority: name exact > name partial > type > content
func (s *SQLiteStorage) searchNodesBasic(query string, limit int) (*SearchResult, error) {
	result := &SearchResult{
		Entities: []EntitySearchHit{},
		Limit:    limit,
	}

	if query == "" {
		return result, nil
	}

	// Split query into words for OR search and expand with synonyms
	words := strings.Fields(query)
	if len(words) == 0 {
		return result, nil
	}
	words = s.expandQueryWithSynonyms(words)

	// Build dynamic WHERE clause for multi-word OR search
	var whereClauses []string
	var countArgs []interface{}

	for _, word := range words {
		searchPattern := "%" + word + "%"
		whereClauses = append(whereClauses, "(e.name LIKE ? OR e.entity_type LIKE ? OR o.content LIKE ?)")
		countArgs = append(countArgs, searchPattern, searchPattern, searchPattern)
	}

	whereClause := strings.Join(whereClauses, " OR ")

	// First, get total count
	countQuery := fmt.Sprintf(`
		SELECT COUNT(DISTINCT e.id)
		FROM entities e
		LEFT JOIN observations o ON e.id = o.entity_id
		WHERE %s
	`, whereClause)

	err := s.rdb().QueryRow(countQuery, countArgs...).Scan(&result.Total)
	if err != nil {
		return nil, fmt.Errorf("failed to count search results: %w", err)
	}

	// Build priority CASE expression for each search word
	// Priority: name exact match > name partial > type match > content match
	var priorityCases []string
	var searchArgs []interface{}

	for _, word := range words {
		exactPattern := word
		partialPattern := "%" + word + "%"
		// CASE expression to calculate priority for each word
		priorityCases = append(priorityCases, fmt.Sprintf(`
			CASE
				WHEN e.name = ? COLLATE NOCASE THEN %d
				WHEN e.name LIKE ? COLLATE NOCASE THEN %d
				WHEN e.entity_type LIKE ? COLLATE NOCASE THEN %d
				ELSE %d
			END
		`, PriorityNameExact, PriorityNamePartial, PriorityType, PriorityContent))
		searchArgs = append(searchArgs, exactPattern, partialPattern, partialPattern)
	}

	// Use MAX to get the highest priority among all matched words
	priorityExpr := fmt.Sprintf("MAX(%s)", strings.Join(priorityCases, ", "))

	// Add WHERE clause args
	for _, word := range words {
		searchPattern := "%" + word + "%"
		searchArgs = append(searchArgs, searchPattern, searchPattern, searchPattern)
	}

	// Get matched entity IDs with priority sorting
	// Time-decay ranking: boost recently accessed entities
	// final_score = priority * (1.0 / (1.0 + 0.01 * days_since_access)) * log2(2 + access_count)
	decayExpr := `(
		CAST(%s AS REAL)
		* (1.0 / (1.0 + 0.01 * MAX(0, COALESCE(julianday('now') - julianday(COALESCE(e.last_accessed_at, e.updated_at, e.created_at)), 0))))
		* (1.0 + log(2.0 + COALESCE(e.access_count, 0)) / log(2.0))
	)`
	rankExpr := fmt.Sprintf(decayExpr, priorityExpr)

	var searchQuery string
	if limit > 0 {
		searchQuery = fmt.Sprintf(`
			SELECT e.id, e.name, e.entity_type, %s AS score
			FROM entities e
			LEFT JOIN observations o ON e.id = o.entity_id
			WHERE %s
			GROUP BY e.id, e.name, e.entity_type
			ORDER BY score DESC, e.created_at DESC
			LIMIT ?
		`, rankExpr, whereClause)
		searchArgs = append(searchArgs, limit)
	} else {
		// No limit - return all results
		searchQuery = fmt.Sprintf(`
			SELECT e.id, e.name, e.entity_type, %s AS score
			FROM entities e
			LEFT JOIN observations o ON e.id = o.entity_id
			WHERE %s
			GROUP BY e.id, e.name, e.entity_type
			ORDER BY score DESC, e.created_at DESC
		`, rankExpr, whereClause)
	}

	rows, err := s.rdb().Query(searchQuery, searchArgs...)
	if err != nil {
		return nil, fmt.Errorf("failed to search entities: %w", err)
	}
	defer rows.Close()

	var entityIDs []int64
	entityMap := make(map[int64]*EntitySearchHit)

	for rows.Next() {
		var id int64
		var name, entityType string
		var score float64
		if err := rows.Scan(&id, &name, &entityType, &score); err != nil {
			return nil, fmt.Errorf("failed to scan search result: %w", err)
		}
		entityIDs = append(entityIDs, id)
		entityMap[id] = &EntitySearchHit{
			Name:       name,
			EntityType: entityType,
			Snippets:   []string{},
		}
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating search results: %w", err)
	}

	// Get snippets, observations count, and relations count for each entity
	if len(entityIDs) > 0 {
		// Build placeholders for entity IDs
		placeholders := make([]string, len(entityIDs))
		idArgs := make([]interface{}, len(entityIDs))
		for i, id := range entityIDs {
			placeholders[i] = "?"
			idArgs[i] = id
		}
		placeholderStr := strings.Join(placeholders, ",")

		// Get observations count for each entity
		obsCountQuery := fmt.Sprintf(`
			SELECT entity_id, COUNT(*) 
			FROM observations 
			WHERE entity_id IN (%s) 
			GROUP BY entity_id
		`, placeholderStr)
		obsRows, err := s.rdb().Query(obsCountQuery, idArgs...)
		if err == nil {
			defer obsRows.Close()
			for obsRows.Next() {
				var entityID int64
				var count int
				if err := obsRows.Scan(&entityID, &count); err == nil {
					if hit, ok := entityMap[entityID]; ok {
						hit.ObservationsCount = count
					}
				}
			}
		}

		// Get relations count for each entity
		relCountQuery := fmt.Sprintf(`
			SELECT e.id, COUNT(DISTINCT r.id)
			FROM entities e
			LEFT JOIN relations r ON e.id = r.from_entity_id OR e.id = r.to_entity_id
			WHERE e.id IN (%s)
			GROUP BY e.id
		`, placeholderStr)
		relRows, err := s.rdb().Query(relCountQuery, idArgs...)
		if err == nil {
			defer relRows.Close()
			for relRows.Next() {
				var entityID int64
				var count int
				if err := relRows.Scan(&entityID, &count); err == nil {
					if hit, ok := entityMap[entityID]; ok {
						hit.RelationsCount = count
					}
				}
			}
		}

		// Get snippets - observations that match query with context around keywords
		// maxSnippets=0 means return all matched snippets when limit=0
		maxSnippets := 2
		if limit == 0 {
			maxSnippets = 0 // unlimited snippets
		}
		for _, id := range entityIDs {
			hit := entityMap[id]
			snippets := s.getMatchedSnippets(id, words, maxSnippets, 50) // 50 chars context before/after keyword
			hit.Snippets = snippets
		}
	}

	// Build result maintaining order
	for _, id := range entityIDs {
		result.Entities = append(result.Entities, *entityMap[id])
	}

	// Update access stats for matched entities
	s.updateAccessStats(entityIDs)

	// Graph traversal: find 1-hop related entities
	result.RelatedEntities = s.findRelatedEntities(entityIDs, entityMap)

	// HasMore is only true when limit is specified and there are more results
	if limit > 0 {
		result.HasMore = result.Total > limit
	} else {
		result.HasMore = false // no limit means all results returned
	}

	return result, nil
}

// findRelatedEntities performs 1-hop graph traversal from matched entities to find related context.
// Returns up to 10 related entities that are not already in the direct match results.
func (s *SQLiteStorage) findRelatedEntities(entityIDs []int64, directHits map[int64]*EntitySearchHit) []RelatedHit {
	if len(entityIDs) == 0 {
		return nil
	}

	placeholders := make([]string, len(entityIDs))
	args := make([]interface{}, len(entityIDs))
	for i, id := range entityIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	placeholderStr := strings.Join(placeholders, ",")

	// Find outgoing relations: matched entity -> related entity
	// Find incoming relations: related entity -> matched entity
	// Exclude entities already in direct hits
	query := fmt.Sprintf(`
		SELECT DISTINCT
			e.id, e.name, e.entity_type, r.relation_type,
			matched.name AS related_to,
			CASE WHEN r.from_entity_id IN (%s) THEN 'outgoing' ELSE 'incoming' END AS direction
		FROM relations r
		JOIN entities e ON (
			CASE WHEN r.from_entity_id IN (%s)
				THEN e.id = r.to_entity_id
				ELSE e.id = r.from_entity_id
			END
		)
		JOIN entities matched ON (
			CASE WHEN r.from_entity_id IN (%s)
				THEN matched.id = r.from_entity_id
				ELSE matched.id = r.to_entity_id
			END
		)
		WHERE (r.from_entity_id IN (%s) OR r.to_entity_id IN (%s))
		  AND e.id NOT IN (%s)
		LIMIT 10
	`, placeholderStr, placeholderStr, placeholderStr, placeholderStr, placeholderStr, placeholderStr)

	// 6 uses of args
	allArgs := make([]interface{}, 0, len(args)*6)
	for i := 0; i < 6; i++ {
		allArgs = append(allArgs, args...)
	}

	rows, err := s.rdb().Query(query, allArgs...)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var related []RelatedHit
	seen := make(map[string]bool)
	for rows.Next() {
		var id int64
		var name, entityType, relationType, relatedTo, direction string
		if err := rows.Scan(&id, &name, &entityType, &relationType, &relatedTo, &direction); err != nil {
			continue
		}
		// Deduplicate by name
		if seen[name] {
			continue
		}
		seen[name] = true
		related = append(related, RelatedHit{
			Name:         name,
			EntityType:   entityType,
			RelationType: relationType,
			RelatedTo:    relatedTo,
			Direction:    direction,
		})
	}

	return related
}

// getMatchedSnippets returns context snippets around matched keywords
// contextChars is the number of characters to show before and after the keyword
func (s *SQLiteStorage) getMatchedSnippets(entityID int64, words []string, maxSnippets int, contextChars int) []string {
	var snippets []string

	// Build WHERE clause to find matching observations
	var whereClauses []string
	var args []interface{}
	args = append(args, entityID)

	for _, word := range words {
		whereClauses = append(whereClauses, "content LIKE ?")
		args = append(args, "%"+word+"%")
	}

	query := fmt.Sprintf(`
		SELECT content FROM observations
		WHERE entity_id = ? AND (%s)
	`, strings.Join(whereClauses, " OR "))

	rows, err := s.rdb().Query(query, args...)
	if err != nil {
		return snippets
	}
	defer rows.Close()

	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err == nil {
			// Extract context around matched keyword
			snippet := extractKeywordContext(content, words, contextChars)
			snippets = append(snippets, snippet)
			if maxSnippets > 0 && len(snippets) >= maxSnippets {
				break
			}
		}
	}

	// If no matched observations, get first 2 observations as fallback
	if len(snippets) == 0 {
		fallbackRows, err := s.rdb().Query(
			"SELECT content FROM observations WHERE entity_id = ? LIMIT ?",
			entityID, 2,
		)
		if err == nil {
			defer fallbackRows.Close()
			for fallbackRows.Next() {
				var content string
				if err := fallbackRows.Scan(&content); err == nil {
					snippets = append(snippets, truncateString(content, contextChars*2))
				}
			}
		}
	}

	return snippets
}

// extractKeywordContext extracts a snippet with context around the first matched keyword
func extractKeywordContext(content string, words []string, contextChars int) string {
	contentLower := strings.ToLower(content)
	contentRunes := []rune(content)
	contentLen := len(contentRunes)

	// Find the first matching keyword position
	matchPos := -1
	matchLen := 0
	for _, word := range words {
		wordLower := strings.ToLower(word)
		pos := strings.Index(contentLower, wordLower)
		if pos != -1 {
			// Convert byte position to rune position
			runePos := len([]rune(content[:pos]))
			if matchPos == -1 || runePos < matchPos {
				matchPos = runePos
				matchLen = len([]rune(word))
			}
		}
	}

	// If no match found, return truncated content
	if matchPos == -1 {
		return truncateString(content, contextChars*2)
	}

	// Calculate start and end positions for context
	start := matchPos - contextChars
	if start < 0 {
		start = 0
	}
	end := matchPos + matchLen + contextChars
	if end > contentLen {
		end = contentLen
	}

	// Build snippet with ellipsis
	var result strings.Builder
	if start > 0 {
		result.WriteString("...")
	}
	result.WriteString(string(contentRunes[start:end]))
	if end < contentLen {
		result.WriteString("...")
	}

	return result.String()
}

// truncateString truncates a string to maxLen characters and adds "..." if truncated
func truncateString(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "..."
}

// OpenNodes retrieves specific nodes by name with truncation protection
const maxObservationsPerEntity = 100

func (s *SQLiteStorage) OpenNodes(names []string) (*KnowledgeGraph, error) {
	graph := &KnowledgeGraph{
		Entities:  []Entity{},
		Relations: []Relation{},
	}

	if len(names) == 0 {
		return graph, nil
	}

	placeholders := make([]string, len(names))
	args := make([]interface{}, len(names))
	for i, name := range names {
		placeholders[i] = "?"
		args[i] = name
	}

	// Load entities first (without observations)
	query := fmt.Sprintf(`
		SELECT e.id, e.name, e.entity_type
		FROM entities e
		WHERE e.name IN (%s)
		ORDER BY e.created_at
	`, strings.Join(placeholders, ","))

	rows, err := s.rdb().Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query entities: %w", err)
	}
	defer rows.Close()

	entityIDs := []int64{}
	entityMap := make(map[int64]*Entity)

	for rows.Next() {
		var id int64
		var name, entityType string

		if err := rows.Scan(&id, &name, &entityType); err != nil {
			return nil, fmt.Errorf("failed to scan entity: %w", err)
		}

		entityIDs = append(entityIDs, id)
		entityMap[id] = &Entity{
			Name:         name,
			EntityType:   entityType,
			Observations: []string{},
		}
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating entities: %w", err)
	}

	// Load observations for each entity with truncation
	truncated := false
	for _, id := range entityIDs {
		entity := entityMap[id]

		// Get total count first
		var totalObs int
		s.rdb().QueryRow("SELECT COUNT(*) FROM observations WHERE entity_id = ?", id).Scan(&totalObs)

		// Get observations with limit
		obsRows, err := s.rdb().Query(
			"SELECT content FROM observations WHERE entity_id = ? LIMIT ?",
			id, maxObservationsPerEntity,
		)
		if err != nil {
			continue
		}

		for obsRows.Next() {
			var content string
			if err := obsRows.Scan(&content); err == nil {
				entity.Observations = append(entity.Observations, content)
			}
		}
		obsRows.Close()

		if totalObs > maxObservationsPerEntity {
			truncated = true
		}
	}

	// Build entities list maintaining order
	for _, id := range entityIDs {
		graph.Entities = append(graph.Entities, *entityMap[id])
	}

	graph.Truncated = truncated

	// Update access stats asynchronously
	s.updateAccessStats(entityIDs)

	// Load relations for found entities
	if len(entityIDs) > 0 {
		placeholders := make([]string, len(entityIDs))
		args := make([]interface{}, len(entityIDs))
		for i, id := range entityIDs {
			placeholders[i] = "?"
			args[i] = id
		}

		relQuery := fmt.Sprintf(`
			SELECT f.name, t.name, r.relation_type
			FROM relations r
			JOIN entities f ON r.from_entity_id = f.id
			JOIN entities t ON r.to_entity_id = t.id
			WHERE r.from_entity_id IN (%s) OR r.to_entity_id IN (%s)
			ORDER BY r.created_at
		`, strings.Join(placeholders, ","), strings.Join(placeholders, ","))

		// Duplicate args for both IN clauses
		relArgs := append(args, args...)

		rows, err := s.rdb().Query(relQuery, relArgs...)
		if err != nil {
			return nil, fmt.Errorf("failed to query relations: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var from, to, relType string
			if err := rows.Scan(&from, &to, &relType); err != nil {
				return nil, fmt.Errorf("failed to scan relation: %w", err)
			}

			graph.Relations = append(graph.Relations, Relation{
				From:         from,
				To:           to,
				RelationType: relType,
			})
		}

		if err = rows.Err(); err != nil {
			return nil, fmt.Errorf("error iterating relations: %w", err)
		}
	}

	return graph, nil
}

// updateAccessStats updates last_accessed_at and access_count for the given entity IDs.
// Runs asynchronously to avoid blocking read operations.
func (s *SQLiteStorage) updateAccessStats(entityIDs []int64) {
	if len(entityIDs) == 0 {
		return
	}

	go func() {
		placeholders := make([]string, len(entityIDs))
		args := make([]interface{}, len(entityIDs))
		for i, id := range entityIDs {
			placeholders[i] = "?"
			args[i] = id
		}
		query := fmt.Sprintf(`
			UPDATE entities
			SET last_accessed_at = CURRENT_TIMESTAMP,
			    access_count = COALESCE(access_count, 0) + 1
			WHERE id IN (%s)
		`, strings.Join(placeholders, ","))
		s.db.Exec(query, args...)
	}()
}

// MergeEntities merges source entity into target: migrates observations and relations, then deletes source.
func (s *SQLiteStorage) MergeEntities(sourceName, targetName string) (*MergeResult, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Get source and target entity IDs
	var sourceID, targetID int64
	err = tx.QueryRow("SELECT id FROM entities WHERE name = ?", sourceName).Scan(&sourceID)
	if err != nil {
		return nil, fmt.Errorf("source entity %q not found: %w", sourceName, err)
	}
	err = tx.QueryRow("SELECT id FROM entities WHERE name = ?", targetName).Scan(&targetID)
	if err != nil {
		return nil, fmt.Errorf("target entity %q not found: %w", targetName, err)
	}

	// Migrate observations (skip duplicates)
	obsResult, err := tx.Exec(`
		INSERT INTO observations (entity_id, content)
		SELECT ?, content FROM observations WHERE entity_id = ?
		ON CONFLICT(entity_id, content) DO NOTHING
	`, targetID, sourceID)
	if err != nil {
		return nil, fmt.Errorf("failed to migrate observations: %w", err)
	}
	mergedObs, _ := obsResult.RowsAffected()

	// Redirect outgoing relations from source to target
	outResult, err := tx.Exec(`
		UPDATE relations SET from_entity_id = ?
		WHERE from_entity_id = ?
		AND NOT EXISTS (
			SELECT 1 FROM relations r2
			WHERE r2.from_entity_id = ? AND r2.to_entity_id = relations.to_entity_id
			AND r2.relation_type = relations.relation_type
		)
	`, targetID, sourceID, targetID)
	if err != nil {
		return nil, fmt.Errorf("failed to redirect outgoing relations: %w", err)
	}
	mergedOut, _ := outResult.RowsAffected()

	// Redirect incoming relations to target
	inResult, err := tx.Exec(`
		UPDATE relations SET to_entity_id = ?
		WHERE to_entity_id = ?
		AND NOT EXISTS (
			SELECT 1 FROM relations r2
			WHERE r2.from_entity_id = relations.from_entity_id AND r2.to_entity_id = ?
			AND r2.relation_type = relations.relation_type
		)
	`, targetID, sourceID, targetID)
	if err != nil {
		return nil, fmt.Errorf("failed to redirect incoming relations: %w", err)
	}
	mergedIn, _ := inResult.RowsAffected()

	// Delete source entity (cascades observations and remaining duplicate relations)
	_, err = tx.Exec("DELETE FROM entities WHERE id = ?", sourceID)
	if err != nil {
		return nil, fmt.Errorf("failed to delete source entity: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("failed to commit merge: %w", err)
	}

	return &MergeResult{
		MergedObservations: int(mergedObs),
		MergedRelations:    int(mergedOut + mergedIn),
		SourceDeleted:      true,
	}, nil
}

// UpdateEntityType updates the entity type for a given entity name.
func (s *SQLiteStorage) UpdateEntityType(name string, newType string) error {
	result, err := s.db.Exec(
		"UPDATE entities SET entity_type = ?, updated_at = CURRENT_TIMESTAMP WHERE name = ?",
		newType, name,
	)
	if err != nil {
		return fmt.Errorf("failed to update entity type: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("entity %q not found", name)
	}
	return nil
}

// UpdateObservation replaces an observation's content for a given entity.
func (s *SQLiteStorage) UpdateObservation(entityName string, oldContent string, newContent string) error {
	result, err := s.db.Exec(`
		UPDATE observations SET content = ?
		WHERE entity_id = (SELECT id FROM entities WHERE name = ?)
		AND content = ?
	`, newContent, entityName, oldContent)
	if err != nil {
		return fmt.Errorf("failed to update observation: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("observation not found for entity %q", entityName)
	}
	return nil
}

// DetectConflicts finds potential duplicate or contradictory observations within an entity.
// If entityName is empty, checks all entities.
func (s *SQLiteStorage) DetectConflicts(entityName string) ([]Conflict, error) {
	var conflicts []Conflict

	// Build query to compare observation pairs within the same entity
	query := `
		SELECT e.name, o1.content, o2.content
		FROM observations o1
		JOIN observations o2 ON o1.entity_id = o2.entity_id AND o1.id < o2.id
		JOIN entities e ON e.id = o1.entity_id
	`
	var args []interface{}
	if entityName != "" {
		query += " WHERE e.name = ?"
		args = append(args, entityName)
	}
	query += " ORDER BY e.name, o1.id"

	rows, err := s.rdb().Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query observations: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var name, content1, content2 string
		if err := rows.Scan(&name, &content1, &content2); err != nil {
			continue
		}

		if conflictType := detectConflictType(content1, content2); conflictType != "" {
			conflicts = append(conflicts, Conflict{
				EntityName:   name,
				Observation1: content1,
				Observation2: content2,
				Type:         conflictType,
			})
		}
	}

	return conflicts, nil
}

// detectConflictType checks if two observations are potentially conflicting.
// Returns conflict type string or empty if no conflict detected.
func detectConflictType(a, b string) string {
	aLower := strings.ToLower(a)
	bLower := strings.ToLower(b)

	// Check for high prefix overlap (potential duplicate)
	if prefixOverlap(aLower, bLower) > 0.6 && aLower != bLower {
		return "potential_duplicate"
	}

	// Check for antonym keyword pairs (potential contradiction)
	antonyms := [][2]string{
		{"enabled", "disabled"},
		{"true", "false"},
		{"likes", "dislikes"},
		{"prefers", "avoids"},
		{"uses", "does not use"},
		{"active", "inactive"},
		{"yes", "no"},
		{"always", "never"},
		{"supports", "does not support"},
	}

	for _, pair := range antonyms {
		aHas0 := strings.Contains(aLower, pair[0])
		aHas1 := strings.Contains(aLower, pair[1])
		bHas0 := strings.Contains(bLower, pair[0])
		bHas1 := strings.Contains(bLower, pair[1])

		// One has the positive term, other has the negative (but not both containing both)
		if (aHas0 && bHas1 && !aHas1 && !bHas0) || (aHas1 && bHas0 && !aHas0 && !bHas1) {
			// Only flag if observations share enough context (at least one common word beyond the antonym)
			aWords := strings.Fields(aLower)
			bWords := strings.Fields(bLower)
			commonWords := 0
			for _, aw := range aWords {
				if aw == pair[0] || aw == pair[1] {
					continue
				}
				for _, bw := range bWords {
					if aw == bw {
						commonWords++
						break
					}
				}
			}
			if commonWords >= 1 {
				return "potential_contradiction"
			}
		}
	}

	return ""
}

// prefixOverlap calculates the ratio of common prefix length to the shorter string length.
func prefixOverlap(a, b string) float64 {
	aWords := strings.Fields(a)
	bWords := strings.Fields(b)
	if len(aWords) == 0 || len(bWords) == 0 {
		return 0
	}

	common := 0
	minLen := len(aWords)
	if len(bWords) < minLen {
		minLen = len(bWords)
	}
	for i := 0; i < minLen; i++ {
		if aWords[i] == bWords[i] {
			common++
		} else {
			break
		}
	}
	return float64(common) / float64(minLen)
}

// ExportData exports all data for migration
func (s *SQLiteStorage) ExportData() (*KnowledgeGraph, error) {
	return s.readGraphFull()
}

// ImportData imports data during migration
func (s *SQLiteStorage) ImportData(graph *KnowledgeGraph) error {
	if graph == nil {
		return nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Import entities
	if len(graph.Entities) > 0 {
		entityStmt, err := tx.Prepare(`
			INSERT INTO entities (name, entity_type) 
			VALUES (?, ?) 
			ON CONFLICT(name) DO UPDATE SET 
				entity_type = excluded.entity_type,
				updated_at = CURRENT_TIMESTAMP
			RETURNING id
		`)
		if err != nil {
			return fmt.Errorf("failed to prepare entity statement: %w", err)
		}
		defer entityStmt.Close()

		obsStmt, err := tx.Prepare(`
			INSERT INTO observations (entity_id, content) 
			VALUES (?, ?) 
			ON CONFLICT(entity_id, content) DO NOTHING
		`)
		if err != nil {
			return fmt.Errorf("failed to prepare observation statement: %w", err)
		}
		defer obsStmt.Close()

		for _, entity := range graph.Entities {
			var entityID int64
			err = entityStmt.QueryRow(entity.Name, entity.EntityType).Scan(&entityID)
			if err != nil {
				return fmt.Errorf("failed to import entity %s: %w", entity.Name, err)
			}

			for _, obs := range entity.Observations {
				_, err = obsStmt.Exec(entityID, obs)
				if err != nil {
					return fmt.Errorf("failed to import observation for %s: %w", entity.Name, err)
				}
			}
		}
	}

	// Import relations
	if len(graph.Relations) > 0 {
		relStmt, err := tx.Prepare(`
			INSERT INTO relations (from_entity_id, to_entity_id, relation_type)
			SELECT 
				(SELECT id FROM entities WHERE name = ? LIMIT 1),
				(SELECT id FROM entities WHERE name = ? LIMIT 1),
				?
			WHERE EXISTS(SELECT 1 FROM entities WHERE name = ?)
			  AND EXISTS(SELECT 1 FROM entities WHERE name = ?)
			ON CONFLICT(from_entity_id, to_entity_id, relation_type) DO NOTHING
		`)
		if err != nil {
			return fmt.Errorf("failed to prepare relation statement: %w", err)
		}
		defer relStmt.Close()

		for _, rel := range graph.Relations {
			_, err = relStmt.Exec(rel.From, rel.To, rel.RelationType, rel.From, rel.To)
			if err != nil {
				return fmt.Errorf("failed to import relation: %w", err)
			}
		}
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit import transaction: %w", err)
	}

	return nil
}
