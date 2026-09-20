package i18n_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/NextProbe/aiprobe/internal/i18n"
)

// A missing id is not a compile error: the printer falls back to returning the
// id, so a typo ships as body text like "caveat.overlap" in the report. This
// walks the tree and checks every literal id the product looks up, in every
// locale. Ids built at runtime (p.S("category." + string(c))) cannot be checked
// here; the enum tables in i18n_test.go cover those.
func TestLiteralMessageIDsExist(t *testing.T) {
	// p.S("id"), p.T("id", ...), i18n.T(loc, "id", ...), {{T "id"}},
	// {{Targs "id" ...}}.
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`\.S\("([a-z][a-z0-9_.\-]*)"\)`),
		regexp.MustCompile(`\.T\("([a-z][a-z0-9_.\-]*)"`),
		regexp.MustCompile(`\b(?:T|Targs)\s+"([a-z][a-z0-9_.\-]*)"`),
	}

	root := filepath.Join("..", "..")
	found := map[string][]string{} // id -> files

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "bin", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		ext := filepath.Ext(path)
		if ext != ".go" && ext != ".gohtml" {
			return nil
		}
		// The catalogs define ids rather than consume them.
		if strings.HasPrefix(filepath.Base(path), "catalog_") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, re := range patterns {
			for _, m := range re.FindAllStringSubmatch(string(src), -1) {
				id := m[1]
				// A bare word is a method call on something else, not an id.
				if !strings.Contains(id, ".") {
					continue
				}
				found[id] = append(found[id], path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(found) == 0 {
		t.Fatal("no message ids found anywhere; the extraction patterns are wrong")
	}

	ids := make([]string, 0, len(found))
	for id := range found {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, loc := range i18n.Locales() {
		for _, id := range ids {
			if !i18n.Has(loc, id) {
				t.Errorf("locale %s: id %q is looked up in %v but is not in the catalog",
					loc, id, found[id])
			}
		}
	}
}
