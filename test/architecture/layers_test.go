package architecture_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// modulePath is the import prefix of this project. Everything below
// internal/ is ours; everything else is the standard library or a dependency,
// and this test has nothing to say about those.
const modulePath = "github.com/ironledger/iron-ledger/internal/"

// layer describes one level of the codebase and the layers it may depend on.
//
// The names are the directories directly under internal/. A package is in the
// layer named after its first path element below internal/, which is why
// domain/wallet and domain/wagering are both "domain": nesting them is exactly
// what makes them a distinct kind of thing from pgdb.
type layer struct {
	name string
	// mayImport lists the layers this one is allowed to depend on. A layer
	// may always depend on itself and on the standard library.
	mayImport []string
}

// layers is the dependency policy, from ARCHITECTURE.md section 2.
//
// The graph is a DAG by construction: app sits on top and composes, the
// contexts sit in the middle and know only infrastructure, and sharedkernel is
// the floor that depends on nothing of ours.
var layers = []layer{
	{
		// The floor. money, ids, idempotency and xerr are depended on by
		// everything, so anything they imported would become a cycle waiting
		// to happen.
		name:      "sharedkernel",
		mayImport: nil,
	},
	{
		// Infrastructure seams: the transaction manager, the event store, the
		// logger. A context uses these; the reverse would mean the database
		// knows what a bet is.
		name:      "platform",
		mayImport: []string{"sharedkernel"},
	},
	{
		// The business model. It may reach for infrastructure and the shared
		// kernel, never for the adapters that carry its decisions outward and
		// never for the application layer that composes them.
		name:      "domain",
		mayImport: []string{"platform", "sharedkernel", "domain"},
	},
	{
		// Adapters. messaging and identity know about queues and tokens;
		// neither knows what a wager is.
		name:      "messaging",
		mayImport: []string{"platform", "sharedkernel"},
	},
	{
		name:      "identity",
		mayImport: []string{"platform", "sharedkernel"},
	},
	{
		// The only layer that sees the whole graph, because composition is
		// its job.
		name: "app",
		mayImport: []string{
			"domain", "messaging", "identity", "platform", "sharedkernel",
		},
	},
}

// goListPackage mirrors the fields of `go list -json` that this test reads.
// Only what is needed is declared, so the test does not break when go adds
// fields to its output.
type goListPackage struct {
	ImportPath string   `json:"ImportPath"`
	Dir        string   `json:"Dir"`
	Imports    []string `json:"Imports"`
}

// repoRoot is the directory holding go.mod. Tests run with their own package
// directory as the working directory, and `go list ./internal/...` means
// nothing from there, so the command is run from the root instead.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving the repository root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("no go.mod at %s: %v", root, err)
	}
	return root
}

// internalPackages returns every package under internal/, with its imports.
func internalPackages(t *testing.T) []goListPackage {
	t.Helper()

	cmd := exec.Command("go", "list", "-json", "./internal/...")
	cmd.Dir = repoRoot(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list ./internal/... failed: %v\n%s", err, out)
	}

	var packages []goListPackage
	decoder := json.NewDecoder(strings.NewReader(string(out)))
	for decoder.More() {
		var pkg goListPackage
		if err := decoder.Decode(&pkg); err != nil {
			t.Fatalf("decoding go list output: %v", err)
		}
		packages = append(packages, pkg)
	}
	if len(packages) == 0 {
		t.Fatal("go list returned no packages under internal/")
	}
	return packages
}

// layerOf returns the layer a package belongs to, or "" when it is not under
// internal/.
func layerOf(importPath string) string {
	if !strings.HasPrefix(importPath, modulePath) {
		return ""
	}
	rest := strings.TrimPrefix(importPath, modulePath)
	first, _, _ := strings.Cut(rest, "/")
	return first
}

// layerByName finds a layer definition by name.
func layerByName(t *testing.T, name string) layer {
	t.Helper()
	for _, l := range layers {
		if l.name == name {
			return l
		}
	}
	t.Fatalf("no layer named %q is declared in the policy; the policy and the directory tree have drifted apart", name)
	return layer{}
}

// relative turns an absolute directory into a path relative to the repository
// root, so a failure message reads `internal/domain/wallet/...` instead of an
// absolute path that means nothing on another machine.
func relative(t *testing.T, dir string) string {
	t.Helper()
	rel, err := filepath.Rel(repoRoot(t), dir)
	if err != nil {
		return dir
	}
	return rel
}

func TestEveryInternalPackageIsInADeclaredLayer(t *testing.T) {
	declared := make(map[string]bool, len(layers))
	for _, l := range layers {
		declared[l.name] = true
	}

	for _, pkg := range internalPackages(t) {
		name := layerOf(pkg.ImportPath)
		if !declared[name] {
			t.Errorf("%s is in %q, which no layer declares.\n"+
				"A directory directly under internal/ must be classified, or the layering is only "+
				"as good as the last edit. Add a layer entry in this file and in ARCHITECTURE.md.",
				relative(t, pkg.Dir), name)
		}
	}
}

func TestImportsRespectTheLayering(t *testing.T) {
	for _, pkg := range internalPackages(t) {
		from := layerOf(pkg.ImportPath)
		policy := layerByName(t, from)
		allowed := map[string]bool{from: true}
		for _, name := range policy.mayImport {
			allowed[name] = true
		}

		for _, imported := range pkg.Imports {
			to := layerOf(imported)
			if to == "" || allowed[to] {
				continue
			}
			t.Errorf("%s may not import %s.\n"+
				"%q may depend on %s, but %q is not among them. "+
				"If this import is genuinely needed, the layering is wrong; if it is not, the import is noise.",
				relative(t, pkg.Dir), relative(t, importedDir(t, imported)),
				from, strings.Join(sortedKeys(allowed), ", "), to)
		}
	}
}

// importedDir resolves the directory of an imported package. It is only used
// for the failure message, so a failure to resolve is reported rather than
// fatal.
func importedDir(t *testing.T, importPath string) string {
	t.Helper()
	cmd := exec.Command("go", "list", "-f", "{{.Dir}}", importPath)
	cmd.Dir = repoRoot(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return importPath
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		return importPath
	}
	return relative(t, dir)
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TestSharedKernelDependsOnNothingOurs is spelled out separately from the table
// above because it is the one rule that makes sharedkernel a shared kernel
// rather than a second platform package: the moment it reaches outside itself,
// every context transitively depends on every context.
//
// Same-layer imports are fine and expected — idempotency using money is the
// kernel doing its job. What is forbidden is reaching up into a context.
func TestSharedKernelDependsOnNothingOurs(t *testing.T) {
	for _, pkg := range internalPackages(t) {
		if layerOf(pkg.ImportPath) != "sharedkernel" {
			continue
		}
		for _, imported := range pkg.Imports {
			if !strings.HasPrefix(imported, modulePath) {
				continue
			}
			if layerOf(imported) == "sharedkernel" {
				continue
			}
			t.Errorf("%s imports %s.\n"+
				"sharedkernel is the floor of the dependency graph. If money or xerr "+
				"needs something from a context, that something belongs in "+
				"sharedkernel instead, not the other way around.",
				relative(t, pkg.Dir), importedDir(t, imported))
		}
	}
}

// TestContextsDoNotImportAdapters is the rule that keeps the hexagonal shape:
// a context states what is true of the business, and something else decides
// where that truth is stored and how it leaves the process.
//
// This is listed on its own because it is the boundary most likely to be
// crossed by accident — a slice that needs to publish something reaches for the
// outbox, which feels harmless because the outbox is in the same repository.
func TestContextsDoNotImportAdapters(t *testing.T) {
	forbidden := map[string]string{
		"messaging": "a context must not talk to the broker; it appends an event and the outbox delivers it",
		"identity":  "a context must not verify tokens; authorisation happens before the command exists",
		"app":       "a context must not depend on the application layer; that layer composes it",
	}

	for _, pkg := range internalPackages(t) {
		if layerOf(pkg.ImportPath) != "domain" {
			continue
		}
		for _, imported := range pkg.Imports {
			name := layerOf(imported)
			if reason, bad := forbidden[name]; bad {
				t.Errorf("%s imports %s.\n%s.",
					relative(t, pkg.Dir), importedDir(t, imported), reason)
			}
		}
	}
}
