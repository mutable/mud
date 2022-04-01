package main

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	slashpath "path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/template"

	"github.com/mutable/archive"
	"github.com/mutable/base32"
	"github.com/mutable/tempfile"
	"go.uber.org/multierr"
	"golang.org/x/tools/go/packages"
)

var tmpl = template.Must(template.New("external").Parse(`
# generator //tools/mud (DO NOT EDIT)
{ platform, pkgs, ... }:

platform.buildGo.external rec {
  path = "{{.Path}}";
  src = platform.lib.fetchGoModule {
    inherit path;
    version = "{{.Version}}";
    sha256 = "{{.ModSHA256}}";
  };
{{- with .Imports}}
  deps = with platform.third_party; [
{{- range .}}
    gopkgs.{{.NixAttr}}
{{- end}}
  ];
{{- end}}
}
`[1:]))

func main() {
	if len(os.Args) > 1 {
		fmt.Fprintln(os.Stderr, "mud takes no arguments")
		os.Exit(1)
	}

	if _, err := os.Stat(".git"); os.IsNotExist(err) {
		fmt.Fprintln(os.Stderr, "mud must be run from the repository root")
		os.Exit(1)
	}

	roots := []string{"./..."}
	{
		pkgs, err := packages.Load(&packages.Config{
			Mode: 0 |
				packages.NeedName |
				packages.NeedImports,
			BuildFlags: []string{"-tags", "tools"},
		}, "./tools")
		if err != nil {
			panic(err)
		}
		for _, pkg := range pkgs {
			for dep := range pkg.Imports {
				roots = append(roots, dep)
			}
		}
	}

	pkgs, err := packages.Load(&packages.Config{
		Mode: 0 |
			packages.NeedName |
			packages.NeedDeps |
			packages.NeedImports |
			packages.NeedModule,
		Tests: true,
	}, roots...)

	if err != nil {
		panic(err)
	}

	// for each module, figure out what dependencies it has
	// NOTE: these aren't necessarily *complete* dependencies,
	// since we are just walking the packages we're transitively using,
	// rather than $MODULE/...

	modules := make(map[Path]*Module)
	packages.Visit(pkgs,
		func(pkg *packages.Package) bool {
			return !isBuiltin(pkg)
		},
		func(pkg *packages.Package) {
			if isBuiltin(pkg) {
				return
			}

			if err := pkgErrors(pkg); err != nil {
				panic(err)
			}

			if pkg.Module == nil {
				if pkg.Name == "main" && strings.HasSuffix(pkg.PkgPath, ".test") {
					return // test packages show up twice, once without pkg.Module set
				}
				if strings.HasSuffix(pkg.PkgPath, "_test") {
					// _test packages don't have pkg.Module set
					// their main package ends in .test instead
					return
				}
				panic(fmt.Errorf("package without a module: %s", pkg.PkgPath))
			}

			mod := modules[Path(pkg.Module.Path)]
			if mod == nil {
				mod = &Module{
					Path:    Path(pkg.Module.Path),
					Version: pkg.Module.Version,
					Dir:     pkg.Module.Dir,
					Deps:    make(map[*Module]PackageSet),
				}

				if pkg.Module.Replace != nil {
					mod.ReplacePath = pkg.Module.Replace.Path
					mod.Version = pkg.Module.Replace.Version
				}

				mod.Version = strings.TrimPrefix(mod.Version, "v")
				modules[mod.Path] = mod
			}

			for _, dep := range pkg.Imports {
				if isBuiltin(dep) {
					continue
				}

				depMod := modules[Path(dep.Module.Path)]
				if depMod.Path == mod.Path {
					continue // ignore intra-module dependencies
				}

				depSet := mod.Dep(depMod)
				depSet.Add(Path(dep.PkgPath))
			}
		},
	)

	var paths []Path
	for path := range modules {
		paths = append(paths, path)
	}
	sortPaths(paths)

	var buffer bytes.Buffer
	for _, path := range paths {
		mod := modules[path]

		if !mod.IsExternal() {
			continue
		}

		buffer.Reset()
		outDir := slashpath.Join("third_party/gopkgs", string(mod.Path))
		if mod.ReplacePath != "" && mod.ReplacePath[0] == '.' {
			if mod.ReplacePath != "./"+outDir {
				fmt.Fprintf(os.Stderr, "replace points at //%v, expected it to point at //%v\n", mod.ReplacePath, outDir)
				os.Exit(1)
			}

			// vendored packages don't use buildGo.external,
			// so we don't generate a manifest for them.
			// they are expected to have their own buildGo expressions,
			// like any other in-tree code.
			continue
		}

		if err := tmpl.Execute(&buffer, mod); err != nil {
			panic(err)
		}

		if err := writeFile(outDir, "default.nix", buffer.Bytes()); err != nil {
			panic(err)
		}
	}
}

func writeFile(dir, name string, data []byte) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	f, err := tempfile.Open(dir, name+".tmp", 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := f.Write(data); err != nil {
		return err
	}

	// TODO(edef): this ought to use unix.Unlink,
	// but that's a bit more caution and effort than a non-library function warrants
	if err := os.Remove(f.Name()); err != nil && !os.IsNotExist(err) {
		return err
	}

	if err := tempfile.Commit(f); err != nil {
		return err
	}

	return os.Rename(f.Name(), filepath.Join(dir, name))
}

func pkgErrors(pkg *packages.Package) error {
	var errs error
	for _, err := range pkg.Errors {
		multierr.AppendInto(&errs, err)
	}
	if pkg.Module != nil && pkg.Module.Error != nil {
		multierr.AppendInto(&errs, errors.New(pkg.Module.Error.Err))
	}
	return errs
}

func isBuiltin(pkg *packages.Package) bool {
	importPath := pkg.PkgPath
	i := strings.IndexByte(importPath, '/')
	if i != -1 {
		importPath = importPath[:i]
	}
	return strings.IndexByte(importPath, '.') == -1
}

var nixIdentRe = regexp.MustCompile(`^[a-zA-Z\_][a-zA-Z0-9\_\'\-]*$`)
var nixKeyword = map[string]bool{
	"if":      true,
	"then":    true,
	"else":    true,
	"assert":  true,
	"with":    true,
	"let":     true,
	"in":      true,
	"rec":     true,
	"inherit": true,
	"or":      true,
}

type Path string

func (p Path) NixAttr() string {
	names := strings.Split(string(p), "/")
	for i, name := range names {
		if !nixIdentRe.MatchString(name) || nixKeyword[name] {
			names[i] = fmt.Sprintf("%q", name)
		}
	}
	return strings.Join(names, ".")
}

type Module struct {
	Path    Path
	Version string
	Dir     string
	// If not empty, the path that this module's source is located at in the repo
	ReplacePath string
	// Deps maps modules we depend on to the exact packages
	// from that module we depend on
	Deps map[*Module]PackageSet
}

func (m *Module) Imports() []Path {
	var imports []Path
	for _, dep := range m.Deps {
		for pkg := range dep {
			imports = append(imports, pkg)
		}
	}
	sortPaths(imports)
	return imports
}

func (m *Module) Dep(d *Module) PackageSet {
	pkgs := m.Deps[d]
	if pkgs == nil {
		pkgs = make(PackageSet)
		m.Deps[d] = pkgs
	}
	return pkgs
}

func (m *Module) ModSHA256() string {
	if m.Dir == "" {
		panic(fmt.Errorf("module without a dir: %s", m.Path))
	}

	h := sha256.New()
	if err := archive.CopyPath(archive.WriteDump(h), m.Dir); err != nil {
		panic(err)
	}

	return base32.Encode(h.Sum(nil))
}

func (m *Module) IsExternal() bool {
	return !strings.HasPrefix(string(m.Path), "example.com/")
}

type PackageSet map[Path]struct{}

func (s PackageSet) Add(p Path) {
	s[p] = struct{}{}
}

func sortPaths(xs []Path) {
	sort.Slice(xs, func(i, j int) bool { return xs[i] < xs[j] })
}
