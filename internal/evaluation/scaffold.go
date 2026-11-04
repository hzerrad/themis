package evaluation

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// template is a pack to fill in. {{name}} in a file stands for the pack's
// name, and {{name_}} for it with underscores, as check names have.
//
//go:embed all:template
var template embed.FS

// Scaffold creates root/name, a scenario pack for a contributor to fill in:
// a commented scenario.yaml, a checks.yaml with an example check, a README
// and two evaluation cases, one where acting is right and a look-alike. Each
// says what to fill in, and CheckPack fails on it until that is done.
func Scaffold(root, name string) error {
	if msgs := validation.IsDNS1123Label(name); len(msgs) > 0 {
		return fmt.Errorf("%q can't name a pack: %s", name, strings.Join(msgs, "; "))
	}
	dir := filepath.Join(root, name)
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s already exists", dir)
	}
	fill := strings.NewReplacer("{{name}}", name, "{{name_}}", strings.ReplaceAll(name, "-", "_"))
	return fs.WalkDir(template, "template", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(template, p)
		if err != nil {
			return err
		}
		// Case folders are named after the pack, since case names are
		// shared by every pack's evaluation.
		rel := strings.TrimPrefix(p, "template/")
		if c, ok := strings.CutPrefix(rel, "eval/"); ok {
			rel = path.Join("eval", name+"-"+c)
		}
		out := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil { //nolint:gosec // a folder in the repository
			return err
		}
		return os.WriteFile(out, []byte(fill.Replace(string(data))), 0o644) //nolint:gosec // a file in the repository
	})
}
