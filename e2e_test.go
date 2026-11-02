// Copyright (c) 2025 okooo5km(十里)
// Licensed under the MIT License.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func newTestManager(t *testing.T) *KnowledgeGraphManager {
	t.Helper()
	mgr, err := NewKnowledgeGraphManager(filepath.Join(t.TempDir(), "test.db"), "sqlite", false)
	if err != nil {
		t.Fatalf("NewKnowledgeGraphManager: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	return mgr
}

// connectInMemory connects a real SDK client to the server over an in-memory
// transport.
func connectInMemory(t *testing.T, s *mcp.Server) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	ct, st := mcp.NewInMemoryTransports()
	ss, err := s.Connect(ctx, st, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// callTool calls a tool and returns its text and IsError flag. A protocol
// level error fails the test.
func callTool(t *testing.T, cs *mcp.ClientSession, name string, args any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s) protocol error: %v", name, err)
	}
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String(), res.IsError
}

func mustCall(t *testing.T, cs *mcp.ClientSession, name string, args any) string {
	t.Helper()
	text, isErr := callTool(t, cs, name, args)
	if isErr {
		t.Fatalf("%s returned a tool error: %s", name, text)
	}
	return text
}

var expectedToolNames = []string{
	"add_observations", "create_entities", "create_relations", "delete_entities",
	"delete_observations", "delete_relations", "detect_conflicts", "merge_entities",
	"open_nodes", "read_graph", "search_nodes", "update_entities", "update_observations",
}

func schemaAt(t *testing.T, schema map[string]any, path ...string) any {
	t.Helper()
	var cur any = schema
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("schema path %v: %q is not an object (got %T)", path, p, cur)
		}
		cur, ok = m[p]
		if !ok {
			t.Fatalf("schema path %v: missing %q", path, p)
		}
	}
	return cur
}

func TestListFeatures(t *testing.T) {
	cs := connectInMemory(t, newMCPServer(newTestManager(t)))
	ctx := context.Background()

	init := cs.InitializeResult()
	if init.ServerInfo.Name != appName || init.ServerInfo.Version != version {
		t.Errorf("server info = %+v, want %s/%s", init.ServerInfo, appName, version)
	}
	caps := init.Capabilities
	if caps.Tools == nil || caps.Prompts == nil || caps.Resources == nil {
		t.Errorf("missing capabilities: %+v", caps)
	}
	if caps.Logging == nil {
		t.Errorf("logging capability should still be declared")
	}
	if caps.Resources != nil && caps.Resources.Subscribe {
		t.Errorf("resources.subscribe must not be advertised")
	}

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var names []string
	byName := map[string]*mcp.Tool{}
	for _, tl := range tools.Tools {
		names = append(names, tl.Name)
		byName[tl.Name] = tl
	}
	sort.Strings(names)
	if strings.Join(names, ",") != strings.Join(expectedToolNames, ",") {
		t.Fatalf("tool names = %v, want %v", names, expectedToolNames)
	}

	readOnly := map[string]bool{"read_graph": true, "search_nodes": true, "open_nodes": true, "detect_conflicts": true}
	for name, tl := range byName {
		if tl.Description == "" {
			t.Errorf("%s: empty description", name)
		}
		if tl.Annotations == nil || tl.Annotations.Title == "" {
			t.Errorf("%s: missing annotations/title", name)
			continue
		}
		if tl.Annotations.ReadOnlyHint != readOnly[name] {
			t.Errorf("%s: readOnlyHint = %v", name, tl.Annotations.ReadOnlyHint)
		}
		if tl.Annotations.DestructiveHint == nil || *tl.Annotations.DestructiveHint == readOnly[name] {
			t.Errorf("%s: destructiveHint = %v, readOnly = %v", name, tl.Annotations.DestructiveHint, readOnly[name])
		}
	}

	// Input schemas: required lists and nested descriptions.
	schemaTests := []struct {
		tool     string
		required []string
		descs    map[string]string // dotted path to "description" -> expected text
	}{
		{"create_entities", []string{"entities"}, map[string]string{
			"properties.entities.description":                               "An array of entities to create",
			"properties.entities.items.properties.name.description":         "Unique name for the entity. Check with search_nodes first to avoid duplicates.",
			"properties.entities.items.properties.entityType.description":   "Category of the entity (e.g. person, technology, project, concept, preference, organization)",
			"properties.entities.items.properties.observations.description": "Atomic facts about the entity. Each observation should be a single, self-contained statement.",
		}},
		{"create_relations", []string{"relations"}, map[string]string{
			"properties.relations.items.properties.from.description":         "Source entity name (must already exist)",
			"properties.relations.items.properties.to.description":           "Target entity name (must already exist)",
			"properties.relations.items.properties.relationType.description": "Relation label in active voice (e.g. works_on, uses, belongs_to)",
		}},
		{"add_observations", []string{"observations"}, map[string]string{
			"properties.observations.items.properties.entityName.description": "Exact name of the existing entity to add observations to",
			"properties.observations.items.properties.contents.description":   "New atomic facts to add. Duplicates are automatically skipped.",
		}},
		{"delete_entities", []string{"entityNames"}, map[string]string{
			"properties.entityNames.description": "Exact names of entities to delete. All associated observations and relations will also be removed.",
		}},
		{"delete_observations", []string{"deletions"}, map[string]string{
			"properties.deletions.items.properties.entityName.description":   "Exact name of the entity containing the observations to delete",
			"properties.deletions.items.properties.observations.description": "Exact observation text strings to remove. Must match existing observations exactly.",
		}},
		{"delete_relations", []string{"relations"}, map[string]string{
			"properties.relations.description":                       "An array of relations to delete (exact match required for all fields)",
			"properties.relations.items.properties.to.description":   "Target entity name",
			"properties.relations.items.properties.from.description": "Source entity name",
		}},
		{"read_graph", nil, map[string]string{
			"properties.mode.description":  "'summary' (default): statistics + entity name list; 'full': complete graph export with all data",
			"properties.limit.description": "Max entity names in summary mode (default: 50, max: 200). Ignored in full mode.",
		}},
		{"search_nodes", []string{"query"}, map[string]string{
			"properties.query.description": "Search keywords. Space-separated words are treated as OR search. Matches against entity names, types, and observation content.",
			"properties.limit.description": "Max entities to return. Omit or set to 0 for all matches.",
		}},
		{"open_nodes", []string{"names"}, map[string]string{
			"properties.names.description": "Exact entity names to retrieve (case-sensitive). Use search_nodes first if unsure.",
		}},
		{"merge_entities", []string{"sourceName", "targetName"}, map[string]string{
			"properties.sourceName.description": "Entity to merge FROM (will be deleted)",
			"properties.targetName.description": "Entity to merge INTO (will receive observations and relations)",
		}},
		{"update_entities", []string{"name", "entityType"}, map[string]string{
			"properties.name.description":       "Exact name of the entity to update",
			"properties.entityType.description": "New entity type to set",
		}},
		{"update_observations", []string{"entityName", "oldContent", "newContent"}, map[string]string{
			"properties.entityName.description": "Exact name of the entity containing the observation",
			"properties.oldContent.description": "Exact current observation text to replace",
			"properties.newContent.description": "New observation text",
		}},
		{"detect_conflicts", nil, map[string]string{
			"properties.entityName.description": "Optional: check only this entity. Omit to check all entities.",
		}},
	}
	for _, tc := range schemaTests {
		t.Run(tc.tool, func(t *testing.T) {
			schema, ok := byName[tc.tool].InputSchema.(map[string]any)
			if !ok {
				t.Fatalf("input schema is %T", byName[tc.tool].InputSchema)
			}
			if schema["type"] != "object" {
				t.Errorf("type = %v", schema["type"])
			}
			var req []string
			if r, ok := schema["required"].([]any); ok {
				for _, v := range r {
					req = append(req, v.(string))
				}
			}
			if strings.Join(req, ",") != strings.Join(tc.required, ",") {
				t.Errorf("required = %v, want %v", req, tc.required)
			}
			for path, want := range tc.descs {
				got := schemaAt(t, schema, append(strings.Split(strings.TrimSuffix(path, ".description"), "."), "description")...)
				if got != want {
					t.Errorf("%s = %v, want %q", path, got, want)
				}
			}
		})
	}
	// Nested required lists for the array-of-object tools.
	nested := map[string][]string{
		"create_entities":     {"name", "entityType", "observations"},
		"create_relations":    {"from", "to", "relationType"},
		"add_observations":    {"entityName", "contents"},
		"delete_observations": {"entityName", "observations"},
		"delete_relations":    {"from", "to", "relationType"},
	}
	for tool, want := range nested {
		schema := byName[tool].InputSchema.(map[string]any)
		prop := schema["properties"].(map[string]any)
		for _, v := range prop {
			items := v.(map[string]any)["items"].(map[string]any)
			var got []string
			for _, r := range items["required"].([]any) {
				got = append(got, r.(string))
			}
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("%s nested required = %v, want %v", tool, got, want)
			}
		}
	}

	resources, err := cs.ListResources(ctx, nil)
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	gotRes := map[string]string{}
	for _, r := range resources.Resources {
		gotRes[r.URI] = r.Name
		if r.MIMEType != "application/json" || r.Description == "" {
			t.Errorf("resource %s: mime=%q desc=%q", r.URI, r.MIMEType, r.Description)
		}
	}
	wantRes := map[string]string{"memory://graph/summary": "Knowledge Graph Summary", "memory://graph/types": "Entity & Relation Types"}
	if fmt.Sprint(gotRes) != fmt.Sprint(wantRes) {
		t.Errorf("resources = %v, want %v", gotRes, wantRes)
	}
	tmpls, err := cs.ListResourceTemplates(ctx, nil)
	if err != nil {
		t.Fatalf("ListResourceTemplates: %v", err)
	}
	if len(tmpls.ResourceTemplates) != 1 || tmpls.ResourceTemplates[0].URITemplate != "memory://entities/{name}" || tmpls.ResourceTemplates[0].Name != "Entity Details" {
		t.Errorf("templates = %+v", tmpls.ResourceTemplates)
	}

	prompts, err := cs.ListPrompts(ctx, nil)
	if err != nil {
		t.Fatalf("ListPrompts: %v", err)
	}
	gotPrompts := map[string]string{}
	for _, p := range prompts.Prompts {
		if len(p.Arguments) != 1 || !p.Arguments[0].Required {
			t.Errorf("prompt %s arguments = %+v", p.Name, p.Arguments)
			continue
		}
		gotPrompts[p.Name] = p.Arguments[0].Name
	}
	wantPrompts := map[string]string{"memory-recall": "topic", "memory-save": "content", "memory-review": "entity_name"}
	if fmt.Sprint(gotPrompts) != fmt.Sprint(wantPrompts) {
		t.Errorf("prompts = %v, want %v", gotPrompts, wantPrompts)
	}
}

func TestToolsRoundTrip(t *testing.T) {
	cs := connectInMemory(t, newMCPServer(newTestManager(t)))

	type step struct {
		name     string
		tool     string
		args     any
		contains []string
		wantErr  bool
	}
	steps := []step{
		{"create entities", "create_entities", map[string]any{"entities": []map[string]any{
			{"name": "TypeScript", "entityType": "technology", "observations": []string{"Typed superset of JavaScript"}},
			{"name": "ProjectAlpha", "entityType": "project", "observations": []string{"Uses React 17"}},
			{"name": "React.js", "entityType": "library", "observations": []string{"UI library"}},
			{"name": "React", "entityType": "library", "observations": []string{"prefers hooks"}},
		}}, []string{"TypeScript", "ProjectAlpha"}, false},
		{"create relations", "create_relations", map[string]any{"relations": []map[string]any{
			{"from": "ProjectAlpha", "to": "TypeScript", "relationType": "uses"},
			{"from": "ProjectAlpha", "to": "React.js", "relationType": "uses"},
		}}, []string{"ProjectAlpha", "uses"}, false},
		{"add observations", "add_observations", map[string]any{"observations": []map[string]any{
			{"entityName": "TypeScript", "contents": []string{"Supports decorators natively"}},
		}}, []string{"Supports decorators natively"}, false},
		{"search", "search_nodes", map[string]any{"query": "TypeScript"}, []string{"TypeScript"}, false},
		{"search with limit", "search_nodes", map[string]any{"query": "library", "limit": 1}, []string{"library"}, false},
		{"open nodes", "open_nodes", map[string]any{"names": []string{"TypeScript"}}, []string{"Supports decorators natively", "ProjectAlpha"}, false},
		{"read graph default", "read_graph", map[string]any{}, []string{"TypeScript"}, false},
		{"read graph summary clamp high", "read_graph", map[string]any{"mode": "summary", "limit": 9999}, []string{"TypeScript"}, false},
		{"read graph summary clamp low", "read_graph", map[string]any{"limit": -3}, []string{"TypeScript"}, false},
		{"read graph full", "read_graph", map[string]any{"mode": "full"}, []string{"Supports decorators natively", "relations"}, false},
		{"update entity type", "update_entities", map[string]any{"name": "ProjectAlpha", "entityType": "initiative"}, []string{`Entity "ProjectAlpha" type updated to "initiative"`}, false},
		{"update observation", "update_observations", map[string]any{"entityName": "ProjectAlpha", "oldContent": "Uses React 17", "newContent": "Uses React 18"}, []string{"Observation updated successfully"}, false},
		{"add contradictory", "add_observations", map[string]any{"observations": []map[string]any{
			{"entityName": "React", "contents": []string{"avoids hooks"}},
		}}, nil, false},
		{"detect conflicts", "detect_conflicts", map[string]any{"entityName": "React"}, []string{"hooks"}, false},
		{"detect conflicts none", "detect_conflicts", map[string]any{"entityName": "TypeScript"}, []string{"No conflicts detected"}, false},
		{"merge", "merge_entities", map[string]any{"sourceName": "React.js", "targetName": "React"}, nil, false},
		{"delete relation", "delete_relations", map[string]any{"relations": []map[string]any{
			{"from": "ProjectAlpha", "to": "TypeScript", "relationType": "uses"},
		}}, []string{"Relations deleted successfully"}, false},
		{"delete observation", "delete_observations", map[string]any{"deletions": []map[string]any{
			{"entityName": "TypeScript", "observations": []string{"Supports decorators natively"}},
		}}, []string{"Observations deleted successfully"}, false},
		{"delete entities", "delete_entities", map[string]any{"entityNames": []string{"TypeScript", "ProjectAlpha", "React"}}, []string{"Entities deleted successfully"}, false},

		// Failures surface as tool errors (IsError), not protocol errors.
		{"missing required", "create_entities", map[string]any{}, []string{"entities"}, true},
		{"empty array", "create_entities", map[string]any{"entities": []any{}}, []string{"missing required parameter: entities"}, true},
		{"wrong type", "search_nodes", map[string]any{"query": 42}, nil, true},
		{"empty query", "search_nodes", map[string]any{"query": ""}, []string{"missing required parameter: query"}, true},
		{"missing nested field", "create_relations", map[string]any{"relations": []map[string]any{{"from": "a"}}}, nil, true},
		{"merge missing entity", "merge_entities", map[string]any{"sourceName": "ghost", "targetName": "phantom"}, nil, true},
	}
	for _, st := range steps {
		t.Run(st.name, func(t *testing.T) {
			text, isErr := callTool(t, cs, st.tool, st.args)
			if isErr != st.wantErr {
				t.Fatalf("IsError = %v, want %v; text: %s", isErr, st.wantErr, text)
			}
			for _, want := range st.contains {
				if !strings.Contains(text, want) {
					t.Errorf("result missing %q:\n%s", want, text)
				}
			}
		})
	}

	// After the merge, React.js must be gone and React must hold its data.
	t.Run("state after merge", func(t *testing.T) {
		cs2 := connectInMemory(t, newMCPServer(newTestManager(t)))
		mustCall(t, cs2, "create_entities", map[string]any{"entities": []map[string]any{
			{"name": "A", "entityType": "x", "observations": []string{"one"}},
			{"name": "B", "entityType": "x", "observations": []string{"two"}},
		}})
		mustCall(t, cs2, "merge_entities", map[string]any{"sourceName": "A", "targetName": "B"})
		out := mustCall(t, cs2, "open_nodes", map[string]any{"names": []string{"B"}})
		if !strings.Contains(out, "one") || !strings.Contains(out, "two") {
			t.Errorf("merged entity lost observations: %s", out)
		}
		out = mustCall(t, cs2, "open_nodes", map[string]any{"names": []string{"A"}})
		var graph struct {
			Entities []any `json:"entities"`
		}
		if err := json.Unmarshal([]byte(out), &graph); err != nil || len(graph.Entities) != 0 {
			t.Errorf("source entity still present: %s (%v)", out, err)
		}
	})
}

func TestResourcesAndPrompts(t *testing.T) {
	cs := connectInMemory(t, newMCPServer(newTestManager(t)))
	ctx := context.Background()
	mustCall(t, cs, "create_entities", map[string]any{"entities": []map[string]any{
		{"name": "Go", "entityType": "technology", "observations": []string{"Compiled language"}},
	}})

	read := func(uri string) (*mcp.ReadResourceResult, error) {
		return cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri})
	}
	for _, tc := range []struct {
		uri  string
		want string
	}{
		{"memory://graph/summary", "Go"},
		{"memory://graph/types", "technology"},
		{"memory://entities/Go", "Compiled language"},
	} {
		res, err := read(tc.uri)
		if err != nil {
			t.Errorf("read %s: %v", tc.uri, err)
			continue
		}
		if len(res.Contents) != 1 || res.Contents[0].URI != tc.uri || res.Contents[0].MIMEType != "application/json" || !strings.Contains(res.Contents[0].Text, tc.want) {
			t.Errorf("read %s = %+v", tc.uri, res.Contents)
		}
		if !json.Valid([]byte(res.Contents[0].Text)) {
			t.Errorf("read %s: not JSON", tc.uri)
		}
	}
	// A missing entity yields an empty graph (as before), not an error.
	res, err := read("memory://entities/Nope")
	if err != nil {
		t.Errorf("missing entity: %v", err)
	} else if !strings.Contains(res.Contents[0].Text, "entities") {
		t.Errorf("missing entity result: %s", res.Contents[0].Text)
	}
	// Unregistered URIs are errors.
	if _, err := read("memory://nothing"); err == nil {
		t.Errorf("unknown resource should fail")
	}

	getPrompt := func(name string, args map[string]string) string {
		res, err := cs.GetPrompt(ctx, &mcp.GetPromptParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("GetPrompt(%s): %v", name, err)
		}
		if len(res.Messages) != 1 || res.Messages[0].Role != "user" {
			t.Fatalf("GetPrompt(%s) messages = %+v", name, res.Messages)
		}
		tc, ok := res.Messages[0].Content.(*mcp.TextContent)
		if !ok {
			t.Fatalf("GetPrompt(%s) content is %T", name, res.Messages[0].Content)
		}
		return res.Description + "\n" + tc.Text
	}
	if out := getPrompt("memory-recall", map[string]string{"topic": "Go"}); !strings.Contains(out, "Recall memories about: Go") || !strings.Contains(out, "found 1 matching entities") {
		t.Errorf("memory-recall: %s", out)
	}
	if out := getPrompt("memory-recall", map[string]string{"topic": "zzzz"}); !strings.Contains(out, "found no matching memories") {
		t.Errorf("memory-recall (none): %s", out)
	}
	if out := getPrompt("memory-save", map[string]string{"content": "Alice likes Go"}); !strings.Contains(out, "Alice likes Go") || !strings.Contains(out, "Please proceed to extract and save the memories.") {
		t.Errorf("memory-save: %s", out)
	}
	if out := getPrompt("memory-review", map[string]string{"entity_name": "Go"}); !strings.Contains(out, "Compiled language") || !strings.Contains(out, "Review memories about: Go") {
		t.Errorf("memory-review: %s", out)
	}
	if out := getPrompt("memory-review", map[string]string{"entity_name": "Ghost"}); !strings.Contains(out, "was not found in the knowledge graph") {
		t.Errorf("memory-review (none): %s", out)
	}
}

func TestPanicRecovery(t *testing.T) {
	s := newMCPServer(newTestManager(t))
	s.AddTool(&mcp.Tool{Name: "boom", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			panic("kaboom")
		})
	cs := connectInMemory(t, s)

	_, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "boom"})
	if err == nil || !strings.Contains(err.Error(), "kaboom") {
		t.Fatalf("panic should surface as an error mentioning the panic, got %v", err)
	}
	// The server must keep working.
	if out := mustCall(t, cs, "read_graph", map[string]any{}); !strings.Contains(out, "entities") && out == "" {
		t.Errorf("server unusable after panic: %q", out)
	}
}
