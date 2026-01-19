package storage

import (
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

// SearchNodesWithFTS searches using FTS5 and returns search hits with snippets
// Results are sorted by match location priority: name/type matches before content matches
func (s *SQLiteStorage) SearchNodesWithFTS(query string, limit int) (*SearchResult, error) {
	result := &SearchResult{
		Entities: []EntitySearchHit{},
		Limit:    limit,
	}

	if query == "" {
		return result, nil
	}

	// Prepare FTS query (escape special characters and add quotes for phrase search)
	ftsQuery := prepareFTSQuery(query)
	words := strings.Fields(query)

	// Use a map to track unique entities (by ID to avoid duplicates)
	// Track match source: entity FTS (name/type) has higher priority than observation FTS
	type entityInfo struct {
		ID            int64
		Name          string
		EntityType    string
		Rank          float64
		MatchedInName bool // true if matched in entities_fts (name/type)
	}
	entityMap := make(map[int64]*entityInfo)
	var nameMatchIDs []int64    // IDs matched in name/type (higher priority)
	var contentMatchIDs []int64 // IDs matched only in observations (lower priority)

	// Search entities using FTS (matches in name or entity_type)
	entityQuery := `
		SELECT DISTINCT e.id, e.name, e.entity_type, bm25(ef) as rank
		FROM entities_fts ef
		JOIN entities e ON ef.rowid = e.id
		WHERE entities_fts MATCH ?
		ORDER BY rank
	`

	entityRows, err := s.db.Query(entityQuery, ftsQuery)
	if err != nil {
		// Return error to allow fallback to basic search
		return nil, fmt.Errorf("FTS entity search failed: %w", err)
	}
	defer entityRows.Close()

	for entityRows.Next() {
		var id int64
		var name, entityType string
		var rank float64

		if err := entityRows.Scan(&id, &name, &entityType, &rank); err != nil {
			continue
		}

		if _, exists := entityMap[id]; !exists {
			entityMap[id] = &entityInfo{
				ID:            id,
				Name:          name,
				EntityType:    entityType,
				Rank:          rank,
				MatchedInName: true, // Matched in entities_fts
			}
			nameMatchIDs = append(nameMatchIDs, id)
		}
	}

	// Search observations using FTS (matches in observation content)
	obsQuery := `
		SELECT DISTINCT e.id, e.name, e.entity_type, bm25(of) as rank
		FROM observations_fts of
		JOIN observations o ON of.rowid = o.id
		JOIN entities e ON o.entity_id = e.id
		WHERE observations_fts MATCH ?
		ORDER BY rank
	`

	obsRows, err := s.db.Query(obsQuery, ftsQuery)
	if err == nil {
		defer obsRows.Close()

		for obsRows.Next() {
			var id int64
			var name, entityType string
			var rank float64

			if err := obsRows.Scan(&id, &name, &entityType, &rank); err != nil {
				continue
			}

			// Add to results if not already found from entity search
			if _, exists := entityMap[id]; !exists {
				entityMap[id] = &entityInfo{
					ID:            id,
					Name:          name,
					EntityType:    entityType,
					Rank:          rank,
					MatchedInName: false, // Only matched in observations
				}
				contentMatchIDs = append(contentMatchIDs, id)
			}
		}
	}

	// Calculate total
	result.Total = len(entityMap)

	// Combine IDs with name matches first, then content matches
	// This ensures entities matched by name/type appear before those matched only by content
	orderedIDs := append(nameMatchIDs, contentMatchIDs...)

	// Apply limit to ordered IDs (only if limit > 0)
	limitedIDs := orderedIDs
	if limit > 0 && len(limitedIDs) > limit {
		limitedIDs = limitedIDs[:limit]
	}

	// Get snippets, observations count, and relations count for each entity
	if len(limitedIDs) > 0 {
		// Build placeholders for entity IDs
		placeholders := make([]string, len(limitedIDs))
		idArgs := make([]interface{}, len(limitedIDs))
		for i, id := range limitedIDs {
			placeholders[i] = "?"
			idArgs[i] = id
		}
		placeholderStr := strings.Join(placeholders, ",")

		// Get observations count for each entity
		obsCountMap := make(map[int64]int)
		obsCountQuery := fmt.Sprintf(`
			SELECT entity_id, COUNT(*) 
			FROM observations 
			WHERE entity_id IN (%s) 
			GROUP BY entity_id
		`, placeholderStr)
		obsCountRows, err := s.db.Query(obsCountQuery, idArgs...)
		if err == nil {
			defer obsCountRows.Close()
			for obsCountRows.Next() {
				var entityID int64
				var count int
				if err := obsCountRows.Scan(&entityID, &count); err == nil {
					obsCountMap[entityID] = count
				}
			}
		}

		// Get relations count for each entity
		relCountMap := make(map[int64]int)
		relCountQuery := fmt.Sprintf(`
			SELECT e.id, COUNT(DISTINCT r.id)
			FROM entities e
			LEFT JOIN relations r ON e.id = r.from_entity_id OR e.id = r.to_entity_id
			WHERE e.id IN (%s)
			GROUP BY e.id
		`, placeholderStr)
		relCountRows, err := s.db.Query(relCountQuery, idArgs...)
		if err == nil {
			defer relCountRows.Close()
			for relCountRows.Next() {
				var entityID int64
				var count int
				if err := relCountRows.Scan(&entityID, &count); err == nil {
					relCountMap[entityID] = count
				}
			}
		}

		// Build result with snippets
		// maxSnippets=0 means return all matched snippets when limit=0
		maxSnippets := 2
		if limit == 0 {
			maxSnippets = 0 // unlimited snippets
		}
		for _, id := range limitedIDs {
			info := entityMap[id]
			hit := EntitySearchHit{
				Name:              info.Name,
				EntityType:        info.EntityType,
				Snippets:          s.getMatchedSnippets(id, words, maxSnippets, 50), // 50 chars context
				ObservationsCount: obsCountMap[id],
				RelationsCount:    relCountMap[id],
			}
			result.Entities = append(result.Entities, hit)
		}
	}

	// HasMore is only true when limit is specified and there are more results
	if limit > 0 {
		result.HasMore = result.Total > limit
	} else {
		result.HasMore = false // no limit means all results returned
	}

	return result, nil
}

// prepareFTSQuery prepares a query string for FTS5
// Multiple space-separated words are treated as OR search with prefix matching
func prepareFTSQuery(query string) string {
	// Escape special FTS characters
	query = strings.ReplaceAll(query, `"`, `""`)

	// Split into words
	words := strings.Fields(query)
	if len(words) == 0 {
		return `""`
	}

	if len(words) == 1 {
		// Single word - use prefix matching
		return fmt.Sprintf(`%s*`, words[0])
	}

	// Multiple words - use OR with prefix matching for each word
	// This allows "十里 田野 开发者" to find entities matching ANY of these keywords
	var parts []string
	for _, word := range words {
		parts = append(parts, fmt.Sprintf(`%s*`, word))
	}
	return strings.Join(parts, " OR ")
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
