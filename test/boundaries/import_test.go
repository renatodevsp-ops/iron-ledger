package boundaries

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const modulePath = "github.com/ironledger/ironledger"

// forbiddenImports is the import set Constitution Principle IV bans from the
// domain and the use case layer. fx is the composition framework, net/http is
// the transport, the database and queue SDKs are the infrastructure.
var forbiddenImports = []string{
	"go.uber.org/fx",
	"go.uber.org/dig",
	"net/http",
	"net/http/httptest",
	"github.com/jackc/pgx",
	"github.com/aws/aws-sdk-go-v2",
	"database/sql",
	"gorm.io",
	"github.com/golang-jwt",
	"github.com/MicahParks/keyfunc",
	"github.com/testcontainers",
}

type violation struct {
	pkg      string
	file     string
	imported string
}

// TestDomainAndUsecaseImportsArePure parses the import graph of
// internal/domain and internal/usecase and fails if it reaches any
// infrastructure package (research.md D-12). The graph is walked from source
// rather than grepped, so an indirect import through an internal package is
// caught exactly as a direct one is.
func TestDomainAndUsecaseImportsArePure(t *testing.T) {
	root := repoRoot(t)
	targets := []string{
		filepath.Join(root, "internal", "domain"),
		filepath.Join(root, "internal", "usecase"),
	}

	var found []violation
	for _, dir := range targets {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			continue
		}
		for _, v := range walkImports(t, root, dir) {
			found = append(found, v)
		}
	}

	for _, v := range found {
		t.Errorf("constitution Principle IV violation: %s imports %q via %s", v.pkg, v.imported, v.file)
	}
	assert.Empty(t, found, "the domain and use case layers must import only the standard library and each other")
}

// walkImports parses every non-test Go file under dir, including files in
// subdirectories, and reports each forbidden import it finds. It walks the whole
// subtree rather than only the top level: a package that grows a subdirectory
// would otherwise escape the guard entirely, which is precisely how a layering
// rule quietly stops being enforced.
func walkImports(t *testing.T, root, dir string) []violation {
	t.Helper()

	var violations []violation
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skip testdata and hidden directories: they hold fixtures, not code
			// that participates in the build graph.
			if path != dir && (d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".")) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}

		fset := token.NewFileSet()
		file, parseErr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		require.NoErrorf(t, parseErr, "parse %s", path)
		rel, _ := filepath.Rel(root, path)
		for _, spec := range file.Imports {
			imported, unquoteErr := strconv.Unquote(spec.Path.Value)
			require.NoError(t, unquoteErr)
			if isForbidden(imported) {
				violations = append(violations, violation{pkg: rel, file: rel, imported: imported})
			}
		}
		return nil
	})
	require.NoErrorf(t, err, "walk %s", dir)
	return violations
}

// eachImport calls fn for every non-test Go file under dir, relative to root.
// It is the single traversal used by all three boundary tests, so none of them
// can quietly cover less ground than the others.
func eachImport(t *testing.T, root, dir string, fn func(rel, imported string)) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != dir && (d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".")) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, parseErr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		require.NoErrorf(t, parseErr, "parse %s", path)
		rel, _ := filepath.Rel(root, path)
		for _, spec := range file.Imports {
			imported, unquoteErr := strconv.Unquote(spec.Path.Value)
			require.NoError(t, unquoteErr)
			fn(rel, imported)
		}
		return nil
	})
	require.NoErrorf(t, err, "walk %s", dir)
}

func isForbidden(imported string) bool {
	for _, banned := range forbiddenImports {
		if imported == banned || strings.HasPrefix(imported, banned+"/") {
			return true
		}
	}
	return false
}

// TestDomainUsesOnlyStdlib is the stricter form of the same rule: the domain
// must not depend on any third-party module at all, and it may only reference
// the use case layer through nothing, since the dependency runs the other way.
func TestDomainUsesOnlyStdlib(t *testing.T) {
	root := repoRoot(t)
	domainDir := filepath.Join(root, "internal", "domain")
	require.DirExists(t, domainDir)

	eachImport(t, root, domainDir, func(rel, imported string) {
		if strings.HasPrefix(imported, modulePath) {
			t.Errorf("%s imports %q; the domain must depend on nothing but the standard library", rel, imported)
		}
	})
}

// TestUsecaseOnlyDependsOnDomain pins the layering direction: the use case
// layer may import the domain and the standard library, and nothing else
// inside the module.
func TestUsecaseOnlyDependsOnDomain(t *testing.T) {
	root := repoRoot(t)
	usecaseDir := filepath.Join(root, "internal", "usecase")
	require.DirExists(t, usecaseDir)

	eachImport(t, root, usecaseDir, func(rel, imported string) {
		if !strings.HasPrefix(imported, modulePath) {
			return
		}
		assert.Equalf(t, modulePath+"/internal/domain", imported,
			"%s must depend on the domain only", rel)
	})
}

// TestNoFloatTypedMoneyFields scans the whole source tree for a float-typed
// struct field whose name looks monetary. It is a coarse net behind the exact
// per-type assertions in internal/domain/money_test.go.
func TestNoFloatTypedMoneyFields(t *testing.T) {
	root := repoRoot(t)
	moneyish := []string{"amount", "balance", "price", "stake", "prize", "fee", "total", "value"}
	offenders := 0

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && (d.Name() == ".git" || d.Name() == "node_modules" || d.Name() == ".opencode") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		text := string(src)
		for _, name := range moneyish {
			for _, kind := range []string{"float32", "float64"} {
				if containsMoneyField(text, name, kind) {
					rel, _ := filepath.Rel(root, path)
					t.Errorf("%s declares a %s field named %q: money MUST be int64 minor units", rel, kind, name)
					offenders++
				}
			}
		}
		return nil
	})
	require.NoError(t, err)
	assert.Zero(t, offenders, "no float may appear on a money field")
}

// containsMoneyField reports whether a struct field declaration ties a float
// kind to a monetary field name, e.g. "Amount float64 `json:\"amount\"`".
func containsMoneyField(text, name, kind string) bool {
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, name+" ") {
			continue
		}
		if strings.Contains(trimmed, "`"+kind+"`") {
			return true
		}
	}
	return false
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	require.NoError(t, err)
	for dir := wd; dir != "/"; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
	}
	t.Fatal("could not locate the module root (go.mod)")
	return ""
}
