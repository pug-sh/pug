package assistant

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"buf.build/gen/go/bufbuild/protovalidate/protocolbuffers/go/buf/validate"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	dashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/dashboards/v1"
	dashboardsv1mcp "github.com/pug-sh/pug/internal/gen/proto/dashboard/dashboards/v1/dashboardsv1mcp"
)

// tileShapeJSON is the JSON Schema for the tile the model authors, derived from
// the generated Upsert schema so a proto change reshapes it instead of leaving
// a stale prompt string.
var tileShapeJSON = sync.OnceValue(func() string {
	var root map[string]any
	if err := json.Unmarshal(dashboardsv1mcp.DashboardsService_UpsertTool.RawInputSchema, &root); err != nil {
		panic(fmt.Sprintf("assistant: upsert schema does not parse: %v", err))
	}

	tile, err := descend(root, "properties", "tiles", "items")
	if err != nil {
		panic(fmt.Sprintf("assistant: locating tile schema: %v", err))
	}
	props, err := descend(tile, "properties")
	if err != nil {
		panic(fmt.Sprintf("assistant: locating tile properties: %v", err))
	}

	// submit() assigns both, and a model-supplied value is discarded.
	delete(props, "id")
	delete(props, "position")

	if err := flattenContentOneof(tile, props); err != nil {
		panic(fmt.Sprintf("assistant: flattening content oneof: %v", err))
	}

	raw, err := json.Marshal(tile)
	if err != nil {
		panic(fmt.Sprintf("assistant: tile schema does not marshal: %v", err))
	}
	return string(raw)
})

// flattenContentOneof rewrites the generated {"which","insight","markdown"}
// discriminator into the flat "insight" key of real proto3 JSON — the shape
// tileFromJSON actually parses. Markdown tiles are not authored.
func flattenContentOneof(tile, props map[string]any) error {
	content, err := descend(props, "content", "properties")
	if err != nil {
		return err
	}
	insight, ok := content["insight"]
	if !ok {
		return errors.New("content oneof has no insight member")
	}
	if m, ok := insight.(map[string]any); ok {
		delete(m, "description")
	}
	delete(props, "content")
	props["insight"] = insight
	tile["required"] = []string{"insight"}
	return nil
}

func descend(m map[string]any, keys ...string) (map[string]any, error) {
	cur := m
	for _, k := range keys {
		next, ok := cur[k].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("no object at %q", k)
		}
		cur = next
	}
	return cur, nil
}

// tileRules lists the protovalidate CEL rules reachable from a tile. These are
// cross-field constraints JSON Schema cannot express, and they are the same
// messages the repair loop hands back after the model has already broken one.
var tileRules = sync.OnceValue(func() string {
	seen := map[protoreflect.FullName]bool{}
	unique := map[string]bool{}
	var rules []string
	collectRules((&dashboardsv1.DashboardTileInput{}).ProtoReflect().Descriptor(), seen, unique, &rules)
	sort.Strings(rules)

	var b strings.Builder
	for _, r := range rules {
		b.WriteString("- ")
		b.WriteString(r)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
})

func collectRules(md protoreflect.MessageDescriptor, seen map[protoreflect.FullName]bool, unique map[string]bool, out *[]string) {
	if seen[md.FullName()] {
		return
	}
	seen[md.FullName()] = true

	if r, ok := proto.GetExtension(md.Options(), validate.E_Message).(*validate.MessageRules); ok && r != nil {
		for _, c := range r.GetCel() {
			appendRule(c.GetMessage(), unique, out)
		}
	}

	fields := md.Fields()
	for i := range fields.Len() {
		f := fields.Get(i)
		if r, ok := proto.GetExtension(f.Options(), validate.E_Field).(*validate.FieldRules); ok && r != nil {
			for _, c := range r.GetCel() {
				appendRule(c.GetMessage(), unique, out)
			}
		}
		if f.Kind() == protoreflect.MessageKind || f.Kind() == protoreflect.GroupKind {
			collectRules(f.Message(), seen, unique, out)
		}
	}
}

func appendRule(msg string, unique map[string]bool, out *[]string) {
	if msg == "" || unique[msg] {
		return
	}
	unique[msg] = true
	*out = append(*out, msg)
}
