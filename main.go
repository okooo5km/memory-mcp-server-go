package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"slices"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// Data structure definitions for knowledge graph
type Entity struct {
	Type         string   `json:"type"`
	Name         string   `json:"name"`
	EntityType   string   `json:"entityType"`
	Observations []string `json:"observations"`
}

type Relation struct {
	Type         string `json:"type"`
	From         string `json:"from"`
	To           string `json:"to"`
	RelationType string `json:"relationType"`
}

type KnowledgeGraph struct {
	Entities  []Entity   `json:"entities"`
	Relations []Relation `json:"relations"`
}

// KnowledgeGraphManager contains all operations for interacting with the knowledge graph
type KnowledgeGraphManager struct {
	memoryPath string
}

// Create a new KnowledgeGraphManager
func NewKnowledgeGraphManager(memory string) *KnowledgeGraphManager {
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

	return &KnowledgeGraphManager{
		memoryPath: memoryPath,
	}
}

// Load graph data
func (m *KnowledgeGraphManager) loadGraph() (KnowledgeGraph, error) {
	graph := KnowledgeGraph{
		Entities:  []Entity{},
		Relations: []Relation{},
	}

	// Check if file exists
	_, err := os.Stat(m.memoryPath)
	if os.IsNotExist(err) {
		return graph, nil
	}

	// Read file content
	data, err := os.ReadFile(m.memoryPath)
	if err != nil {
		return graph, err
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

		var item map[string]any
		if err := json.Unmarshal([]byte(line), &item); err != nil {
			continue
		}

		itemType, ok := item["type"].(string)
		if !ok {
			continue
		}

		if itemType == "entity" {
			var entity Entity
			if err := json.Unmarshal([]byte(line), &entity); err == nil {
				graph.Entities = append(graph.Entities, entity)
			}
		} else if itemType == "relation" {
			var relation Relation
			if err := json.Unmarshal([]byte(line), &relation); err == nil {
				graph.Relations = append(graph.Relations, relation)
			}
		}
	}

	return graph, nil
}

// Save graph data
func (m *KnowledgeGraphManager) saveGraph(graph KnowledgeGraph) error {
	// Prepare data to save
	var lines []string
	for _, entity := range graph.Entities {
		entity.Type = "entity"
		data, err := json.Marshal(entity)
		if err != nil {
			continue
		}
		lines = append(lines, string(data))
	}

	for _, relation := range graph.Relations {
		relation.Type = "relation"
		data, err := json.Marshal(relation)
		if err != nil {
			continue
		}
		lines = append(lines, string(data))
	}

	// Save to file
	return os.WriteFile(m.memoryPath, []byte(strings.Join(lines, "\n")), 0644)
}

// CreateEntities creates multiple new entities
func (m *KnowledgeGraphManager) CreateEntities(entities []Entity) ([]Entity, error) {
	graph, err := m.loadGraph()
	if err != nil {
		return nil, err
	}

	var newEntities []Entity
	for _, entity := range entities {
		// Check if entity already exists
		exists := false
		for _, e := range graph.Entities {
			if e.Name == entity.Name {
				exists = true
				break
			}
		}
		if !exists {
			entity.Type = "entity"
			graph.Entities = append(graph.Entities, entity)
			newEntities = append(newEntities, entity)
		}
	}

	if err := m.saveGraph(graph); err != nil {
		return nil, err
	}
	return newEntities, nil
}

// CreateRelations creates multiple new relations
func (m *KnowledgeGraphManager) CreateRelations(relations []Relation) ([]Relation, error) {
	graph, err := m.loadGraph()
	if err != nil {
		return nil, err
	}

	var newRelations []Relation
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
			relation.Type = "relation"
			graph.Relations = append(graph.Relations, relation)
			newRelations = append(newRelations, relation)
		}
	}

	if err := m.saveGraph(graph); err != nil {
		return nil, err
	}
	return newRelations, nil
}

// AddObservations adds new observations to existing entities
type ObservationAddition struct {
	EntityName string   `json:"entityName"`
	Contents   []string `json:"contents"`
}

type ObservationAdditionResult struct {
	EntityName        string   `json:"entityName"`
	AddedObservations []string `json:"addedObservations"`
}

func (m *KnowledgeGraphManager) AddObservations(additions []ObservationAddition) ([]ObservationAdditionResult, error) {
	graph, err := m.loadGraph()
	if err != nil {
		return nil, err
	}

	var results []ObservationAdditionResult
	for _, addition := range additions {
		var result ObservationAdditionResult
		result.EntityName = addition.EntityName
		result.AddedObservations = []string{}

		// Find entity
		entityFound := false
		for i, entity := range graph.Entities {
			if entity.Name == addition.EntityName {
				entityFound = true

				// Add non-duplicate observations
				for _, content := range addition.Contents {
					exists := slices.Contains(entity.Observations, content)
					if !exists {
						graph.Entities[i].Observations = append(graph.Entities[i].Observations, content)
						result.AddedObservations = append(result.AddedObservations, content)
					}
				}
				break
			}
		}

		if !entityFound {
			return nil, fmt.Errorf("entity %s not found", addition.EntityName)
		}

		results = append(results, result)
	}

	if err := m.saveGraph(graph); err != nil {
		return nil, err
	}
	return results, nil
}

// DeleteEntities deletes multiple entities and their associated relations
func (m *KnowledgeGraphManager) DeleteEntities(entityNames []string) error {
	graph, err := m.loadGraph()
	if err != nil {
		return err
	}

	// Create a set for quick entity name lookup
	namesToDelete := make(map[string]bool)
	for _, name := range entityNames {
		namesToDelete[name] = true
	}

	// Filter entities
	var filteredEntities []Entity
	for _, entity := range graph.Entities {
		if !namesToDelete[entity.Name] {
			filteredEntities = append(filteredEntities, entity)
		}
	}
	graph.Entities = filteredEntities

	// Filter relations
	var filteredRelations []Relation
	for _, relation := range graph.Relations {
		if !namesToDelete[relation.From] && !namesToDelete[relation.To] {
			filteredRelations = append(filteredRelations, relation)
		}
	}
	graph.Relations = filteredRelations

	return m.saveGraph(graph)
}

// DeleteObservations deletes specific observations from entities
type ObservationDeletion struct {
	EntityName   string   `json:"entityName"`
	Observations []string `json:"observations"`
}

func (m *KnowledgeGraphManager) DeleteObservations(deletions []ObservationDeletion) error {
	graph, err := m.loadGraph()
	if err != nil {
		return err
	}

	for _, deletion := range deletions {
		// Find entity
		for i, entity := range graph.Entities {
			if entity.Name == deletion.EntityName {
				// Create set of observations to delete
				obsToDelete := make(map[string]bool)
				for _, obs := range deletion.Observations {
					obsToDelete[obs] = true
				}

				// Filter observations
				var filteredObs []string
				for _, obs := range entity.Observations {
					if !obsToDelete[obs] {
						filteredObs = append(filteredObs, obs)
					}
				}
				graph.Entities[i].Observations = filteredObs
				break
			}
		}
	}

	return m.saveGraph(graph)
}

// DeleteRelations deletes multiple relations
func (m *KnowledgeGraphManager) DeleteRelations(relations []Relation) error {
	graph, err := m.loadGraph()
	if err != nil {
		return err
	}

	var filteredRelations []Relation
	for _, r1 := range graph.Relations {
		shouldKeep := true
		for _, r2 := range relations {
			if r1.From == r2.From && r1.To == r2.To && r1.RelationType == r2.RelationType {
				shouldKeep = false
				break
			}
		}
		if shouldKeep {
			filteredRelations = append(filteredRelations, r1)
		}
	}
	graph.Relations = filteredRelations

	return m.saveGraph(graph)
}

// ReadGraph reads the entire knowledge graph
func (m *KnowledgeGraphManager) ReadGraph() (KnowledgeGraph, error) {
	return m.loadGraph()
}

// SearchNodes searches for nodes in the knowledge graph based on a query
func (m *KnowledgeGraphManager) SearchNodes(query string) (KnowledgeGraph, error) {
	graph, err := m.loadGraph()
	if err != nil {
		return KnowledgeGraph{}, err
	}

	query = strings.ToLower(query)
	var filteredEntities []Entity

	// Filter entities
	for _, entity := range graph.Entities {
		if strings.Contains(strings.ToLower(entity.Name), query) ||
			strings.Contains(strings.ToLower(entity.EntityType), query) {
			filteredEntities = append(filteredEntities, entity)
			continue
		}

		// Check observations
		for _, obs := range entity.Observations {
			if strings.Contains(strings.ToLower(obs), query) {
				filteredEntities = append(filteredEntities, entity)
				break
			}
		}
	}

	// Create a set for quick entity name lookup
	filteredEntityNames := make(map[string]bool)
	for _, entity := range filteredEntities {
		filteredEntityNames[entity.Name] = true
	}

	// Filter relations
	var filteredRelations []Relation
	for _, relation := range graph.Relations {
		if filteredEntityNames[relation.From] && filteredEntityNames[relation.To] {
			filteredRelations = append(filteredRelations, relation)
		}
	}

	return KnowledgeGraph{
		Entities:  filteredEntities,
		Relations: filteredRelations,
	}, nil
}

// OpenNodes opens specific nodes in the knowledge graph by their names
func (m *KnowledgeGraphManager) OpenNodes(names []string) (KnowledgeGraph, error) {
	graph, err := m.loadGraph()
	if err != nil {
		return KnowledgeGraph{}, err
	}

	// Create a set for quick name lookup
	nameSet := make(map[string]bool)
	for _, name := range names {
		nameSet[name] = true
	}

	// Filter entities
	var filteredEntities []Entity
	for _, entity := range graph.Entities {
		if nameSet[entity.Name] {
			filteredEntities = append(filteredEntities, entity)
		}
	}

	// Create a set for quick filtered entity name lookup
	filteredEntityNames := make(map[string]bool)
	for _, entity := range filteredEntities {
		filteredEntityNames[entity.Name] = true
	}

	// Filter relations
	var filteredRelations []Relation
	for _, relation := range graph.Relations {
		if filteredEntityNames[relation.From] && filteredEntityNames[relation.To] {
			filteredRelations = append(filteredRelations, relation)
		}
	}

	return KnowledgeGraph{
		Entities:  filteredEntities,
		Relations: filteredRelations,
	}, nil
}

func main() {

	var transport string
	var memory string
	var port int = 8080

	flag.StringVar(&transport, "transport", "stdio", "Transport type (stdio or sse)")
	flag.StringVar(&transport, "t", "stdio", "Transport type (stdio or sse)")
	flag.StringVar(&memory, "memory", "", "Path to memory file")
	flag.StringVar(&memory, "m", "", "Path to memory file")
	flag.IntVar(&port, "port", 8080, "Port for SSE transport")
	flag.IntVar(&port, "p", 8080, "Port for SSE transport")
	flag.Parse()

	// Create knowledge graph manager
	manager := NewKnowledgeGraphManager(memory)

	// Create a new MCP server
	s := server.NewMCPServer(
		"Memory Knowledge Graph",
		"0.1.0",
		server.WithResourceCapabilities(true, true),
		server.WithLogging(),
	)

	// Add create_entities tool
	createEntitiesTool := mcp.NewTool("create_entities",
		mcp.WithDescription("Create multiple new entities in the knowledge graph"),
		mcp.WithArray("entities",
			mcp.Required(),
			mcp.Description("An array of entities to create"),
			mcp.Items(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{
						"type":        "string",
						"description": "The name of the entity",
					},
					"entityType": map[string]any{
						"type":        "string",
						"description": "The type of the entity",
					},
					"observations": map[string]any{
						"type":        "array",
						"description": "An array of observation contents associated with the entity",
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
		mcp.WithDescription("Create multiple new relations between entities in the knowledge graph. Relations should be in active voice"),
		mcp.WithArray("relations",
			mcp.Required(),
			mcp.Description("An array of relations to create"),
			mcp.Items(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"from": map[string]any{
						"type":        "string",
						"description": "The name of the entity where the relation starts",
					},
					"to": map[string]any{
						"type":        "string",
						"description": "The name of the entity where the relation ends",
					},
					"relationType": map[string]any{
						"type":        "string",
						"description": "The type of the relation",
					},
				},
				"required": []string{"from", "to", "relationType"},
			}),
		),
	)

	// Add add_observations tool
	addObservationsTool := mcp.NewTool("add_observations",
		mcp.WithDescription("Add new observations to existing entities in the knowledge graph"),
		mcp.WithArray("observations",
			mcp.Required(),
			mcp.Description("An array of observations to add to entities"),
			mcp.Items(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"entityName": map[string]any{
						"type":        "string",
						"description": "The name of the entity to add the observations to",
					},
					"contents": map[string]any{
						"type":        "array",
						"description": "An array of observation contents to add",
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
		mcp.WithDescription("Delete multiple entities and their associated relations from the knowledge graph"),
		mcp.WithArray("entityNames",
			mcp.Required(),
			mcp.Description("An array of entity names to delete"),
			mcp.Items(map[string]any{
				"type": "string",
			}),
		),
	)

	// Add delete_observations tool
	deleteObservationsTool := mcp.NewTool("delete_observations",
		mcp.WithDescription("Delete specific observations from entities in the knowledge graph"),
		mcp.WithArray("deletions",
			mcp.Required(),
			mcp.Description("An array of observations to delete from entities"),
			mcp.Items(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"entityName": map[string]any{
						"type":        "string",
						"description": "The name of the entity containing the observations",
					},
					"observations": map[string]any{
						"type":        "array",
						"description": "An array of observations to delete",
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
		mcp.WithDescription("Delete multiple relations from the knowledge graph"),
		mcp.WithArray("relations",
			mcp.Required(),
			mcp.Description("An array of relations to delete"),
			mcp.Items(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"from": map[string]any{
						"type":        "string",
						"description": "The name of the entity where the relation starts",
					},
					"to": map[string]any{
						"type":        "string",
						"description": "The name of the entity where the relation ends",
					},
					"relationType": map[string]any{
						"type":        "string",
						"description": "The type of the relation",
					},
				},
				"required": []string{"from", "to", "relationType"},
			}),
		),
	)

	// Add read_graph tool
	readGraphTool := mcp.NewTool("read_graph",
		mcp.WithDescription("Read the entire knowledge graph"),
	)

	// Add search_nodes tool
	searchNodesTool := mcp.NewTool("search_nodes",
		mcp.WithDescription("Search for nodes in the knowledge graph based on a query"),
		mcp.WithString("query",
			mcp.Required(),
			mcp.Description("The search query to match against entity names, types, and observation content"),
		),
	)

	// Add open_nodes tool
	openNodesTool := mcp.NewTool("open_nodes",
		mcp.WithDescription("Open specific nodes in the knowledge graph by their names"),
		mcp.WithArray("names",
			mcp.Required(),
			mcp.Description("An array of entity names to retrieve"),
			mcp.Items(map[string]any{
				"type": "string",
			}),
		),
	)

	// Add handlers
	s.AddTool(createEntitiesTool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := request.Params.Arguments["entities"]
		if args == nil {
			return nil, errors.New("missing required parameter: entities")
		}

		// Convert parameters to entity list
		var entities []Entity
		data, err := json.Marshal(args)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &entities); err != nil {
			return nil, err
		}

		// Create entities
		newEntities, err := manager.CreateEntities(entities)
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
		args := request.Params.Arguments["relations"]
		if args == nil {
			return nil, errors.New("missing required parameter: relations")
		}

		// Convert parameters to relation list
		var relations []Relation
		data, err := json.Marshal(args)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &relations); err != nil {
			return nil, err
		}

		// Create relations
		newRelations, err := manager.CreateRelations(relations)
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
		args := request.Params.Arguments["observations"]
		if args == nil {
			return nil, errors.New("missing required parameter: observations")
		}

		// Convert parameters to observation addition list
		var observations []ObservationAddition
		data, err := json.Marshal(args)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &observations); err != nil {
			return nil, err
		}

		// Add observations
		results, err := manager.AddObservations(observations)
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
		args := request.Params.Arguments["entityNames"]
		if args == nil {
			return nil, errors.New("missing required parameter: entityNames")
		}

		// Convert parameters to entity name list
		var entityNames []string
		data, err := json.Marshal(args)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &entityNames); err != nil {
			return nil, err
		}

		// Delete entities
		if err := manager.DeleteEntities(entityNames); err != nil {
			return nil, err
		}

		return mcp.NewToolResultText("Entities deleted successfully"), nil
	})

	s.AddTool(deleteObservationsTool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := request.Params.Arguments["deletions"]
		if args == nil {
			return nil, errors.New("missing required parameter: deletions")
		}

		// Convert parameters to observation deletion list
		var deletions []ObservationDeletion
		data, err := json.Marshal(args)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &deletions); err != nil {
			return nil, err
		}

		// Delete observations
		if err := manager.DeleteObservations(deletions); err != nil {
			return nil, err
		}

		return mcp.NewToolResultText("Observations deleted successfully"), nil
	})

	s.AddTool(deleteRelationsTool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := request.Params.Arguments["relations"]
		if args == nil {
			return nil, errors.New("missing required parameter: relations")
		}

		// Convert parameters to relation list
		var relations []Relation
		data, err := json.Marshal(args)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &relations); err != nil {
			return nil, err
		}

		// Delete relations
		if err := manager.DeleteRelations(relations); err != nil {
			return nil, err
		}

		return mcp.NewToolResultText("Relations deleted successfully"), nil
	})

	s.AddTool(readGraphTool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// Read the entire graph
		graph, err := manager.ReadGraph()
		if err != nil {
			return nil, err
		}

		// Convert result to JSON
		resultJSON, err := json.MarshalIndent(graph, "", "  ")
		if err != nil {
			return nil, err
		}

		return mcp.NewToolResultText(string(resultJSON)), nil
	})

	s.AddTool(searchNodesTool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		query, ok := request.Params.Arguments["query"].(string)
		if !ok {
			return nil, errors.New("missing required parameter: query")
		}

		// Search nodes
		results, err := manager.SearchNodes(query)
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
		args := request.Params.Arguments["names"]
		if args == nil {
			return nil, errors.New("missing required parameter: names")
		}

		// Convert parameters to name list
		var names []string
		data, err := json.Marshal(args)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &names); err != nil {
			return nil, err
		}

		// Open nodes
		results, err := manager.OpenNodes(names)
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

	if transport == "stdio" {
		fmt.Fprintln(os.Stderr, "Knowledge Graph MCP Server running on stdio")
		if err := server.ServeStdio(s); err != nil {
			fmt.Fprintf(os.Stderr, "Server error: %v\n", err)
		}
	} else if transport == "sse" {
		fmt.Fprintln(os.Stderr, "Knowledge Graph MCP Server running on SSE")
		sseServer := server.NewSSEServer(s, server.WithBaseURL(fmt.Sprintf("http://localhost:%d", port)))
		log.Printf("Server started listening on :%d\n", port)
		if err := sseServer.Start(fmt.Sprintf(":%d", port)); err != nil {
			log.Fatalf("Failed to start server: %v", err)
		}
	}
}
