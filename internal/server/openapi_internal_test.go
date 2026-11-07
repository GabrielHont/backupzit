package server

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func TestOpenAPIContract(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal(publicOpenAPI, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["openapi"] != "3.1.0" {
		t.Fatal("expected OpenAPI 3.1")
	}
	paths := doc["paths"].(map[string]any)
	ids := map[string]bool{}
	for p, item := range paths {
		for method, raw := range item.(map[string]any) {
			op := raw.(map[string]any)
			id := op["operationId"].(string)
			if ids[id] {
				t.Errorf("duplicate operation ID %s", id)
			}
			ids[id] = true
			if p == "/agent/{file}" {
				if method != "get" || op["x-minimum-role"] != "public" || !reflect.DeepEqual(op["security"], []any{}) || !reflect.DeepEqual(op["servers"], []any{map[string]any{"url": "/"}}) {
					t.Errorf("public installer route must be unauthenticated at the console root")
				}
			} else {
				if _, ok := roleRank[op["x-minimum-role"].(string)]; !ok {
					t.Errorf("invalid role for %s %s", method, p)
				}
				responses := op["responses"].(map[string]any)
				if responses["401"] == nil || responses["403"] == nil {
					t.Errorf("missing auth errors: %s %s", method, p)
				}
			}
		}
	}
	// Keep the contract complete as routes are added. The two scheduling
	// actions are registered in a loop, all other routes use literals.
	routes := map[string]bool{"post /jobs/{id}/enable": true, "post /jobs/{id}/disable": true}
	pattern := regexp.MustCompile(`"(GET|POST|PUT|PATCH|DELETE) /api/v1([^"\n]+)"`)
	publicPattern := regexp.MustCompile(`"(GET) (/agent/\{file\})"`)
	files, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if !strings.HasSuffix(f.Name(), ".go") || strings.HasSuffix(f.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(f.Name())
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range pattern.FindAllStringSubmatch(string(b), -1) {
			if strings.HasSuffix(m[2], "/") {
				continue
			}
			routes[strings.ToLower(m[1])+" "+m[2]] = true
		}
		for _, m := range publicPattern.FindAllStringSubmatch(string(b), -1) {
			routes[strings.ToLower(m[1])+" "+m[2]] = true
		}
	}
	for route := range routes {
		method, p, _ := strings.Cut(route, " ")
		item, ok := paths[p].(map[string]any)
		if !ok || item[method] == nil {
			t.Errorf("undocumented route %s", route)
		}
	}
	if len(ids) != len(routes) {
		t.Errorf("contract has %d operations, source has %d", len(ids), len(routes))
	}
	var walk func(any)
	walk = func(value any) {
		switch v := value.(type) {
		case map[string]any:
			if r, ok := v["$ref"].(string); ok {
				var target any = doc
				if !strings.HasPrefix(r, "#/") {
					t.Errorf("external ref %s", r)
					return
				}
				for _, part := range strings.Split(r[2:], "/") {
					m, ok := target.(map[string]any)
					if !ok {
						target = nil
						break
					}
					target = m[part]
				}
				if target == nil {
					t.Errorf("broken ref %s", r)
				}
			}
			for _, child := range v {
				walk(child)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(doc)
	schemas := doc["components"].(map[string]any)["schemas"].(map[string]any)
	for name, typ := range map[string]reflect.Type{"Agent": reflect.TypeFor[apiAgent](), "AgentPackage": reflect.TypeFor[apiAgentPackage](), "AgentRemoval": reflect.TypeFor[apiAgentRemoval](), "Job": reflect.TypeFor[apiJob](), "Run": reflect.TypeFor[apiRun](), "EnrollmentToken": reflect.TypeFor[EnrollmentToken](), "Limits": reflect.TypeFor[ConsoleLimits](), "JobOptions": reflect.TypeFor[apiJobOptions](), "FileEntry": reflect.TypeFor[apiFileEntry]()} {
		schema := schemas[name].(map[string]any)
		properties := schema["properties"].(map[string]any)
		required := map[string]bool{}
		if values, ok := schema["required"].([]any); ok {
			for _, v := range values {
				required[v.(string)] = true
			}
		}
		for i := 0; i < typ.NumField(); i++ {
			parts := strings.Split(typ.Field(i).Tag.Get("json"), ",")
			key := parts[0]
			if properties[key] == nil {
				t.Errorf("%s.%s missing in contract", name, key)
			}
			optional := len(parts) > 1 && parts[1] == "omitempty"
			if required[key] == optional {
				t.Errorf("%s.%s required/omitempty mismatch", name, key)
			}
		}
	}
}
