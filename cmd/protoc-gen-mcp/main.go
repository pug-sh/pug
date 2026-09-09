// Command protoc-gen-mcp is a thin in-repo wrapper around
// redpanda-data/protoc-gen-go-mcp that declares support for protobuf editions
// and repairs one import the upstream generator gets wrong.
//
// The upstream plugin only advertises FEATURE_PROTO3_OPTIONAL, so buf refuses to
// run it against our `edition = "2023"` protos ("plugin does not support
// editions"). This wrapper imports the plugin's exported pkg/generator and sets
// the editions feature bits on the protogen.Plugin before delegating to the
// exact same generation call the upstream main performs.
//
// Delete this command and point buf.gen.yaml at the upstream
// `protoc-gen-go-mcp` binary once it declares editions support itself.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/redpanda-data/protoc-gen-go-mcp/pkg/generator"
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/pluginpb"
)

// runtime.Tool's schema fields are encoding/json's RawMessage, which Go 1.25's
// json/v2 makes an ALIAS for jsontext.Value, so the upstream generator emits
// `jsontext.Value` while importing only encoding/json. Rewritten back to the alias.
var (
	emittedType = []byte("jsontext.Value{")
	importedAs  = []byte("json.RawMessage{")
)

func main() {
	var flagSet flag.FlagSet
	packageSuffix := flagSet.String(
		"package_suffix",
		"mcp",
		"Generate files into a sub-package of the package containing the base .pb.go files using the given suffix. An empty suffix denotes to generate into the same package as the base pb.go files.",
	)

	// Not protogen.Options.Run: that writes the response itself, and the import
	// repair below has to happen after generation and before it is written.
	opts := protogen.Options{ParamFunc: flagSet.Set}
	req, err := readRequest()
	if err != nil {
		fail(err)
	}
	gen, err := opts.New(req)
	if err != nil {
		fail(err)
	}

	// Advertise editions support so buf will run us on `edition = "2023"` protos.
	gen.SupportedFeatures |= uint64(pluginpb.CodeGeneratorResponse_FEATURE_SUPPORTS_EDITIONS)
	gen.SupportedEditionsMinimum = descriptorpb.Edition_EDITION_PROTO2
	gen.SupportedEditionsMaximum = descriptorpb.Edition_EDITION_2023

	for _, f := range gen.Files {
		if !f.Generate {
			continue
		}
		// Upstream emits one MCP tool per UNARY method (generator.go skips
		// streaming methods) but still writes the file scaffold. For a file
		// whose services are streaming-only (ai/dashboards/v1/assistant.proto)
		// that scaffold has unused imports and does not compile, so skip such
		// files entirely — the same outcome upstream already produces for
		// files with no services at all.
		if !hasUnaryMethod(f) {
			continue
		}
		generator.NewFileGenerator(f, gen).Generate(*packageSuffix)
	}

	resp := gen.Response()
	for _, f := range resp.File {
		if f.Content == nil {
			continue
		}
		f.Content = proto.String(string(bytes.ReplaceAll([]byte(f.GetContent()), emittedType, importedAs)))
	}
	out, err := proto.Marshal(resp)
	if err != nil {
		fail(err)
	}
	if _, err := os.Stdout.Write(out); err != nil {
		fail(err)
	}
}

func readRequest() (*pluginpb.CodeGeneratorRequest, error) {
	in, err := io.ReadAll(os.Stdin)
	if err != nil {
		return nil, err
	}
	req := &pluginpb.CodeGeneratorRequest{}
	if err := proto.Unmarshal(in, req); err != nil {
		return nil, err
	}
	return req, nil
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "protoc-gen-mcp: %v\n", err)
	os.Exit(1)
}

func hasUnaryMethod(f *protogen.File) bool {
	for _, svc := range f.Services {
		for _, m := range svc.Methods {
			if !m.Desc.IsStreamingClient() && !m.Desc.IsStreamingServer() {
				return true
			}
		}
	}
	return false
}
