package graphql

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"
)

// coreSchemaDir is MonsterMQ Core's SDL in the sibling checkout.
const coreSchemaDir = "../../../main/broker/src/main/resources"

func loadSDL(t *testing.T, pattern string) *ast.SchemaDocument {
	t.Helper()
	files, err := filepath.Glob(pattern)
	if err != nil || len(files) == 0 {
		return nil
	}
	doc := &ast.SchemaDocument{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		d, err := parser.ParseSchema(&ast.Source{Name: f, Input: string(raw)})
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		doc.Merge(d)
	}
	return doc
}

// definitions merges a type's definition and extensions by name.
func definitions(doc *ast.SchemaDocument) map[string]*ast.Definition {
	out := map[string]*ast.Definition{}
	add := func(d *ast.Definition) {
		cur, ok := out[d.Name]
		if !ok {
			cp := *d
			out[d.Name] = &cp
			return
		}
		cur.Fields = append(cur.Fields, d.Fields...)
		cur.EnumValues = append(cur.EnumValues, d.EnumValues...)
	}
	for _, d := range doc.Definitions {
		add(d)
	}
	for _, d := range doc.Extensions {
		add(d)
	}
	return out
}

// The Kafka client schema must stay a strict subset of Core's, so the same
// dashboard works against both brokers (AGENTS.md, GraphQL parity rule).
func TestKafkaClientSchemaIsSubsetOfCore(t *testing.T) {
	checkSubsetOfCore(t, "schema/kafkaclient.graphqls")
}

// The Kafka server schema is a verbatim copy of Core's schema-kafka-servers.graphqls.
func TestKafkaServerSchemaIsSubsetOfCore(t *testing.T) {
	checkSubsetOfCore(t, "schema/kafkaservers.graphqls")
}

func checkSubsetOfCore(t *testing.T, edgeFile string) {
	t.Helper()
	core := loadSDL(t, filepath.Join(coreSchemaDir, "schema-*.graphqls"))
	if core == nil {
		t.Skip("Core schema not checked out next to edge")
	}
	edge := loadSDL(t, edgeFile)
	if edge == nil {
		t.Fatal(edgeFile + " missing")
	}
	coreDefs := definitions(core)
	for name, ed := range definitions(edge) {
		cd, ok := coreDefs[name]
		if !ok {
			t.Errorf("type %s is not in Core's schema", name)
			continue
		}
		if ed.Kind != cd.Kind {
			t.Errorf("%s: kind %s, Core %s", name, ed.Kind, cd.Kind)
		}
		coreValues := map[string]bool{}
		for _, v := range cd.EnumValues {
			coreValues[v.Name] = true
		}
		for _, v := range ed.EnumValues {
			if !coreValues[v.Name] {
				t.Errorf("%s: enum value %s is not in Core", name, v.Name)
			}
		}
		coreFields := map[string]*ast.FieldDefinition{}
		for _, f := range cd.Fields {
			coreFields[f.Name] = f
		}
		for _, f := range ed.Fields {
			cf, ok := coreFields[f.Name]
			if !ok {
				t.Errorf("%s.%s is not in Core", name, f.Name)
				continue
			}
			if f.Type.String() != cf.Type.String() {
				t.Errorf("%s.%s: type %s, Core %s", name, f.Name, f.Type, cf.Type)
			}
			if defaultString(f.DefaultValue) != defaultString(cf.DefaultValue) {
				t.Errorf("%s.%s: default %s, Core %s", name, f.Name, defaultString(f.DefaultValue), defaultString(cf.DefaultValue))
			}
			coreArgs := map[string]*ast.ArgumentDefinition{}
			for _, a := range cf.Arguments {
				coreArgs[a.Name] = a
			}
			for _, a := range f.Arguments {
				ca, ok := coreArgs[a.Name]
				if !ok {
					t.Errorf("%s.%s(%s) is not in Core", name, f.Name, a.Name)
					continue
				}
				if a.Type.String() != ca.Type.String() {
					t.Errorf("%s.%s(%s): type %s, Core %s", name, f.Name, a.Name, a.Type, ca.Type)
				}
			}
		}
	}
}

func defaultString(v *ast.Value) string {
	if v == nil {
		return ""
	}
	return strings.TrimSpace(v.String())
}
