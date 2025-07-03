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

// ReadGraph reads the entire knowledge graph
func (s *SQLiteStorage) ReadGraph() (*KnowledgeGraph, error) {
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

// SearchNodes searches for nodes containing the query string
func (s *SQLiteStorage) SearchNodes(query string) (*KnowledgeGraph, error) {
	// Try FTS search first if available
	if s.isFTSAvailable() {
		result, err := s.SearchNodesWithFTS(query)
		if err == nil {
			return result, nil
		}
		// Log FTS error but continue with basic search
		// Silently fallback - don't print to stdout in MCP mode
	}
	
	// Always use basic search as fallback
	return s.searchNodesBasic(query)
}

// isFTSAvailable checks if FTS5 tables are available
func (s *SQLiteStorage) isFTSAvailable() bool {
	var count int
	err := s.db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='entities_fts'").Scan(&count)
	return err == nil && count > 0
}

// searchNodesBasic performs basic LIKE-based search
func (s *SQLiteStorage) searchNodesBasic(query string) (*KnowledgeGraph, error) {
	graph := &KnowledgeGraph{
		Entities:  []Entity{},
		Relations: []Relation{},
	}
	
	if query == "" {
		return graph, nil
	}
	
	// Search in entity names, types, and observations
	searchQuery := `
		SELECT DISTINCT e.id, e.name, e.entity_type
		FROM entities e
		LEFT JOIN observations o ON e.id = o.entity_id
		WHERE e.name LIKE ? 
		   OR e.entity_type LIKE ?
		   OR o.content LIKE ?
		ORDER BY e.created_at
	`
	
	searchPattern := "%" + query + "%"
	rows, err := s.db.Query(searchQuery, searchPattern, searchPattern, searchPattern)
	if err != nil {
		return nil, fmt.Errorf("failed to search entities: %w", err)
	}
	defer rows.Close()
	
	entityIDs := []int64{}
	entityMap := make(map[int64]Entity)
	
	for rows.Next() {
		var id int64
		var name, entityType string
		
		if err := rows.Scan(&id, &name, &entityType); err != nil {
			return nil, fmt.Errorf("failed to scan search result: %w", err)
		}
		
		entityIDs = append(entityIDs, id)
		entityMap[id] = Entity{
			Name:         name,
			EntityType:   entityType,
			Observations: []string{},
		}
	}
	
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating search results: %w", err)
	}
	
	// Load observations for found entities
	if len(entityIDs) > 0 {
		placeholders := make([]string, len(entityIDs))
		args := make([]interface{}, len(entityIDs))
		for i, id := range entityIDs {
			placeholders[i] = "?"
			args[i] = id
		}
		
		obsQuery := fmt.Sprintf(`
			SELECT entity_id, content 
			FROM observations 
			WHERE entity_id IN (%s)
			ORDER BY id
		`, strings.Join(placeholders, ","))
		
		rows, err := s.db.Query(obsQuery, args...)
		if err != nil {
			return nil, fmt.Errorf("failed to query observations: %w", err)
		}
		defer rows.Close()
		
		for rows.Next() {
			var entityID int64
			var content string
			
			if err := rows.Scan(&entityID, &content); err != nil {
				return nil, fmt.Errorf("failed to scan observation: %w", err)
			}
			
			if entity, ok := entityMap[entityID]; ok {
				entity.Observations = append(entity.Observations, content)
				entityMap[entityID] = entity
			}
		}
		
		if err = rows.Err(); err != nil {
			return nil, fmt.Errorf("error iterating observations: %w", err)
		}
		
		// Convert map to slice
		for _, entity := range entityMap {
			graph.Entities = append(graph.Entities, entity)
		}
		
		// Load relations for found entities
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
		
		rows, err = s.db.Query(relQuery, relArgs...)
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

// OpenNodes retrieves specific nodes by name
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
	
	// Load entities with observations
	query := fmt.Sprintf(`
		SELECT e.id, e.name, e.entity_type, 
		       GROUP_CONCAT(o.content, '|||') as observations
		FROM entities e
		LEFT JOIN observations o ON e.id = o.entity_id
		WHERE e.name IN (%s)
		GROUP BY e.id, e.name, e.entity_type
		ORDER BY e.created_at
	`, strings.Join(placeholders, ","))
	
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query entities: %w", err)
	}
	defer rows.Close()
	
	entityIDs := []int64{}
	
	for rows.Next() {
		var id int64
		var name, entityType string
		var obsStr sql.NullString
		
		if err := rows.Scan(&id, &name, &entityType, &obsStr); err != nil {
			return nil, fmt.Errorf("failed to scan entity: %w", err)
		}
		
		entityIDs = append(entityIDs, id)
		
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
	return s.ReadGraph()
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