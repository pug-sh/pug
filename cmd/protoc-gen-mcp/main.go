// Command protoc-gen-mcp is a thin in-repo wrapper around
// redpanda-data/protoc-gen-go-mcp that additionally declares support for
// protobuf editions.
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
	"flag"

	"github.com/redpanda-data/protoc-gen-go-mcp/pkg/generator"
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/pluginpb"
)

func main() {
	var flagSet flag.FlagSet
	packageSuffix := flagSet.String(
		"package_suffix",
		"mcp",
		"Generate files into a sub-package of the package containing the base .pb.go files using the given suffix. An empty suffix denotes to generate into the same package as the base pb.go files.",
	)

	protogen.Options{ParamFunc: flagSet.Set}.Run(func(gen *protogen.Plugin) error {
		// The only delta from upstream cmd/protoc-gen-go-mcp: advertise editions
		// support so buf will run us on `edition = "2023"` protos.
		gen.SupportedFeatures |= uint64(pluginpb.CodeGeneratorResponse_FEATURE_SUPPORTS_EDITIONS)
		gen.SupportedEditionsMinimum = descriptorpb.Edition_EDITION_PROTO2
		gen.SupportedEditionsMaximum = descriptorpb.Edition_EDITION_2023

		for _, f := range gen.Files {
			if !f.Generate {
				continue
			}
			generator.NewFileGenerator(f, gen).Generate(*packageSuffix)
		}
		return nil
	})
}
