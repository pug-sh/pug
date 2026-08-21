package lint

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

var consumerLookup = regexp.MustCompile(`\.GetConsumerConfigByName\(([^)]*)\)`)

func checkNATSConsumers(root string) ([]string, error) {
	var doc struct {
		Consumers []struct {
			Name        string `yaml:"name"`
			DurableName string `yaml:"durable_name"`
			StreamName  string `yaml:"stream_name"`
		} `yaml:"consumers"`
	}
	body, err := os.ReadFile(filepath.Join(root, "schema/nats/consumers.yaml"))
	if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	declared := map[string]bool{}
	for _, c := range doc.Consumers {
		declared[c.Name], declared[c.DurableName] = true, true
	}

	var streams struct {
		Streams []struct {
			Name string `yaml:"name"`
		} `yaml:"streams"`
	}
	body, err = os.ReadFile(filepath.Join(root, "schema/nats/streams.yaml"))
	if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(body, &streams); err != nil {
		return nil, err
	}
	knownStream := map[string]bool{}
	for _, s := range streams.Streams {
		knownStream[s.Name] = true
	}

	var out []string
	for _, c := range doc.Consumers {
		if !knownStream[c.StreamName] {
			out = append(out, fmt.Sprintf("schema/nats/consumers.yaml: consumer %q binds unknown stream %q", c.Name, c.StreamName))
		}
	}
	err = walkGo(root, func(path string, body []byte) {
		for _, m := range consumerLookup.FindAllSubmatch(body, -1) {
			arg := strings.TrimSpace(string(m[1]))
			name, err := strconv.Unquote(arg)
			if err != nil {
				// A hoisted const would otherwise opt the worker out silently.
				out = append(out, fmt.Sprintf("%s: consumer name %s is not a literal, so it cannot be checked against schema/nats/consumers.yaml", rel(root, path), arg))
				continue
			}
			if !declared[name] {
				out = append(out, fmt.Sprintf("%s: consumer %q is not declared in schema/nats/consumers.yaml", rel(root, path), name))
			}
		}
	})
	return out, err
}
