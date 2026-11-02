// Copyright (c) 2025 okooo5km(十里)
// Licensed under the MIT License.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"runtime/debug"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"memory-mcp-server-go/storage"
)

// JSON schema helpers. The tool input schemas are written out by hand so the
// wire format stays identical to what earlier releases advertised.

func strSchema(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func numSchema(desc string) map[string]any {
	return map[string]any{"type": "number", "description": desc}
}

func strArraySchema(desc string) map[string]any {
	return map[string]any{
		"type":        "array",
		"description": desc,
		"items":       map[string]any{"type": "string"},
	}
}

func objectItems(props map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "object", "properties": props, "required": required}
}

func arrayOfObjects(desc string, items map[string]any) map[string]any {
	return map[string]any{"type": "array", "description": desc, "items": items}
}

func toolInput(props map[string]any, required ...string) map[string]any {
	schema := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func boolPtr(b bool) *bool { return &b }

func writeAnnotations(title string) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{Title: title, DestructiveHint: boolPtr(true)}
}

func readAnnotations(title string) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{Title: title, ReadOnlyHint: true, DestructiveHint: boolPtr(false)}
}

func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

func jsonResult(v any) (*mcp.CallToolResult, any, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	return textResult(string(data)), nil, nil
}

// recoveryMiddleware converts panics in any request handler into errors, so a
// bug in one tool call cannot take the whole process down.
func recoveryMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (res mcp.Result, err error) {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("panic while handling %s: %v\n%s", method, r, debug.Stack())
				res = nil
				err = fmt.Errorf("internal error while handling %s: %v", method, r)
			}
		}()
		return next(ctx, method, req)
	}
}

// Tool argument types.
type createEntitiesArgs struct {
	Entities []storage.Entity `json:"entities"`
}

type createRelationsArgs struct {
	Relations []storage.Relation `json:"relations"`
}

type addObservationsArgs struct {
	Observations []ObservationAddition `json:"observations"`
}

type deleteEntitiesArgs struct {
	EntityNames []string `json:"entityNames"`
}

type deleteObservationsArgs struct {
	Deletions []storage.ObservationDeletion `json:"deletions"`
}

type deleteRelationsArgs struct {
	Relations []storage.Relation `json:"relations"`
}

type readGraphArgs struct {
	Mode  *string `json:"mode"`
	Limit *int    `json:"limit"`
}

type searchNodesArgs struct {
	Query string `json:"query"`
	Limit *int   `json:"limit"`
}

type openNodesArgs struct {
	Names []string `json:"names"`
}

type mergeEntitiesArgs struct {
	SourceName string `json:"sourceName"`
	TargetName string `json:"targetName"`
}

type updateEntitiesArgs struct {
	Name       string `json:"name"`
	EntityType string `json:"entityType"`
}

type updateObservationsArgs struct {
	EntityName string `json:"entityName"`
	OldContent string `json:"oldContent"`
	NewContent string `json:"newContent"`
}

type detectConflictsArgs struct {
	EntityName *string `json:"entityName"`
}

// newMCPServer builds the MCP server (tools, resources and prompts) on top of
// the given knowledge graph manager.
func newMCPServer(manager *KnowledgeGraphManager) *mcp.Server {
	return newMCPServerWithKeepAlive(manager, 0)
}

// newMCPServerWithKeepAlive is newMCPServer with a server-to-client ping
// interval (0 disables pings).
func newMCPServerWithKeepAlive(manager *KnowledgeGraphManager, keepAlive time.Duration) *mcp.Server {
	// The default capabilities (logging, plus tools/prompts/resources inferred
	// from what is registered) are what we want; resource subscriptions are
	// not implemented so "subscribe" is not advertised.
	s := mcp.NewServer(&mcp.Implementation{Name: appName, Version: version}, &mcp.ServerOptions{
		KeepAlive: keepAlive,
	})
	s.AddReceivingMiddleware(recoveryMiddleware)

	addResources(s, manager)
	addPrompts(s, manager)
	addTools(s, manager)
	return s
}

func addResources(s *mcp.Server, manager *KnowledgeGraphManager) {
	// Resources allow AI clients to passively load memory context without
	// explicitly calling tools, improving memory awareness and utilization.

	// Resource: Knowledge Graph Summary
	s.AddResource(&mcp.Resource{
		URI:         "memory://graph/summary",
		Name:        "Knowledge Graph Summary",
		Description: "Overview of the knowledge graph including entity/relation counts, type distribution, and entity name list. Load this at the start of a conversation to understand what memories are available.",
		MIMEType:    "application/json",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		result, err := manager.ReadGraph("summary", 50)
		if err != nil {
			return nil, fmt.Errorf("failed to read graph summary: %w", err)
		}
		data, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("failed to marshal graph summary: %w", err)
		}
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{
			URI:      "memory://graph/summary",
			MIMEType: "application/json",
			Text:     string(data),
		}}}, nil
	})

	// Resource: Entity type and relation type distribution
	s.AddResource(&mcp.Resource{
		URI:         "memory://graph/types",
		Name:        "Entity & Relation Types",
		Description: "Lists all entity types and relation types currently in the knowledge graph with their counts. Useful for understanding the schema and maintaining consistent naming when creating new entities.",
		MIMEType:    "application/json",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		result, err := manager.ReadGraph("summary", 1)
		if err != nil {
			return nil, fmt.Errorf("failed to read graph types: %w", err)
		}
		// Extract just the type information from the summary
		summary, ok := result.(*storage.GraphSummary)
		if !ok {
			return nil, fmt.Errorf("unexpected result type from ReadGraph")
		}
		typeInfo := map[string]interface{}{
			"entityTypes":   summary.EntityTypes,
			"relationTypes": summary.RelationTypes,
		}
		data, err := json.MarshalIndent(typeInfo, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("failed to marshal type info: %w", err)
		}
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{
			URI:      "memory://graph/types",
			MIMEType: "application/json",
			Text:     string(data),
		}}}, nil
	})

	// Resource Template: Individual entity details
	s.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "memory://entities/{name}",
		Name:        "Entity Details",
		Description: "Get full details of a specific entity by name, including all observations and relations. Use the entity name from graph summary or search results.",
		MIMEType:    "application/json",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		// Extract entity name from URI. A missing entity is not an error here:
		// as before, the (empty) graph is returned.
		uri := req.Params.URI
		prefix := "memory://entities/"
		if !strings.HasPrefix(uri, prefix) {
			return nil, fmt.Errorf("invalid resource URI: %s", uri)
		}
		name := strings.TrimPrefix(uri, prefix)
		if name == "" {
			return nil, fmt.Errorf("entity name is required")
		}

		graph, err := manager.OpenNodes([]string{name})
		if err != nil {
			return nil, fmt.Errorf("failed to open entity %q: %w", name, err)
		}
		data, err := json.MarshalIndent(graph, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("failed to marshal entity data: %w", err)
		}
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{
			URI:      uri,
			MIMEType: "application/json",
			Text:     string(data),
		}}}, nil
	})
}

func userPrompt(description, text string) *mcp.GetPromptResult {
	return &mcp.GetPromptResult{
		Description: description,
		Messages: []*mcp.PromptMessage{{
			Role:    "user",
			Content: &mcp.TextContent{Text: text},
		}},
	}
}

func addPrompts(s *mcp.Server, manager *KnowledgeGraphManager) {
	// Prompts provide standardized memory operation templates that appear
	// as clickable actions in clients like Claude Desktop and VS Code.

	// Prompt: Recall memories about a topic
	s.AddPrompt(&mcp.Prompt{
		Name:        "memory-recall",
		Description: "Search and recall relevant memories from the knowledge graph based on a topic or question",
		Arguments: []*mcp.PromptArgument{{
			Name:        "topic",
			Description: "The topic, question, or keywords to search memories for",
			Required:    true,
		}},
	}, func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		topic := req.Params.Arguments["topic"]
		results, err := manager.SearchNodes(topic, 10)
		if err != nil {
			return nil, fmt.Errorf("failed to search for topic %q: %w", topic, err)
		}

		var promptText string
		if results.Total == 0 {
			promptText = fmt.Sprintf("I searched the knowledge graph for '%s' but found no matching memories. You may want to create new entities to store information about this topic.", topic)
		} else {
			var entityList strings.Builder
			for i, entity := range results.Entities {
				entityList.WriteString(fmt.Sprintf("\n%d. **%s** (%s) - %d observations, %d relations",
					i+1, entity.Name, entity.EntityType, entity.ObservationsCount, entity.RelationsCount))
				for _, snippet := range entity.Snippets {
					entityList.WriteString(fmt.Sprintf("\n   > %s", snippet))
				}
			}

			// Collect entity names for the open_nodes suggestion
			names := make([]string, len(results.Entities))
			for i, e := range results.Entities {
				names[i] = e.Name
			}

			promptText = fmt.Sprintf(`I searched the knowledge graph for '%s' and found %d matching entities:
%s

Please use the open_nodes tool to retrieve full details for the most relevant entities (suggested names: %s), then summarize the relevant memories for the user.`,
				topic, results.Total, entityList.String(), strings.Join(names, ", "))
		}

		return userPrompt(fmt.Sprintf("Recall memories about: %s", topic), promptText), nil
	})

	// Prompt: Save conversation information as memories
	s.AddPrompt(&mcp.Prompt{
		Name:        "memory-save",
		Description: "Analyze text and extract entities, relations, and observations to save as memories in the knowledge graph",
		Arguments: []*mcp.PromptArgument{{
			Name:        "content",
			Description: "The text or conversation content to extract memories from",
			Required:    true,
		}},
	}, func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		content := req.Params.Arguments["content"]

		promptText := fmt.Sprintf(`Analyze the following content and extract structured knowledge to save in the memory graph.

CONTENT TO ANALYZE:
---
%s
---

INSTRUCTIONS:
1. Identify key entities (people, technologies, projects, concepts, preferences, organizations)
2. For each entity, determine:
   - name: Clear, descriptive name
   - entityType: Category (person, technology, project, concept, preference, organization)
   - observations: Atomic facts about this entity from the content
3. Identify relations between entities (use active voice: "works_on", "uses", "belongs_to", etc.)
4. IMPORTANT: First use search_nodes to check if any of these entities already exist
   - If an entity exists, use add_observations to add new facts
   - If an entity is new, use create_entities to create it
5. Use create_relations to connect related entities

Please proceed to extract and save the memories.`, content)

		return userPrompt("Extract and save memories from content", promptText), nil
	})

	// Prompt: Review memories about an entity
	s.AddPrompt(&mcp.Prompt{
		Name:        "memory-review",
		Description: "Review and summarize all stored memories about a specific entity, including its observations and connections",
		Arguments: []*mcp.PromptArgument{{
			Name:        "entity_name",
			Description: "The exact name of the entity to review",
			Required:    true,
		}},
	}, func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		entityName := req.Params.Arguments["entity_name"]

		graph, err := manager.OpenNodes([]string{entityName})
		if err != nil {
			return nil, fmt.Errorf("failed to open entity %q: %w", entityName, err)
		}

		if len(graph.Entities) == 0 {
			return userPrompt(
				fmt.Sprintf("No entity found: %s", entityName),
				fmt.Sprintf("Entity '%s' was not found in the knowledge graph. Use search_nodes to find similar entity names.", entityName),
			), nil
		}

		data, err := json.MarshalIndent(graph, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("failed to marshal entity data: %w", err)
		}

		promptText := fmt.Sprintf(`Review the following entity data from the knowledge graph and provide a clear summary:

ENTITY DATA:
%s

Please summarize:
1. What is this entity and what type is it?
2. Key facts (observations) — highlight the most important ones
3. How it connects to other entities (relations)
4. Are there any observations that seem outdated or contradictory?
5. Suggest any missing information that might be worth adding`, string(data))

		return userPrompt(fmt.Sprintf("Review memories about: %s", entityName), promptText), nil
	})
}

func addTools(s *mcp.Server, manager *KnowledgeGraphManager) {
	// create_entities
	mcp.AddTool(s, &mcp.Tool{
		Name: "create_entities",
		Description: `Create new entities in the knowledge graph. Each entity has a unique name, a type, and observations (atomic facts).

BEFORE CREATING: Use search_nodes to check if the entity already exists. If it does, use add_observations to add new facts instead of creating a duplicate.

NAMING CONVENTIONS:
- Use clear, descriptive names (e.g. "TypeScript", "ProjectAlpha", "JohnDoe")
- entityType should be a lowercase category: "person", "technology", "project", "concept", "preference", "organization", "event", "location"
- Each observation should be a single, atomic fact — not a paragraph

EXAMPLE:
  name: "TypeScript", entityType: "technology"
  observations: ["Preferred language for frontend development", "Used with React in current project"]`,
		Annotations: writeAnnotations("Create Entities"),
		InputSchema: toolInput(map[string]any{
			"entities": arrayOfObjects("An array of entities to create", objectItems(map[string]any{
				"name":         strSchema("Unique name for the entity. Check with search_nodes first to avoid duplicates."),
				"entityType":   strSchema("Category of the entity (e.g. person, technology, project, concept, preference, organization)"),
				"observations": strArraySchema("Atomic facts about the entity. Each observation should be a single, self-contained statement."),
			}, "name", "entityType", "observations")),
		}, "entities"),
	}, func(ctx context.Context, req *mcp.CallToolRequest, arg createEntitiesArgs) (*mcp.CallToolResult, any, error) {
		if len(arg.Entities) == 0 {
			return nil, nil, errors.New("missing required parameter: entities")
		}
		newEntities, err := manager.CreateEntities(arg.Entities)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(newEntities)
	})

	// create_relations
	mcp.AddTool(s, &mcp.Tool{
		Name: "create_relations",
		Description: `Create directed relations (edges) between existing entities in the knowledge graph.

Relations express how entities are connected. Use active voice for relation types.
Both "from" and "to" entities must already exist — create them first if needed.

RELATION TYPE EXAMPLES:
  "works_on", "uses", "belongs_to", "created_by", "depends_on", "manages", "likes", "knows"

EXAMPLE:
  from: "JohnDoe", to: "ProjectAlpha", relationType: "works_on"
  from: "ProjectAlpha", to: "TypeScript", relationType: "uses"`,
		Annotations: writeAnnotations("Create Relations"),
		InputSchema: toolInput(map[string]any{
			"relations": arrayOfObjects("An array of relations to create", objectItems(map[string]any{
				"from":         strSchema("Source entity name (must already exist)"),
				"to":           strSchema("Target entity name (must already exist)"),
				"relationType": strSchema("Relation label in active voice (e.g. works_on, uses, belongs_to)"),
			}, "from", "to", "relationType")),
		}, "relations"),
	}, func(ctx context.Context, req *mcp.CallToolRequest, arg createRelationsArgs) (*mcp.CallToolResult, any, error) {
		if len(arg.Relations) == 0 {
			return nil, nil, errors.New("missing required parameter: relations")
		}
		newRelations, err := manager.CreateRelations(arg.Relations)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(newRelations)
	})

	// add_observations
	mcp.AddTool(s, &mcp.Tool{
		Name: "add_observations",
		Description: `Add new observations (facts) to existing entities in the knowledge graph.

Use this to append new information to entities that already exist. If the entity doesn't exist yet, use create_entities first.

Each observation should be a single, atomic fact. Duplicate observations are automatically skipped.

EXAMPLE:
  entityName: "TypeScript", contents: ["Version 5.0 released in 2023", "Supports decorators natively"]`,
		Annotations: writeAnnotations("Add Observations"),
		InputSchema: toolInput(map[string]any{
			"observations": arrayOfObjects("An array of observations to add to entities", objectItems(map[string]any{
				"entityName": strSchema("Exact name of the existing entity to add observations to"),
				"contents":   strArraySchema("New atomic facts to add. Duplicates are automatically skipped."),
			}, "entityName", "contents")),
		}, "observations"),
	}, func(ctx context.Context, req *mcp.CallToolRequest, arg addObservationsArgs) (*mcp.CallToolResult, any, error) {
		if len(arg.Observations) == 0 {
			return nil, nil, errors.New("missing required parameter: observations")
		}
		results, err := manager.AddObservations(arg.Observations)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(results)
	})

	// delete_entities
	mcp.AddTool(s, &mcp.Tool{
		Name:        "delete_entities",
		Description: "Delete entities and all their associated observations and relations from the knowledge graph. This action is irreversible.",
		Annotations: writeAnnotations("Delete Entities"),
		InputSchema: toolInput(map[string]any{
			"entityNames": strArraySchema("Exact names of entities to delete. All associated observations and relations will also be removed."),
		}, "entityNames"),
	}, func(ctx context.Context, req *mcp.CallToolRequest, arg deleteEntitiesArgs) (*mcp.CallToolResult, any, error) {
		if len(arg.EntityNames) == 0 {
			return nil, nil, errors.New("missing required parameter: entityNames")
		}
		if err := manager.DeleteEntities(arg.EntityNames); err != nil {
			return nil, nil, err
		}
		return textResult("Entities deleted successfully"), nil, nil
	})

	// delete_observations
	mcp.AddTool(s, &mcp.Tool{
		Name:        "delete_observations",
		Description: "Delete specific observations from entities. Use this to remove outdated or incorrect facts while keeping the entity itself.",
		Annotations: writeAnnotations("Delete Observations"),
		InputSchema: toolInput(map[string]any{
			"deletions": arrayOfObjects("An array of observations to delete from entities", objectItems(map[string]any{
				"entityName":   strSchema("Exact name of the entity containing the observations to delete"),
				"observations": strArraySchema("Exact observation text strings to remove. Must match existing observations exactly."),
			}, "entityName", "observations")),
		}, "deletions"),
	}, func(ctx context.Context, req *mcp.CallToolRequest, arg deleteObservationsArgs) (*mcp.CallToolResult, any, error) {
		if len(arg.Deletions) == 0 {
			return nil, nil, errors.New("missing required parameter: deletions")
		}
		if err := manager.DeleteObservations(arg.Deletions); err != nil {
			return nil, nil, err
		}
		return textResult("Observations deleted successfully"), nil, nil
	})

	// delete_relations
	mcp.AddTool(s, &mcp.Tool{
		Name:        "delete_relations",
		Description: "Delete specific relations from the knowledge graph. All three fields (from, to, relationType) must match exactly.",
		Annotations: writeAnnotations("Delete Relations"),
		InputSchema: toolInput(map[string]any{
			"relations": arrayOfObjects("An array of relations to delete (exact match required for all fields)", objectItems(map[string]any{
				"from":         strSchema("Source entity name"),
				"to":           strSchema("Target entity name"),
				"relationType": strSchema("Relation type to delete"),
			}, "from", "to", "relationType")),
		}, "relations"),
	}, func(ctx context.Context, req *mcp.CallToolRequest, arg deleteRelationsArgs) (*mcp.CallToolResult, any, error) {
		if len(arg.Relations) == 0 {
			return nil, nil, errors.New("missing required parameter: relations")
		}
		if err := manager.DeleteRelations(arg.Relations); err != nil {
			return nil, nil, err
		}
		return textResult("Relations deleted successfully"), nil, nil
	})

	// read_graph
	mcp.AddTool(s, &mcp.Tool{
		Name: "read_graph",
		Description: `Read the knowledge graph to understand what memories are stored.

MODES:
- "summary" (default): Returns statistics (entity/relation counts, type distribution) and a list of entity names. Use this to get an overview of available memories.
- "full": Returns the complete graph with all entities, observations, and relations. Use for backup or comprehensive analysis. Can be large.

RECOMMENDED WORKFLOW: Start with summary mode to see what's available, then use search_nodes for specific topics.`,
		Annotations: readAnnotations("Read Graph"),
		InputSchema: toolInput(map[string]any{
			"mode":  strSchema("'summary' (default): statistics + entity name list; 'full': complete graph export with all data"),
			"limit": numSchema("Max entity names in summary mode (default: 50, max: 200). Ignored in full mode."),
		}),
	}, func(ctx context.Context, req *mcp.CallToolRequest, arg readGraphArgs) (*mcp.CallToolResult, any, error) {
		// Default mode is "summary"
		mode := "summary"
		if arg.Mode != nil && *arg.Mode == "full" {
			mode = "full"
		}

		// Apply default and max limits (only relevant for summary mode)
		limit := 50
		if arg.Limit != nil {
			limit = *arg.Limit
			if limit > 200 {
				limit = 200
			}
			if limit < 1 {
				limit = 50
			}
		}

		result, err := manager.ReadGraph(mode, limit)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(result)
	})

	// search_nodes
	mcp.AddTool(s, &mcp.Tool{
		Name: "search_nodes",
		Description: `Search the knowledge graph for entities matching your query. This should be your FIRST step when looking for stored information.

Returns lightweight results (name, type, matched snippets, counts) — NOT full entity details. Use open_nodes with entity names from results to get complete information.

SEARCH BEHAVIOR:
- Single keyword: "React" matches entities with "React" in name, type, or observations
- Multiple keywords (space-separated OR): "React Vue" finds entities matching EITHER keyword
- Results are ranked: name matches first, then type matches, then observation content matches

WORKFLOW: search_nodes (find relevant entities) → open_nodes (get full details)`,
		Annotations: readAnnotations("Search Nodes"),
		InputSchema: toolInput(map[string]any{
			"query": strSchema("Search keywords. Space-separated words are treated as OR search. Matches against entity names, types, and observation content."),
			"limit": numSchema("Max entities to return. Omit or set to 0 for all matches."),
		}, "query"),
	}, func(ctx context.Context, req *mcp.CallToolRequest, arg searchNodesArgs) (*mcp.CallToolResult, any, error) {
		if arg.Query == "" {
			return nil, nil, errors.New("missing required parameter: query")
		}

		// If limit not specified, use 0 to indicate "all results"
		limit := 0
		if arg.Limit != nil {
			limit = *arg.Limit
			if limit < 1 {
				limit = 0 // treat invalid as "all"
			}
		}

		results, err := manager.SearchNodes(arg.Query, limit)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(results)
	})

	// open_nodes
	mcp.AddTool(s, &mcp.Tool{
		Name: "open_nodes",
		Description: `Get FULL details of specific entities by their exact names.

Returns complete entity data including ALL observations and ALL relations (both incoming and outgoing). Use search_nodes first to find entity names if you're unsure of the exact name.

REQUIRES: Exact entity names (case-sensitive). Get these from search_nodes results.
RETURNS: Complete entities with all observations, plus all relations connected to these entities.`,
		Annotations: readAnnotations("Open Nodes"),
		InputSchema: toolInput(map[string]any{
			"names": strArraySchema("Exact entity names to retrieve (case-sensitive). Use search_nodes first if unsure."),
		}, "names"),
	}, func(ctx context.Context, req *mcp.CallToolRequest, arg openNodesArgs) (*mcp.CallToolResult, any, error) {
		if len(arg.Names) == 0 {
			return nil, nil, errors.New("missing required parameter: names")
		}
		results, err := manager.OpenNodes(arg.Names)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(results)
	})

	// merge_entities
	mcp.AddTool(s, &mcp.Tool{
		Name: "merge_entities",
		Description: `Merge two entities into one. All observations and relations from the source entity are migrated to the target entity, then the source is deleted.

USE WHEN: You discover duplicate entities (e.g. "React.js" and "React" refer to the same thing).

BEHAVIOR:
- Source observations are added to target (duplicates skipped)
- Source relations are redirected to target (duplicates skipped)
- Source entity is deleted after migration

EXAMPLE: sourceName: "React.js", targetName: "React" → merges React.js into React`,
		Annotations: writeAnnotations("Merge Entities"),
		InputSchema: toolInput(map[string]any{
			"sourceName": strSchema("Entity to merge FROM (will be deleted)"),
			"targetName": strSchema("Entity to merge INTO (will receive observations and relations)"),
		}, "sourceName", "targetName"),
	}, func(ctx context.Context, req *mcp.CallToolRequest, arg mergeEntitiesArgs) (*mcp.CallToolResult, any, error) {
		if arg.SourceName == "" || arg.TargetName == "" {
			return nil, nil, errors.New("missing required parameters: sourceName and targetName")
		}
		result, err := manager.MergeEntities(arg.SourceName, arg.TargetName)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(result)
	})

	// update_entities
	mcp.AddTool(s, &mcp.Tool{
		Name: "update_entities",
		Description: `Update the type of an existing entity.

USE WHEN: An entity was created with the wrong type and needs correction.

EXAMPLE: name: "React", entityType: "framework" (was previously "library")`,
		Annotations: writeAnnotations("Update Entity Type"),
		InputSchema: toolInput(map[string]any{
			"name":       strSchema("Exact name of the entity to update"),
			"entityType": strSchema("New entity type to set"),
		}, "name", "entityType"),
	}, func(ctx context.Context, req *mcp.CallToolRequest, arg updateEntitiesArgs) (*mcp.CallToolResult, any, error) {
		if arg.Name == "" || arg.EntityType == "" {
			return nil, nil, errors.New("missing required parameters: name and entityType")
		}
		if err := manager.UpdateEntityType(arg.Name, arg.EntityType); err != nil {
			return nil, nil, err
		}
		return textResult(fmt.Sprintf("Entity %q type updated to %q", arg.Name, arg.EntityType)), nil, nil
	})

	// update_observations
	mcp.AddTool(s, &mcp.Tool{
		Name: "update_observations",
		Description: `Replace an existing observation with updated content. Use this to correct outdated or inaccurate facts.

USE WHEN: An observation needs correction (e.g. "Uses React 17" → "Uses React 18").

REQUIRES: The exact old observation text. Use open_nodes first to get the current text.`,
		Annotations: writeAnnotations("Update Observation"),
		InputSchema: toolInput(map[string]any{
			"entityName": strSchema("Exact name of the entity containing the observation"),
			"oldContent": strSchema("Exact current observation text to replace"),
			"newContent": strSchema("New observation text"),
		}, "entityName", "oldContent", "newContent"),
	}, func(ctx context.Context, req *mcp.CallToolRequest, arg updateObservationsArgs) (*mcp.CallToolResult, any, error) {
		if arg.EntityName == "" || arg.OldContent == "" || arg.NewContent == "" {
			return nil, nil, errors.New("missing required parameters: entityName, oldContent, and newContent")
		}
		if err := manager.UpdateObservation(arg.EntityName, arg.OldContent, arg.NewContent); err != nil {
			return nil, nil, err
		}
		return textResult("Observation updated successfully"), nil, nil
	})

	// detect_conflicts
	mcp.AddTool(s, &mcp.Tool{
		Name: "detect_conflicts",
		Description: `Detect potential duplicate or contradictory observations within entities.

Analyzes observation pairs for:
- Potential duplicates: observations with high prefix overlap (>60% of words match at start)
- Potential contradictions: observations containing antonym keyword pairs (e.g. "likes X" vs "dislikes X")

USE WHEN: Reviewing memory quality, or after bulk imports to find inconsistencies.

RETURNS: List of conflicts with entity name, both observations, and conflict type.`,
		Annotations: readAnnotations("Detect Conflicts"),
		InputSchema: toolInput(map[string]any{
			"entityName": strSchema("Optional: check only this entity. Omit to check all entities."),
		}),
	}, func(ctx context.Context, req *mcp.CallToolRequest, arg detectConflictsArgs) (*mcp.CallToolResult, any, error) {
		entityName := ""
		if arg.EntityName != nil {
			entityName = *arg.EntityName
		}

		conflicts, err := manager.DetectConflicts(entityName)
		if err != nil {
			return nil, nil, err
		}
		if len(conflicts) == 0 {
			return textResult("No conflicts detected"), nil, nil
		}
		return jsonResult(conflicts)
	})
}
