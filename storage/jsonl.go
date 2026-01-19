package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// JSONLStorage implements Storage interface using JSONL file format
type JSONLStorage struct {
	config Config
}

// NewJSONLStorage creates a new JSONL storage instance
func NewJSONLStorage(config Config) (*JSONLStorage, error) {
	return &JSONLStorage{config: config}, nil
}

// Initialize prepares the JSONL storage
func (j *JSONLStorage) Initialize() error {
	// Ensure directory exists
	dir := filepath.Dir(j.config.FilePath)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create directory: %w", err)
		}
	}

	// Create file if it doesn't exist
	if _, err := os.Stat(j.config.FilePath); os.IsNotExist(err) {
		file, err := os.Create(j.config.FilePath)
		if err != nil {
			return fmt.Errorf("failed to create file: %w", err)
		}
		file.Close()
	}

	return nil
}

// Close cleans up resources
func (j *JSONLStorage) Close() error {
	// No resources to clean up for file-based storage
	return nil
}

// loadGraph loads the knowledge graph from JSONL file
func (j *JSONLStorage) loadGraph() (*KnowledgeGraph, error) {
	graph := &KnowledgeGraph{
		Entities:  []Entity{},
		Relations: []Relation{},
	}

	// Check if file exists
	if _, err := os.Stat(j.config.FilePath); os.IsNotExist(err) {
		return graph, nil
	}

	// Read file content
	data, err := os.ReadFile(j.config.FilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	if len(data) == 0 {
		return graph, nil
	}

	// Parse line by line
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// First check the type field
		var item map[string]interface{}
		if err := json.Unmarshal([]byte(line), &item); err != nil {
			continue
		}

		itemType, ok := item["type"].(string)
		if !ok {
			continue
		}

		if itemType == "entity" {
			var entity jsonlEntity
			if err := json.Unmarshal([]byte(line), &entity); err == nil {
				graph.Entities = append(graph.Entities, Entity{
					Name:         entity.Name,
					EntityType:   entity.EntityType,
					Observations: entity.Observations,
				})
			}
		} else if itemType == "relation" {
			var relation jsonlRelation
			if err := json.Unmarshal([]byte(line), &relation); err == nil {
				graph.Relations = append(graph.Relations, Relation{
					From:         relation.From,
					To:           relation.To,
					RelationType: relation.RelationType,
				})
			}
		}
	}

	return graph, nil
}

// saveGraph saves the knowledge graph to JSONL file
func (j *JSONLStorage) saveGraph(graph *KnowledgeGraph) error {
	var lines []string

	// Convert entities
	for _, entity := range graph.Entities {
		jsonEntity := jsonlEntity{
			Type:         "entity",
			Name:         entity.Name,
			EntityType:   entity.EntityType,
			Observations: entity.Observations,
		}
		data, err := json.Marshal(jsonEntity)
		if err != nil {
			continue
		}
		lines = append(lines, string(data))
	}

	// Convert relations
	for _, relation := range graph.Relations {
		jsonRelation := jsonlRelation{
			Type:         "relation",
			From:         relation.From,
			To:           relation.To,
			RelationType: relation.RelationType,
		}
		data, err := json.Marshal(jsonRelation)
		if err != nil {
			continue
		}
		lines = append(lines, string(data))
	}

	// Save to file
	content := strings.Join(lines, "\n")
	if len(lines) > 0 {
		content += "\n"
	}

	return os.WriteFile(j.config.FilePath, []byte(content), 0644)
}

// CreateEntities creates new entities
func (j *JSONLStorage) CreateEntities(entities []Entity) ([]Entity, error) {
	graph, err := j.loadGraph()
	if err != nil {
		return nil, err
	}

	created := []Entity{}
	for _, entity := range entities {
		// Check if entity already exists
		exists := false
		for i, e := range graph.Entities {
			if e.Name == entity.Name {
				exists = true
				// Update entity type if changed
				graph.Entities[i].EntityType = entity.EntityType
				// Merge observations
				for _, obs := range entity.Observations {
					if !slices.Contains(graph.Entities[i].Observations, obs) {
						graph.Entities[i].Observations = append(graph.Entities[i].Observations, obs)
					}
				}
				created = append(created, graph.Entities[i])
				break
			}
		}

		if !exists {
			graph.Entities = append(graph.Entities, entity)
			created = append(created, entity)
		}
	}

	if err := j.saveGraph(graph); err != nil {
		return nil, err
	}

	return created, nil
}

// DeleteEntities deletes entities by name
func (j *JSONLStorage) DeleteEntities(names []string) error {
	graph, err := j.loadGraph()
	if err != nil {
		return err
	}

	// Create a set for quick lookup
	namesToDelete := make(map[string]bool)
	for _, name := range names {
		namesToDelete[name] = true
	}

	// Filter entities
	filteredEntities := []Entity{}
	for _, entity := range graph.Entities {
		if !namesToDelete[entity.Name] {
			filteredEntities = append(filteredEntities, entity)
		}
	}
	graph.Entities = filteredEntities

	// Filter relations (remove those involving deleted entities)
	filteredRelations := []Relation{}
	for _, relation := range graph.Relations {
		if !namesToDelete[relation.From] && !namesToDelete[relation.To] {
			filteredRelations = append(filteredRelations, relation)
		}
	}
	graph.Relations = filteredRelations

	return j.saveGraph(graph)
}

// CreateRelations creates new relations
func (j *JSONLStorage) CreateRelations(relations []Relation) ([]Relation, error) {
	graph, err := j.loadGraph()
	if err != nil {
		return nil, err
	}

	created := []Relation{}
	for _, relation := range relations {
		// Check if relation already exists
		exists := false
		for _, r := range graph.Relations {
			if r.From == relation.From && r.To == relation.To && r.RelationType == relation.RelationType {
				exists = true
				break
			}
		}

		if !exists {
			graph.Relations = append(graph.Relations, relation)
			created = append(created, relation)
		}
	}

	if err := j.saveGraph(graph); err != nil {
		return nil, err
	}

	return created, nil
}

// DeleteRelations deletes specific relations
func (j *JSONLStorage) DeleteRelations(relations []Relation) error {
	graph, err := j.loadGraph()
	if err != nil {
		return err
	}

	// Create a set for relation lookup
	relationsToDelete := make(map[string]bool)
	for _, r := range relations {
		key := fmt.Sprintf("%s|%s|%s", r.From, r.To, r.RelationType)
		relationsToDelete[key] = true
	}

	// Filter relations
	filteredRelations := []Relation{}
	for _, relation := range graph.Relations {
		key := fmt.Sprintf("%s|%s|%s", relation.From, relation.To, relation.RelationType)
		if !relationsToDelete[key] {
			filteredRelations = append(filteredRelations, relation)
		}
	}
	graph.Relations = filteredRelations

	return j.saveGraph(graph)
}

// AddObservations adds observations to entities
func (j *JSONLStorage) AddObservations(observations map[string][]string) (map[string][]string, error) {
	graph, err := j.loadGraph()
	if err != nil {
		return nil, err
	}

	added := make(map[string][]string)

	for entityName, obsList := range observations {
		added[entityName] = []string{}

		// Find entity
		found := false
		for i, entity := range graph.Entities {
			if entity.Name == entityName {
				found = true

				// Add non-duplicate observations
				for _, obs := range obsList {
					if !slices.Contains(entity.Observations, obs) {
						graph.Entities[i].Observations = append(graph.Entities[i].Observations, obs)
						added[entityName] = append(added[entityName], obs)
					}
				}
				break
			}
		}

		if !found {
			return nil, fmt.Errorf("entity %s not found", entityName)
		}
	}

	if err := j.saveGraph(graph); err != nil {
		return nil, err
	}

	return added, nil
}

// DeleteObservations deletes specific observations
func (j *JSONLStorage) DeleteObservations(deletions []ObservationDeletion) error {
	graph, err := j.loadGraph()
	if err != nil {
		return err
	}

	for _, deletion := range deletions {
		// Find entity
		for i, entity := range graph.Entities {
			if entity.Name == deletion.EntityName {
				// Create set of observations to delete
				toDelete := make(map[string]bool)
				for _, obs := range deletion.Observations {
					toDelete[obs] = true
				}

				// Filter observations
				filteredObs := []string{}
				for _, obs := range entity.Observations {
					if !toDelete[obs] {
						filteredObs = append(filteredObs, obs)
					}
				}
				graph.Entities[i].Observations = filteredObs
				break
			}
		}
	}

	return j.saveGraph(graph)
}

// ReadGraph returns either a lightweight summary or full graph based on mode
func (j *JSONLStorage) ReadGraph(mode string, limit int) (interface{}, error) {
	graph, err := j.loadGraph()
	if err != nil {
		return nil, err
	}

	if mode == "full" {
		return graph, nil
	}

	// Summary mode
	summary := &GraphSummary{
		TotalEntities:  len(graph.Entities),
		TotalRelations: len(graph.Relations),
		EntityTypes:    make(map[string]int),
		RelationTypes:  make(map[string]int),
		Entities:       []EntitySummary{},
		Limit:          limit,
	}

	// Calculate entity type distribution
	for _, entity := range graph.Entities {
		summary.EntityTypes[entity.EntityType]++
	}

	// Calculate relation type distribution
	for _, relation := range graph.Relations {
		summary.RelationTypes[relation.RelationType]++
	}

	// Add entity summaries (limited)
	count := 0
	for _, entity := range graph.Entities {
		if count >= limit {
			break
		}
		summary.Entities = append(summary.Entities, EntitySummary{
			Name:       entity.Name,
			EntityType: entity.EntityType,
		})
		count++
	}

	summary.HasMore = summary.TotalEntities > limit

	return summary, nil
}

// Match priority constants for JSONL search ranking (same as SQLite)
const (
	jsonlPriorityNameExact   = 100 // Exact name match
	jsonlPriorityNamePartial = 80  // Partial name match
	jsonlPriorityType        = 50  // Entity type match
	jsonlPriorityContent     = 20  // Observations content match
)

// SearchNodes searches for nodes and returns search hits with context snippets
// Multiple space-separated words are treated as OR search
// Results are sorted by match priority: name exact > name partial > type > content
func (j *JSONLStorage) SearchNodes(query string, limit int) (*SearchResult, error) {
	fullGraph, err := j.loadGraph()
	if err != nil {
		return nil, err
	}

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

	// Convert words to lowercase for case-insensitive search
	lowerWords := make([]string, len(words))
	for i, word := range words {
		lowerWords[i] = strings.ToLower(word)
	}

	// Build entity name to relations count map
	relationsCountMap := make(map[string]int)
	for _, rel := range fullGraph.Relations {
		relationsCountMap[rel.From]++
		relationsCountMap[rel.To]++
	}

	// Determine max snippets per entity
	maxSnippets := 2
	if limit == 0 {
		maxSnippets = 0 // unlimited snippets when no limit
	}

	// Search entities - match if ANY word matches
	// Track priority for sorting
	type matchedEntity struct {
		entity          Entity
		matchedSnippets []string
		priority        int // Match priority for sorting
	}
	var matchedEntities []matchedEntity

	for _, entity := range fullGraph.Entities {
		matched := false
		var snippets []string
		priority := 0 // Track the highest priority match

		for _, queryWord := range lowerWords {
			lowerName := strings.ToLower(entity.Name)
			lowerType := strings.ToLower(entity.EntityType)

			// Check name - exact match (highest priority)
			if lowerName == queryWord {
				matched = true
				if jsonlPriorityNameExact > priority {
					priority = jsonlPriorityNameExact
				}
			} else if strings.Contains(lowerName, queryWord) {
				// Check name - partial match
				matched = true
				if jsonlPriorityNamePartial > priority {
					priority = jsonlPriorityNamePartial
				}
			}

			// Check type
			if strings.Contains(lowerType, queryWord) {
				matched = true
				if jsonlPriorityType > priority {
					priority = jsonlPriorityType
				}
			}

			// Check observations and collect context snippets around keywords
			for _, obs := range entity.Observations {
				if strings.Contains(strings.ToLower(obs), queryWord) {
					matched = true
					if jsonlPriorityContent > priority {
						priority = jsonlPriorityContent
					}
					// Add context snippet if within limit
					if maxSnippets == 0 || len(snippets) < maxSnippets {
						snippets = append(snippets, extractKeywordContextJSON(obs, words, 50))
					}
				}
			}
		}

		if matched {
			// If no matching snippets from observations, use first observations as fallback
			if len(snippets) == 0 && len(entity.Observations) > 0 {
				fallbackCount := 2
				if maxSnippets > 0 && maxSnippets < fallbackCount {
					fallbackCount = maxSnippets
				}
				for i := 0; i < fallbackCount && i < len(entity.Observations); i++ {
					snippets = append(snippets, truncateStringJSON(entity.Observations[i], 100))
				}
			}
			matchedEntities = append(matchedEntities, matchedEntity{
				entity:          entity,
				matchedSnippets: snippets,
				priority:        priority,
			})
		}
	}

	// Sort by priority (descending), then by name (ascending) for stable ordering
	slices.SortFunc(matchedEntities, func(a, b matchedEntity) int {
		if a.priority != b.priority {
			return b.priority - a.priority // Higher priority first
		}
		return strings.Compare(a.entity.Name, b.entity.Name) // Alphabetical
	})

	result.Total = len(matchedEntities)

	// Apply limit and build result (limit=0 means all)
	for i, me := range matchedEntities {
		if limit > 0 && i >= limit {
			break
		}
		result.Entities = append(result.Entities, EntitySearchHit{
			Name:              me.entity.Name,
			EntityType:        me.entity.EntityType,
			Snippets:          me.matchedSnippets,
			ObservationsCount: len(me.entity.Observations),
			RelationsCount:    relationsCountMap[me.entity.Name],
		})
	}

	// HasMore is only true when limit is specified and there are more results
	if limit > 0 {
		result.HasMore = result.Total > limit
	} else {
		result.HasMore = false // no limit means all results returned
	}

	return result, nil
}

// truncateStringJSON truncates a string to maxLen characters and adds "..." if truncated
func truncateStringJSON(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "..."
}

// extractKeywordContextJSON extracts a snippet with context around the first matched keyword
func extractKeywordContextJSON(content string, words []string, contextChars int) string {
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
		return truncateStringJSON(content, contextChars*2)
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

// OpenNodes retrieves specific nodes by name with truncation protection
const maxObservationsPerEntityJSONL = 100

func (j *JSONLStorage) OpenNodes(names []string) (*KnowledgeGraph, error) {
	fullGraph, err := j.loadGraph()
	if err != nil {
		return nil, err
	}

	if len(names) == 0 {
		return &KnowledgeGraph{Entities: []Entity{}, Relations: []Relation{}}, nil
	}

	// Create set for quick lookup
	nameSet := make(map[string]bool)
	for _, name := range names {
		nameSet[name] = true
	}

	result := &KnowledgeGraph{
		Entities:  []Entity{},
		Relations: []Relation{},
	}

	truncated := false

	// Get requested entities with truncation
	for _, entity := range fullGraph.Entities {
		if nameSet[entity.Name] {
			e := Entity{
				Name:         entity.Name,
				EntityType:   entity.EntityType,
				Observations: entity.Observations,
			}

			// Apply truncation if needed
			if len(e.Observations) > maxObservationsPerEntityJSONL {
				e.Observations = e.Observations[:maxObservationsPerEntityJSONL]
				truncated = true
			}

			result.Entities = append(result.Entities, e)
		}
	}

	result.Truncated = truncated

	// Get relations involving requested entities
	for _, relation := range fullGraph.Relations {
		if nameSet[relation.From] || nameSet[relation.To] {
			result.Relations = append(result.Relations, relation)
		}
	}

	return result, nil
}

// ExportData exports all data for migration
func (j *JSONLStorage) ExportData() (*KnowledgeGraph, error) {
	return j.loadGraph()
}

// ImportData imports data during migration
func (j *JSONLStorage) ImportData(graph *KnowledgeGraph) error {
	if graph == nil {
		return nil
	}
	return j.saveGraph(graph)
}

// jsonlEntity represents the JSONL format for entities
type jsonlEntity struct {
	Type         string   `json:"type"`
	Name         string   `json:"name"`
	EntityType   string   `json:"entityType"`
	Observations []string `json:"observations"`
}

// jsonlRelation represents the JSONL format for relations
type jsonlRelation struct {
	Type         string `json:"type"`
	From         string `json:"from"`
	To           string `json:"to"`
	RelationType string `json:"relationType"`
}
