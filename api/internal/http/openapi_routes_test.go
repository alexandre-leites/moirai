package server

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The API's OpenAPI document is hand-written, so nothing but a test stops it
// from drifting away from the routes the service actually serves. Issue #368
// was exactly that: GET /api/v1/queue shipped, the console called it, and the
// spec never mentioned it. These tests compare the two directions -- a route
// with no operation, and an operation with no route -- so either kind of drift
// fails the `api` job rather than surviving to a generated client.
//
// Route discovery reads the handler sources rather than a live mux: net/http
// exposes no way to enumerate a ServeMux's patterns, and constructing every
// handler would drag in an orchestrator connection this package has no reason
// to need. The spec is read with a small line scanner because the api module
// deliberately carries no YAML dependency; the document's paths and component
// names are plain block-style keys at fixed indentation, which is all the
// scanner relies on.

const openAPIPath = "../../openapi.yaml"

// routeSourceDirs are the package directories that register routes on the
// server's mux, relative to this package.
var routeSourceDirs = []string{".", "handlers"}

var httpMethods = map[string]bool{
	"get": true, "post": true, "put": true, "patch": true,
	"delete": true, "head": true, "options": true,
}

func TestOpenAPIDocumentsEveryRegisteredRoute(t *testing.T) {
	registered := registeredRoutes(t)
	documented := documentedOperations(t)

	if len(registered) == 0 {
		t.Fatal("found no registered routes; the source scanner is broken")
	}
	for _, route := range sortedKeys(registered) {
		if !documented[route] {
			t.Errorf("route %s is registered but absent from %s", route, openAPIPath)
		}
	}
	for _, operation := range sortedKeys(documented) {
		if !registered[operation] {
			t.Errorf("operation %s is documented in %s but no handler registers it", operation, openAPIPath)
		}
	}
}

// TestOpenAPIReferencesResolve catches the other way a hand-written spec rots:
// an operation pointing at a component that was never defined. Both
// `responses/OrchestratorTimeout` and `schemas/Problem` were dangling when
// issue #368 was fixed.
func TestOpenAPIReferencesResolve(t *testing.T) {
	defined, referenced := openAPIComponents(t)
	if len(referenced) == 0 {
		t.Fatal("found no component references; the spec scanner is broken")
	}
	for _, ref := range sortedKeys(referenced) {
		if !defined[ref] {
			t.Errorf("%s references #/%s, which is not defined", openAPIPath, ref)
		}
	}
}

// registeredRoutes returns the set of "METHOD /path" patterns passed to
// ServeMux.Handle and ServeMux.HandleFunc across the route source directories.
func registeredRoutes(t *testing.T) map[string]bool {
	t.Helper()
	routes := map[string]bool{}
	for _, dir := range routeSourceDirs {
		fileSet := token.NewFileSet()
		files := parseSourceDir(t, fileSet, dir)
		constants := stringConstants(files)
		for _, file := range files {
			ast.Inspect(file, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok || len(call.Args) == 0 {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || (selector.Sel.Name != "Handle" && selector.Sel.Name != "HandleFunc") {
					return true
				}
				// Every registration in this service is on a receiver named
				// mux or s.mux. Narrowing to that keeps unrelated Handle
				// methods out without having to type-check the expression.
				if !strings.Contains(strings.ToLower(exprName(selector.X)), "mux") {
					return true
				}
				pattern, err := foldString(call.Args[0], constants)
				if err != nil {
					// Failing loudly matters more than the missed route: a
					// pattern this scanner cannot read is a route the test
					// would otherwise silently stop guarding.
					t.Fatalf("%s: cannot resolve route pattern: %v", fileSet.Position(call.Pos()), err)
				}
				method, path, found := strings.Cut(pattern, " ")
				if !found || !strings.HasPrefix(path, "/") {
					t.Fatalf("%s: route pattern %q is not \"METHOD /path\"", fileSet.Position(call.Pos()), pattern)
				}
				routes[method+" "+path] = true
				return true
			})
		}
	}
	return routes
}

// parseSourceDir parses every non-test Go file in dir.
func parseSourceDir(t *testing.T, fileSet *token.FileSet, dir string) []*ast.File {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var files []*ast.File
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fileSet, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files = append(files, file)
	}
	return files
}

// stringConstants collects package-level string constants so patterns built by
// concatenation, such as "GET "+apiPathPrefix+"/health", still resolve.
func stringConstants(files []*ast.File) map[string]string {
	constants := map[string]string{}
	for _, file := range files {
		for _, decl := range file.Decls {
			generic, ok := decl.(*ast.GenDecl)
			if !ok || generic.Tok != token.CONST {
				continue
			}
			for _, spec := range generic.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range value.Names {
					if i >= len(value.Values) {
						continue
					}
					literal, ok := value.Values[i].(*ast.BasicLit)
					if !ok || literal.Kind != token.STRING {
						continue
					}
					if unquoted, err := strconv.Unquote(literal.Value); err == nil {
						constants[name.Name] = unquoted
					}
				}
			}
		}
	}
	return constants
}

// foldString evaluates a string expression built from literals, package-level
// string constants and `+`.
func foldString(expr ast.Expr, constants map[string]string) (string, error) {
	switch node := expr.(type) {
	case *ast.BasicLit:
		if node.Kind != token.STRING {
			return "", fmt.Errorf("literal is not a string")
		}
		return strconv.Unquote(node.Value)
	case *ast.Ident:
		value, ok := constants[node.Name]
		if !ok {
			return "", fmt.Errorf("unknown constant %q", node.Name)
		}
		return value, nil
	case *ast.BinaryExpr:
		if node.Op != token.ADD {
			return "", fmt.Errorf("unsupported operator %s", node.Op)
		}
		left, err := foldString(node.X, constants)
		if err != nil {
			return "", err
		}
		right, err := foldString(node.Y, constants)
		if err != nil {
			return "", err
		}
		return left + right, nil
	case *ast.ParenExpr:
		return foldString(node.X, constants)
	default:
		return "", fmt.Errorf("unsupported expression %T", expr)
	}
}

func exprName(expr ast.Expr) string {
	switch node := expr.(type) {
	case *ast.Ident:
		return node.Name
	case *ast.SelectorExpr:
		return node.Sel.Name
	default:
		return ""
	}
}

// documentedOperations returns the set of "METHOD /path" operations declared
// under the spec's top-level `paths` mapping.
func documentedOperations(t *testing.T) map[string]bool {
	t.Helper()
	operations := map[string]bool{}
	pathKey := regexp.MustCompile(`^ {2}(/\S*):\s*$`)
	methodKey := regexp.MustCompile(`^ {4}([a-z]+):\s*$`)
	currentPath := ""
	inPaths := false
	for _, line := range specLines(t) {
		if isTopLevelKey(line) {
			inPaths = strings.HasPrefix(line, "paths:")
			currentPath = ""
			continue
		}
		if !inPaths {
			continue
		}
		if match := pathKey.FindStringSubmatch(line); match != nil {
			currentPath = match[1]
			continue
		}
		if match := methodKey.FindStringSubmatch(line); match != nil && httpMethods[match[1]] {
			if currentPath == "" {
				t.Fatalf("operation %q appears before any path key", match[1])
			}
			operations[strings.ToUpper(match[1])+" "+currentPath] = true
		}
	}
	return operations
}

// openAPIComponents returns the component names the spec defines and the ones
// it references, both as "components/<section>/<name>".
func openAPIComponents(t *testing.T) (defined, referenced map[string]bool) {
	t.Helper()
	defined, referenced = map[string]bool{}, map[string]bool{}
	sectionKey := regexp.MustCompile(`^ {2}([A-Za-z]+):\s*$`)
	nameKey := regexp.MustCompile(`^ {4}([A-Za-z0-9_]+):\s*$`)
	reference := regexp.MustCompile(`#/(components/[A-Za-z]+/[A-Za-z0-9_]+)`)
	inComponents := false
	section := ""
	for _, line := range specLines(t) {
		for _, match := range reference.FindAllStringSubmatch(line, -1) {
			referenced[match[1]] = true
		}
		if isTopLevelKey(line) {
			inComponents = strings.HasPrefix(line, "components:")
			section = ""
			continue
		}
		if !inComponents {
			continue
		}
		if match := sectionKey.FindStringSubmatch(line); match != nil {
			section = match[1]
			continue
		}
		if match := nameKey.FindStringSubmatch(line); match != nil && section != "" {
			defined["components/"+section+"/"+match[1]] = true
		}
	}
	return defined, referenced
}

func specLines(t *testing.T) []string {
	t.Helper()
	content, err := os.ReadFile(filepath.Clean(openAPIPath))
	if err != nil {
		t.Fatalf("read %s: %v", openAPIPath, err)
	}
	return strings.Split(string(content), "\n")
}

// isTopLevelKey reports whether the line opens an unindented mapping key, which
// is what ends the `paths` and `components` sections.
func isTopLevelKey(line string) bool {
	trimmed := strings.TrimRight(line, " \t")
	if trimmed == "" || strings.HasPrefix(trimmed, " ") || strings.HasPrefix(trimmed, "#") {
		return false
	}
	return strings.Contains(trimmed, ":")
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
