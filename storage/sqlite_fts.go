package storage

import (
	"database/sql"
	"fmt"
	"strings"
)

// FTSConfig holds FTS5 configuration
type FTSConfig struct {
	Enabled          bool
	Tokenizer        string // porter, unicode61, etc.
	RemoveDiacritics bool
}

// createFTSSchema creates FTS5 virtual tables for full-text search
func (s *SQLiteStorage) createFTSSchema() error {
	schema := `
	-- FTS5 virtual table for entity search
	CREATE VIRTUAL TABLE IF NOT EXISTS entities_fts USING fts5(
		name, 
		entity_type, 
		content='entities', 
		content_rowid='id',
		tokenize='porter unicode61 remove_diacritics 1'
	);

	-- FTS5 virtual table for observation search
	CREATE VIRTUAL TABLE IF NOT EXISTS observations_fts USING fts5(
		content,
		entity_name,
		content='observations',
		content_rowid='id',
		tokenize='porter unicode61 remove_diacritics 1'
	);

	-- Triggers to keep FTS tables in sync
	CREATE TRIGGER IF NOT EXISTS entities_fts_insert AFTER INSERT ON entities BEGIN
		INSERT INTO entities_fts(rowid, name, entity_type) VALUES (new.id, new.name, new.entity_type);
	END;

	CREATE TRIGGER IF NOT EXISTS entities_fts_delete AFTER DELETE ON entities BEGIN
		INSERT INTO entities_fts(entities_fts, rowid, name, entity_type) VALUES('delete', old.id, old.name, old.entity_type);
	END;

	CREATE TRIGGER IF NOT EXISTS entities_fts_update AFTER UPDATE ON entities BEGIN
		INSERT INTO entities_fts(entities_fts, rowid, name, entity_type) VALUES('delete', old.id, old.name, old.entity_type);
		INSERT INTO entities_fts(rowid, name, entity_type) VALUES (new.id, new.name, new.entity_type);
	END;

	CREATE TRIGGER IF NOT EXISTS observations_fts_insert AFTER INSERT ON observations BEGIN
		INSERT INTO observations_fts(rowid, content, entity_name) 
		SELECT new.id, new.content, e.name FROM entities e WHERE e.id = new.entity_id;
	END;

	CREATE TRIGGER IF NOT EXISTS observations_fts_delete AFTER DELETE ON observations BEGIN
		INSERT INTO observations_fts(observations_fts, rowid, content, entity_name) 
		SELECT 'delete', old.id, old.content, e.name FROM entities e WHERE e.id = old.entity_id;
	END;

	CREATE TRIGGER IF NOT EXISTS observations_fts_update AFTER UPDATE ON observations BEGIN
		INSERT INTO observations_fts(observations_fts, rowid, content, entity_name) 
		SELECT 'delete', old.id, old.content, e.name FROM entities e WHERE e.id = old.entity_id;
		INSERT INTO observations_fts(rowid, content, entity_name) 
		SELECT new.id, new.content, e.name FROM entities e WHERE e.id = new.entity_id;
	END;
	`

	_, err := s.db.Exec(schema)
	if err != nil {
		return fmt.Errorf("failed to create FTS schema: %w", err)
	}

	// Skip FTS population for now - will be populated through triggers
	return nil
}

// rebuildFTSIndex rebuilds the FTS index
func (s *SQLiteStorage) rebuildFTSIndex() error {
	// First populate entities FTS manually
	_, err := s.db.Exec(`
		INSERT INTO entities_fts(rowid, name, entity_type)
		SELECT id, name, entity_type FROM entities
		WHERE id NOT IN (SELECT rowid FROM entities_fts)
	`)
	if err != nil {
		// Try rebuild if manual insert fails
		_, err = s.db.Exec("INSERT INTO entities_fts(entities_fts) VALUES('rebuild')")
		if err != nil {
			return fmt.Errorf("failed to rebuild entities FTS: %w", err)
		}
	}

	// Populate observations FTS manually
	_, err = s.db.Exec(`
		INSERT INTO observations_fts(rowid, content, entity_name)
		SELECT o.id, o.content, e.name 
		FROM observations o
		JOIN entities e ON o.entity_id = e.id
		WHERE o.id NOT IN (SELECT rowid FROM observations_fts)
	`)
	if err != nil {
		// Try rebuild if manual insert fails
		_, err = s.db.Exec("INSERT INTO observations_fts(observations_fts) VALUES('rebuild')")
		if err != nil {
			return fmt.Errorf("failed to rebuild observations FTS: %w", err)
		}
	}

	return nil
}

// SearchNodesWithFTS searches using FTS5 for better performance and results
func (s *SQLiteStorage) SearchNodesWithFTS(query string) (*KnowledgeGraph, error) {
	graph := &KnowledgeGraph{
		Entities:  []Entity{},
		Relations: []Relation{},
	}

	if query == "" {
		return graph, nil
	}

	// Prepare FTS query (escape special characters and add quotes for phrase search)
	ftsQuery := prepareFTSQuery(query)

	// Search entities using FTS
	entityQuery := `
		SELECT DISTINCT e.id, e.name, e.entity_type,
		       GROUP_CONCAT(o.content, '|||') as observations,
		       bm25(ef) as rank
		FROM entities_fts ef
		JOIN entities e ON ef.rowid = e.id
		LEFT JOIN observations o ON e.id = o.entity_id
		WHERE entities_fts MATCH ?
		GROUP BY e.id, e.name, e.entity_type
		ORDER BY rank
		LIMIT 100
	`

	entityRows, err := s.db.Query(entityQuery, ftsQuery)
	if err != nil {
		// Return error to allow fallback to basic search
		return nil, fmt.Errorf("FTS entity search failed: %w", err)
	}
	defer entityRows.Close()

	entityIDs := []int64{}
	entityMap := make(map[int64]Entity)

	for entityRows.Next() {
		var id int64
		var name, entityType string
		var obsStr sql.NullString
		var rank float64

		if err := entityRows.Scan(&id, &name, &entityType, &obsStr, &rank); err != nil {
			continue
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

		entityMap[id] = entity
	}

	// Search observations using FTS
	obsQuery := `
		SELECT DISTINCT e.id, e.name, e.entity_type,
		       GROUP_CONCAT(o.content, '|||') as observations,
		       bm25(of) as rank
		FROM observations_fts of
		JOIN observations o ON of.rowid = o.id
		JOIN entities e ON o.entity_id = e.id
		WHERE observations_fts MATCH ?
		GROUP BY e.id, e.name, e.entity_type
		ORDER BY rank
		LIMIT 100
	`

	obsRows, err := s.db.Query(obsQuery, ftsQuery)
	if err == nil {
		defer obsRows.Close()

		for obsRows.Next() {
			var id int64
			var name, entityType string
			var obsStr sql.NullString
			var rank float64

			if err := obsRows.Scan(&id, &name, &entityType, &obsStr, &rank); err != nil {
				continue
			}

			// Add to results if not already found
			if _, exists := entityMap[id]; !exists {
				entityIDs = append(entityIDs, id)

				entity := Entity{
					Name:         name,
					EntityType:   entityType,
					Observations: []string{},
				}

				if obsStr.Valid && obsStr.String != "" {
					entity.Observations = strings.Split(obsStr.String, "|||")
				}

				entityMap[id] = entity
			}
		}
	}

	// Convert map to slice
	for _, entity := range entityMap {
		graph.Entities = append(graph.Entities, entity)
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
		if err == nil {
			defer rows.Close()

			for rows.Next() {
				var from, to, relType string
				if err := rows.Scan(&from, &to, &relType); err != nil {
					continue
				}

				graph.Relations = append(graph.Relations, Relation{
					From:         from,
					To:           to,
					RelationType: relType,
				})
			}
		}
	}

	return graph, nil
}

// prepareFTSQuery prepares a query string for FTS5
func prepareFTSQuery(query string) string {
	// Escape special FTS characters
	query = strings.ReplaceAll(query, `"`, `""`)

	// Split into words and create a phrase query or AND query
	words := strings.Fields(query)
	if len(words) == 0 {
		return `""`
	}

	if len(words) == 1 {
		// Single word - use prefix matching
		return fmt.Sprintf(`%s*`, words[0])
	}

	// Multiple words - try phrase search first, fallback to AND
	if len(strings.Join(words, " ")) < 50 { // Reasonable phrase length
		return fmt.Sprintf(`"%s"`, strings.Join(words, " "))
	}

	// Long query - use AND of individual words
	return strings.Join(words, " AND ")
}

// GetSearchSuggestions provides search suggestions based on partial input
func (s *SQLiteStorage) GetSearchSuggestions(partial string, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 10
	}

	suggestions := []string{}

	// Get entity name suggestions
	query := `
		SELECT DISTINCT name
		FROM entities
		WHERE name LIKE ?
		ORDER BY name
		LIMIT ?
	`

	rows, err := s.db.Query(query, partial+"%", limit/2)
	if err != nil {
		return suggestions, err
	}
	defer rows.Close()

	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err == nil {
			suggestions = append(suggestions, name)
		}
	}

	// Get entity type suggestions
	query = `
		SELECT DISTINCT entity_type
		FROM entities
		WHERE entity_type LIKE ?
		ORDER BY entity_type
		LIMIT ?
	`

	rows, err = s.db.Query(query, partial+"%", limit-len(suggestions))
	if err != nil {
		return suggestions, err
	}
	defer rows.Close()

	for rows.Next() {
		var entityType string
		if err := rows.Scan(&entityType); err == nil {
			suggestions = append(suggestions, entityType)
		}
	}

	return suggestions, nil
}

// AnalyzeGraph provides analytics about the knowledge graph
func (s *SQLiteStorage) AnalyzeGraph() (map[string]interface{}, error) {
	analysis := make(map[string]interface{})

	// Total counts
	var entityCount, relationCount, observationCount int

	err := s.db.QueryRow("SELECT COUNT(*) FROM entities").Scan(&entityCount)
	if err != nil {
		return nil, err
	}

	err = s.db.QueryRow("SELECT COUNT(*) FROM relations").Scan(&relationCount)
	if err != nil {
		return nil, err
	}

	err = s.db.QueryRow("SELECT COUNT(*) FROM observations").Scan(&observationCount)
	if err != nil {
		return nil, err
	}

	analysis["entity_count"] = entityCount
	analysis["relation_count"] = relationCount
	analysis["observation_count"] = observationCount

	// Entity type distribution
	entityTypes := make(map[string]int)
	rows, err := s.db.Query("SELECT entity_type, COUNT(*) FROM entities GROUP BY entity_type ORDER BY COUNT(*) DESC")
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var entityType string
			var count int
			if err := rows.Scan(&entityType, &count); err == nil {
				entityTypes[entityType] = count
			}
		}
	}
	analysis["entity_types"] = entityTypes

	// Relation type distribution
	relationTypes := make(map[string]int)
	rows, err = s.db.Query("SELECT relation_type, COUNT(*) FROM relations GROUP BY relation_type ORDER BY COUNT(*) DESC")
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var relationType string
			var count int
			if err := rows.Scan(&relationType, &count); err == nil {
				relationTypes[relationType] = count
			}
		}
	}
	analysis["relation_types"] = relationTypes

	// Most connected entities
	connectedEntities := []map[string]interface{}{}
	rows, err = s.db.Query(`
		SELECT e.name, e.entity_type, 
		       COUNT(DISTINCT r1.id) + COUNT(DISTINCT r2.id) as connection_count
		FROM entities e
		LEFT JOIN relations r1 ON e.id = r1.from_entity_id
		LEFT JOIN relations r2 ON e.id = r2.to_entity_id
		GROUP BY e.id, e.name, e.entity_type
		HAVING connection_count > 0
		ORDER BY connection_count DESC
		LIMIT 10
	`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var name, entityType string
			var connectionCount int
			if err := rows.Scan(&name, &entityType, &connectionCount); err == nil {
				connectedEntities = append(connectedEntities, map[string]interface{}{
					"name":             name,
					"entity_type":      entityType,
					"connection_count": connectionCount,
				})
			}
		}
	}
	analysis["most_connected"] = connectedEntities

	return analysis, nil
}
