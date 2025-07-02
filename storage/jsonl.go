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

// ReadGraph returns the entire knowledge graph
func (j *JSONLStorage) ReadGraph() (*KnowledgeGraph, error) {
	return j.loadGraph()
}

// SearchNodes searches for nodes containing the query string
func (j *JSONLStorage) SearchNodes(query string) (*KnowledgeGraph, error) {
	fullGraph, err := j.loadGraph()
	if err != nil {
		return nil, err
	}
	
	if query == "" {
		return &KnowledgeGraph{Entities: []Entity{}, Relations: []Relation{}}, nil
	}
	
	queryLower := strings.ToLower(query)
	result := &KnowledgeGraph{
		Entities:  []Entity{},
		Relations: []Relation{},
	}
	
	// Search entities
	matchedEntityNames := make(map[string]bool)
	for _, entity := range fullGraph.Entities {
		matched := false
		
		// Check name
		if strings.Contains(strings.ToLower(entity.Name), queryLower) {
			matched = true
		}
		
		// Check type
		if !matched && strings.Contains(strings.ToLower(entity.EntityType), queryLower) {
			matched = true
		}
		
		// Check observations
		if !matched {
			for _, obs := range entity.Observations {
				if strings.Contains(strings.ToLower(obs), queryLower) {
					matched = true
					break
				}
			}
		}
		
		if matched {
			result.Entities = append(result.Entities, entity)
			matchedEntityNames[entity.Name] = true
		}
	}
	
	// Include relations involving matched entities
	for _, relation := range fullGraph.Relations {
		if matchedEntityNames[relation.From] || matchedEntityNames[relation.To] {
			result.Relations = append(result.Relations, relation)
		}
	}
	
	return result, nil
}

// OpenNodes retrieves specific nodes by name
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
	
	// Get requested entities
	for _, entity := range fullGraph.Entities {
		if nameSet[entity.Name] {
			result.Entities = append(result.Entities, entity)
		}
	}
	
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