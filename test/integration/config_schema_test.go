package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"gopkg.in/yaml.v3"

	"monstermq.io/edge/internal/config"
)

// The example configurations are live documents: they must validate against
// yaml-json-schema.json and load through the Go config.
func TestExampleConfigsValidate(t *testing.T) {
	root := filepath.Join("..", "..")
	raw, err := os.ReadFile(filepath.Join(root, "yaml-json-schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	// The draft-07 meta-schema URL is not resolvable offline.
	schema.Schema = ""
	resolved, err := schema.Resolve(nil)
	if err != nil {
		t.Fatalf("resolve schema: %v", err)
	}
	for _, f := range []string{"config.yaml.example", "winccoa/monstermq.yaml.example"} {
		data, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			t.Fatal(err)
		}
		var doc any
		if err := yaml.Unmarshal(data, &doc); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		js, _ := json.Marshal(doc)
		var inst any
		_ = json.Unmarshal(js, &inst)
		if err := resolved.Validate(inst); err != nil {
			t.Errorf("%s does not validate: %v", f, err)
		}
		cfg, err := config.Load(filepath.Join(root, f))
		if err != nil {
			t.Errorf("%s does not load: %v", f, err)
			continue
		}
		if f == "winccoa/monstermq.yaml.example" && (!cfg.WinCCOaNative.Enabled || len(cfg.WinCCOaNative.Stores) != 3) {
			t.Errorf("%s: WinCCOaNative not parsed: %+v", f, cfg.WinCCOaNative)
		}
	}
}
