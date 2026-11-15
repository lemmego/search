package search_test

import (
	"os/exec"
	"strings"
	"testing"
)

// TestTheRootIsTheStandardLibraryOnly pins this module's dependency set,
// because the shape of v0.2.0 rests on it.
//
// The lesson is lemmego/log's: with its provider inside the module, a consumer
// importing only the core inherited 289 modules — cloud SDKs, OpenTelemetry,
// gRPC — for a package that formats text. Moving the provider out took it to 2.
// fsys measured the same thing from the other side: while its S3 and GCS
// drivers lived in the root, importing it was the difference between 58
// packages and 691, and between a 2.3MB binary and a 44MB one.
//
// This module is in the unusual position of being able to hold *every* engine
// and still import nothing: the Postgres engine needs database/sql and the
// Meilisearch engine needs net/http. That is why the Postgres engine stays in
// the root rather than moving to a subpackage, and it is only true as long as
// nobody reaches for an SDK. A Meilisearch client library would be the first
// thing to break it, and the fix is a subpackage — see search/engines — rather
// than a relaxation here.
//
// There is deliberately no denied list naming the SDKs. i18n can guard its
// list with `go list <denied>` because those packages exist in its module
// graph; here they do not, so a denied entry could be misspelled and forbid
// nothing, which is this project's most-repeated class of failing test. The
// stdlib-only rule already forbids every one of them.
func TestTheRootIsTheStandardLibraryOnly(t *testing.T) {
	assertStandardLibraryOnly(t, ".")
}

// TestTheWholeModuleIsTheStandardLibraryOnly is the claim above, made about
// every package a consumer can import rather than only the one they usually do.
//
// A consumer blank-importing search/engines to get the external engines must
// not thereby inherit an SDK, which is the entire reason that package exists
// as an aggregator of blank imports.
func TestTheWholeModuleIsTheStandardLibraryOnly(t *testing.T) {
	for _, pkg := range []string{
		".", "./engine/memory", "./enginetest", "./engines", "./engine/meili",
		// internal/syncer is shared by the three ORM adapters and must stay
		// on this side of the line: it holds the error policy and the
		// deferral seam, and an import of an ORM there would put all three
		// ORMs in every adapter's closure.
		"./internal/syncer",
	} {
		if !packageExists(pkg) {
			// Named here before it is written, so the guard covers each
			// package from its first commit rather than from whenever
			// somebody remembers to add it.
			t.Logf("%s does not exist yet", pkg)
			continue
		}
		assertStandardLibraryOnly(t, pkg)
	}
}

// TestTheProviderIsWhereTheApiDependencyLives asserts the split is real
// rather than intended, which is the other half of the guard above.
func TestTheProviderIsWhereTheApiDependencyLives(t *testing.T) {
	if !packageExists("./provider") {
		t.Skip("search/provider does not exist yet")
	}

	deps := dependencies(t, "./provider")
	if !strings.Contains(strings.Join(deps, "\n"), "github.com/lemmego/api/app") {
		t.Error("search/provider does not depend on api/app, so either the " +
			"provider is not wired to the container or this test is in the " +
			"wrong place")
	}

	// And the measurement that makes the split worth having.
	root := dependencies(t, ".")
	if len(root) >= len(deps) {
		t.Errorf("the root reaches %d packages and the provider %d; the whole "+
			"point of the split is that the root is the cheap one",
			len(root), len(deps))
	}
}

// TestEachOrmAdapterCarriesOnlyItsOwnOrm.
//
// The positive half, and the one that would be missed: three adapters in one
// module means three ORMs in the module graph, and the thing that keeps them
// out of a build is the package split plus graph pruning. A shared helper
// that imported one of them — or an adapter that imported a sibling for a
// type — would collapse that silently, because everything would still
// compile and the only symptom is a bigger binary.
func TestEachOrmAdapterCarriesOnlyItsOwnOrm(t *testing.T) {
	orms := map[string]string{
		"./syncorm":  "github.com/lemmego/orm",
		"./syncgorm": "gorm.io/gorm",
		"./syncbun":  "github.com/uptrace/bun",
	}

	for pkg, own := range orms {
		if !packageExists(pkg) {
			t.Logf("%s does not exist yet", pkg)
			continue
		}
		closure := strings.Join(dependencies(t, pkg), "\n")

		if !strings.Contains(closure, own) {
			t.Errorf("%s does not depend on %s, so either it is not wired to "+
				"that ORM or this test names the wrong one", pkg, own)
		}
		for other, path := range orms {
			if other == pkg {
				continue
			}
			if strings.Contains(closure, path) {
				t.Errorf("%s depends on %s, which belongs to %s. Three ORMs in "+
					"one module is only affordable because each adapter carries "+
					"exactly one of them.", pkg, path, other)
			}
		}
	}

	// And the root carries none of the three, which is what lets an
	// application use the query API with whatever it already has.
	root := strings.Join(dependencies(t, "."), "\n")
	for _, path := range orms {
		if strings.Contains(root, path) {
			t.Errorf("the root package depends on %s", path)
		}
	}
}

func assertStandardLibraryOnly(t *testing.T, pkg string) {
	t.Helper()

	for _, dep := range dependencies(t, pkg) {
		if dep == "github.com/lemmego/search" ||
			strings.HasPrefix(dep, "github.com/lemmego/search/") {
			continue
		}

		// A standard-library path has no dot in its first segment, which is
		// how the toolchain itself tells the two apart. Note that the check is
		// on the first segment and not on the whole path: the closure contains
		// crypto/internal/entropy/v1.0.0, which has a dot but is stdlib.
		first, _, _ := strings.Cut(dep, "/")
		if strings.Contains(first, ".") {
			t.Errorf("%s depends on %s.\n"+
				"Only the standard library belongs in this module. An engine "+
				"that needs a client library belongs in its own package under "+
				"search/engine/, registered from an init and reached through "+
				"search/engines — the way database/sql drivers and "+
				"cache/store/* are. If it is api/app, it belongs in "+
				"search/provider.", pkg, dep)
		}
	}
}

func dependencies(t *testing.T, pkg string) []string {
	t.Helper()

	out, err := exec.Command("go", "list", "-deps", pkg).Output()
	if err != nil {
		t.Fatalf("listing the dependencies of %s: %v", pkg, err)
	}
	return strings.Fields(string(out))
}

func packageExists(pkg string) bool {
	return exec.Command("go", "list", pkg).Run() == nil
}
