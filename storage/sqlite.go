package storage

import (
	"database/sql"
	"fmt"
	"strings"

	_ "modernc.org/sqlite"
)

// SQLiteStorage implements Storage interface using SQLite
type SQLiteStorage struct {
	db     *sql.DB
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

	// Create schema
	if err = s.createSchema(); err != nil {
		return fmt.Errorf("failed to create schema: %w", err)
	}

	// Try to create FTS schema (optional, will fallback to regular search if it fails)
	if err = s.createFTSSchema(); err != nil {
		// Log warning but don't fail initialization
		// Silently fallback - don't print to stdout in MCP mode
		// FTS5 is optional, basic search will work fine
	}

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

// Close closes the database connection
func (s *SQLiteStorage) Close() error {
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

// CreateEntities creates new entities in the database
func (s *SQLiteStorage) CreateEntities(entities []Entity) ([]Entity, error) {
	if len(entities) == 0 {
		return []Entity{}, nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

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
	err := s.db.QueryRow("SELECT COUNT(*) FROM entities").Scan(&summary.TotalEntities)
	if err != nil {
		return nil, fmt.Errorf("failed to count entities: %w", err)
	}

	// Get total relation count
	err = s.db.QueryRow("SELECT COUNT(*) FROM relations").Scan(&summary.TotalRelations)
	if err != nil {
		return nil, fmt.Errorf("failed to count relations: %w", err)
	}

	// Get entity type distribution
	rows, err := s.db.Query("SELECT entity_type, COUNT(*) FROM entities GROUP BY entity_type ORDER BY COUNT(*) DESC")
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
	rows, err = s.db.Query("SELECT relation_type, COUNT(*) FROM relations GROUP BY relation_type ORDER BY COUNT(*) DESC")
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
	rows, err = s.db.Query(`
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
	rows, err := s.db.Query(`
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
	rows, err = s.db.Query(`
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
	err := s.db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='entities_fts'").Scan(&count)
	return err == nil && count > 0
}

// searchNodesBasic performs basic LIKE-based search and returns search hits with snippets
// Multiple space-separated words are treated as OR search
func (s *SQLiteStorage) searchNodesBasic(query string, limit int) (*SearchResult, error) {
	result := &SearchResult{
		Entities: []EntitySearchHit{},
		Limit:    limit,
	}

	if query == "" {
		return result, nil
	}

	// Split query into words for OR search
	words := strings.Fields(query)
	if len(words) == 0 {
		return result, nil
	}

	// Build dynamic WHERE clause for multi-word OR search
	var whereClauses []string
	var args []interface{}

	for _, word := range words {
		searchPattern := "%" + word + "%"
		whereClauses = append(whereClauses, "(e.name LIKE ? OR e.entity_type LIKE ? OR o.content LIKE ?)")
		args = append(args, searchPattern, searchPattern, searchPattern)
	}

	whereClause := strings.Join(whereClauses, " OR ")

	// First, get total count
	countQuery := fmt.Sprintf(`
		SELECT COUNT(DISTINCT e.id)
		FROM entities e
		LEFT JOIN observations o ON e.id = o.entity_id
		WHERE %s
	`, whereClause)

	err := s.db.QueryRow(countQuery, args...).Scan(&result.Total)
	if err != nil {
		return nil, fmt.Errorf("failed to count search results: %w", err)
	}

	// Get matched entity IDs (with optional limit)
	var searchQuery string
	if limit > 0 {
		searchQuery = fmt.Sprintf(`
			SELECT DISTINCT e.id, e.name, e.entity_type
			FROM entities e
			LEFT JOIN observations o ON e.id = o.entity_id
			WHERE %s
			ORDER BY e.created_at DESC
			LIMIT ?
		`, whereClause)
		args = append(args, limit)
	} else {
		// No limit - return all results
		searchQuery = fmt.Sprintf(`
			SELECT DISTINCT e.id, e.name, e.entity_type
			FROM entities e
			LEFT JOIN observations o ON e.id = o.entity_id
			WHERE %s
			ORDER BY e.created_at DESC
		`, whereClause)
	}

	rows, err := s.db.Query(searchQuery, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to search entities: %w", err)
	}
	defer rows.Close()

	var entityIDs []int64
	entityMap := make(map[int64]*EntitySearchHit)

	for rows.Next() {
		var id int64
		var name, entityType string
		if err := rows.Scan(&id, &name, &entityType); err != nil {
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
		obsRows, err := s.db.Query(obsCountQuery, idArgs...)
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
		relRows, err := s.db.Query(relCountQuery, idArgs...)
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

	// HasMore is only true when limit is specified and there are more results
	if limit > 0 {
		result.HasMore = result.Total > limit
	} else {
		result.HasMore = false // no limit means all results returned
	}

	return result, nil
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

	rows, err := s.db.Query(query, args...)
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
		fallbackRows, err := s.db.Query(
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

	rows, err := s.db.Query(query, args...)
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
		s.db.QueryRow("SELECT COUNT(*) FROM observations WHERE entity_id = ?", id).Scan(&totalObs)

		// Get observations with limit
		obsRows, err := s.db.Query(
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

		rows, err := s.db.Query(relQuery, relArgs...)
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
