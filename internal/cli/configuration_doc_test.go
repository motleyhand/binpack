package cli

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/motleyhand/binpack/api/v1alpha1"
)

// The reference lives beside the diagnostics one and for the same reason: the
// CLI is what points people at these documents, and it is the package allowed
// to read the filesystem.
const configurationReference = "../../docs/reference/configuration.md"

var yamlBlockRE = regexp.MustCompile("(?s)```yaml\n(.*?)```")

// TestEveryYAMLExampleInTheConfigurationReferenceLoads runs the document's own
// examples through the loader.
//
// An operator copies these. A wrong one does not degrade to a default — the
// loader is strict, so a configuration binpack cannot parse is a binpack that
// does not start, and the reader has no reason to suspect the document rather
// than themselves. Prose about the format can only be checked by reading;
// examples can be executed, so they are.
//
// Uniform leading indentation is stripped, because some examples are nested
// inside a list item in the Markdown and the indentation is the document's
// layout rather than the configuration's.
func TestEveryYAMLExampleInTheConfigurationReferenceLoads(t *testing.T) {
	data, err := os.ReadFile(configurationReference)
	if err != nil {
		t.Fatalf("reading the configuration reference: %v", err)
	}

	matches := yamlBlockRE.FindAllStringSubmatch(string(data), -1)
	if len(matches) == 0 {
		t.Fatal("no yaml examples found: the extraction is wrong, not the document")
	}

	for _, m := range matches {
		block := dedent(m[1])
		first, _, _ := strings.Cut(strings.TrimSpace(block), "\n")

		t.Run(first, func(t *testing.T) {
			if _, err := v1alpha1.Load([]byte(block)); err != nil {
				t.Errorf("this example does not load:\n%s\n%v", block, err)
			}
		})
	}
}

// dedent removes the smallest indentation common to every non-blank line.
func dedent(s string) string {
	lines := strings.Split(s, "\n")

	width := -1
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if n := len(l) - len(strings.TrimLeft(l, " ")); width < 0 || n < width {
			width = n
		}
	}
	if width <= 0 {
		return s
	}

	for i, l := range lines {
		if len(l) >= width {
			lines[i] = l[width:]
		}
	}
	return strings.Join(lines, "\n")
}

// The task pages are where an operator copies a configuration fragment from,
// so they are the pages a field name that does not exist does the most damage
// on. The reference pages are excluded deliberately rather than by oversight:
// cli.md documents `pools.source` and friends, which are `diagnose --output
// json` fields and not configuration at all, and the ADRs name `pools[].minSize`
// and `pools[].nodeSelector` — designs that were considered and rejected, so a
// path resolving there would be the failure.
const howToDirectory = "../../docs/how-to"

// Two ways a path is named in Markdown, and both are load-bearing. A fenced
// block is what gets copied; backticked prose is what gets typed from memory.
var backtickRE = regexp.MustCompile("`([^`\n]+)`")

// A path may name one entry of a list — `pools[]`, or `pools[0]` — and the
// index says nothing about the field.
var pathIndexRE = regexp.MustCompile(`\[[^\]]*\]$`)

// TestEveryConfigurationPathInTheTaskPagesIsAField resolves every
// configuration path the how-to pages name against the wire type.
//
// This exists because nothing held the documentation to the type, and the
// documentation drifted off it: one page instructed the reader to disable a
// pool with `policy.byPool.<name>.enabled`, a field binpack has never had.
// That is the worst shape a documentation error takes here, because the loader
// rejects unknown fields — so the reader who follows the page gets a binpack
// that will not start, with an error naming their file rather than ours, and
// no reason to suspect the page.
//
// The prose half cannot recognise a path whose *first* segment is invented,
// since nothing distinguishes `byPool.<name>` from any other backticked word.
// It anchors on a real top-level field and checks the rest, which is where the
// invented segments have actually appeared.
func TestEveryConfigurationPathInTheTaskPagesIsAField(t *testing.T) {
	entries, err := os.ReadDir(howToDirectory)
	if err != nil {
		t.Fatalf("reading the how-to directory: %v", err)
	}

	// A guard over a corpus it failed to extract anything from passes for the
	// wrong reason. Both extractions are counted, because either one silently
	// finding nothing would leave half the rule unenforced.
	fenced, prose := 0, 0

	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".md" {
			continue
		}

		data, err := os.ReadFile(filepath.Join(howToDirectory, entry.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", entry.Name(), err)
		}

		t.Run(entry.Name(), func(t *testing.T) {
			yamlPaths, err := fencedConfigurationPaths(string(data))
			if err != nil {
				t.Error(err)
			}
			prosePaths := backtickedConfigurationPaths(string(data))
			fenced += len(yamlPaths)
			prose += len(prosePaths)

			for _, path := range slices.Concat(yamlPaths, prosePaths) {
				if err := resolveConfigurationPath(path); err != nil {
					t.Errorf("this page names %s: %v", path, err)
				}
			}
		})
	}

	if fenced == 0 {
		t.Error("no configuration paths found in any fenced block: the extraction is wrong, not the pages")
	}
	if prose == 0 {
		t.Error("no configuration paths found in backticks: the extraction is wrong, not the pages")
	}
}

// fencedConfigurationPaths returns every path a page's YAML examples name.
//
// Only two block shapes are a configuration document. A raw one declares the
// apiVersion, or leads with a section of the policy; a chart values block
// carries the whole document under `config`, which the chart writes into the
// ConfigMap with `toYaml` and no filtering — so every key under it is a field
// of this type, not only the ones under policy. Everything else on these pages
// is a Kubernetes manifest or another chart's values, and belongs to nobody
// here.
//
// Two ways a block can yield nothing, and only one of them is acceptable. A
// fence that is valid YAML but not a mapping is a fragment nobody here owns,
// and is skipped. A fence that does not parse at all is a defect on its own
// terms — it is tagged yaml on a page an operator copies from — and skipping it
// would take every path in it out of the corpus at exactly the moment the
// corpus stopped being trustworthy. Every failing block is named rather than
// the first, which is how the loader reports a bad document too.
func fencedConfigurationPaths(doc string) ([]string, error) {
	var paths []string
	var problems []error

	for _, match := range yamlBlockRE.FindAllStringSubmatch(doc, -1) {
		var document any
		if err := yaml.Unmarshal([]byte(dedent(match[1])), &document); err != nil {
			first, _, _ := strings.Cut(strings.TrimSpace(match[1]), "\n")
			problems = append(problems, fmt.Errorf("the yaml block starting %q does not parse: %w", first, err))
			continue
		}

		block, mapping := document.(map[string]any)
		if !mapping {
			continue
		}

		// Classified the way the loader reads it, which is case-insensitively:
		// resolving paths that way and then choosing the root by exact key
		// would drop a whole `Policy:` fence before resolution ever saw it.
		root := block
		config, wrapped := lookup(block, "config")
		apiVersion, _ := lookup(block, "apiVersion")
		_, policy := lookup(block, "policy")
		_, pools := lookup(block, "pools")

		switch nested, nestedIsMapping := config.(map[string]any); {
		case wrapped && nestedIsMapping:
			root = nested
		// The group version is a value rather than a field name, so the loader
		// does not case-fold it and neither does this.
		case apiVersion == v1alpha1.GroupVersion:
		case policy || pools:
		default:
			continue
		}

		paths = append(paths, keyPaths("", root)...)
	}

	slices.Sort(paths)
	return slices.Compact(paths), errors.Join(problems...)
}

// keyPaths flattens a decoded document into the dotted paths its keys spell.
func keyPaths(prefix string, node map[string]any) []string {
	var paths []string

	for key, value := range node {
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}

		switch value := value.(type) {
		case map[string]any:
			// The mapping's own key first. Descending straight into it loses
			// `polciy: {}` entirely — an empty mapping contributes no path, so
			// a misspelling with nothing under it would be invisible to a
			// guard that only records leaves.
			paths = append(paths, path)
			paths = append(paths, keyPaths(path, value)...)
		case []any:
			paths = append(paths, path)
			for _, element := range value {
				if element, ok := element.(map[string]any); ok {
					paths = append(paths, keyPaths(path+"[]", element)...)
				}
			}
		default:
			paths = append(paths, path)
		}
	}

	return paths
}

// backtickedConfigurationPaths returns every path a page names in prose.
//
// A backticked span is a configuration path when its first segment is a
// top-level field of the document — the only anchor available, since prose has
// no syntax that says "this is configuration". A trailing value is dropped, so
// `dryRun: false` names the field `dryRun`.
func backtickedConfigurationPaths(doc string) []string {
	top := jsonFields(reflect.TypeFor[v1alpha1.Config]())

	var paths []string
	for _, match := range backtickRE.FindAllStringSubmatch(doc, -1) {
		path, _, _ := strings.Cut(match[1], ":")
		path = strings.TrimSpace(path)

		if path == "" || strings.ContainsAny(path, " \t/=") {
			continue
		}
		head, _, _ := strings.Cut(path, ".")
		if _, ok := lookup(top, pathIndexRE.ReplaceAllString(head, "")); !ok {
			continue
		}

		paths = append(paths, path)
	}

	slices.Sort(paths)
	return slices.Compact(paths)
}

// resolveConfigurationPath walks a dotted path down the wire type by JSON tag,
// which is the name an operator writes rather than the Go one.
func resolveConfigurationPath(path string) error {
	typ := reflect.TypeFor[v1alpha1.Config]()

	for _, segment := range strings.Split(path, ".") {
		name := pathIndexRE.ReplaceAllString(segment, "")

		typ = underlying(typ)
		if typ.Kind() != reflect.Struct {
			return fmt.Errorf("%q has no fields under it", strings.TrimSuffix(path, "."+segment))
		}

		fields := jsonFields(typ)
		field, ok := lookup(fields, name)
		if !ok {
			known := slices.Sorted(maps.Keys(fields))
			return fmt.Errorf("%q is not a field of %s, which has %s",
				name, typ.Name(), strings.Join(known, ", "))
		}
		typ = field
	}

	return nil
}

// jsonFields maps a struct's JSON field names to their types, flattening the
// inline ones. PoolOverride inlines Policy, so a pool entry spells a policy
// field at its own level: `pools[].enabled`, never `pools[].policy.enabled`.
func jsonFields(typ reflect.Type) map[string]reflect.Type {
	fields := map[string]reflect.Type{}

	for field := range typ.Fields() {
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")

		switch {
		case name == "" && field.Anonymous:
			maps.Copy(fields, jsonFields(underlying(field.Type)))
		case name == "" || name == "-":
		default:
			fields[name] = field.Type
		}
	}

	return fields
}

// lookup reads a name the way the loader does, which is case-insensitively:
// YAML is parsed through encoding/json, so `dryrun` and `dryRun` are the same
// field. Matching exactly would let a differently-cased name — `Policy.byPool`
// at the start of a sentence — fall out of the corpus unchecked, taking its
// invented tail with it. Exact first, so an error message quotes the real
// spelling rather than whichever case it happened to iterate onto.
//
// Used over both a decoded document and a struct's fields, because the rule is
// the loader's and does not care which of the two it is reading.
func lookup[V any](m map[string]V, name string) (V, bool) {
	if value, ok := m[name]; ok {
		return value, true
	}
	for key, value := range m {
		if strings.EqualFold(key, name) {
			return value, true
		}
	}

	var absent V
	return absent, false
}

// underlying strips the pointers and slices a path segment does not spell.
func underlying(typ reflect.Type) reflect.Type {
	for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice {
		typ = typ.Elem()
	}
	return typ
}
