package main

import (
	"fmt"
	"go/build"
	"go/parser"
	"go/token"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	pathpkg "path"
)

var (
	buildContext = build.Default
	gorootSrc    = filepath.Join(buildContext.GOROOT, "src")
)

// packageContext is used to track an import and which package imported it.
type packageContext struct {
	pkg *build.Package // package that imports the import
	imp string         // import
}

// depScanner tracks the processed and to be processed packageContexts
type depScanner struct {
	processed []packageContext
	todo      []packageContext
}

// Next package and import to process
func (ds *depScanner) Next() (*build.Package, string) {
	c := ds.todo[0]
	ds.processed = append(ds.processed, c)
	ds.todo = ds.todo[1:]
	return c.pkg, c.imp
}

// Continue looping?
func (ds *depScanner) Continue() bool {
	if len(ds.todo) > 0 {
		return true
	}
	return false
}

// Add a package and imports to the depScanner. Skips already processed package/import combos
func (ds *depScanner) Add(pkg *build.Package, imports ...string) {
NextImport:
	for _, i := range imports {
		if i == "C" {
			i = "runtime/cgo"
		}
		pc := packageContext{pkg, i}
		for _, epc := range ds.processed {
			if epc == pc {
				debugln("ctxts epc == pc, skipping", epc, pc)
				continue NextImport
			}
			if pc.pkg.Dir == epc.pkg.Dir && pc.imp == epc.imp {
				debugln("ctxts epc.pkg.Dir == pc.pkg.Dir && pc.imp == epc.imp, skipping", epc.pkg.Dir, pc.imp)
				continue NextImport
			}
		}
		for _, epc := range ds.todo {
			if epc == pc {
				debugln("ctxts epc == pc, skipping", epc, pc)
				continue NextImport
			}
		}
		debugln("Adding pc:", pc)
		ds.todo = append(ds.todo, pc)
	}
}

func checkGoroot(p *build.Package, err error) (*build.Package, error) {
	if p.Goroot && err != nil {
		buildContext.UseAllFiles = false
		p, err = buildContext.Import(p.ImportPath, p.Dir, 0)
		buildContext.UseAllFiles = true
	}
	return p, err
}

// listPackage specified by path
func listPackage(path string) (*Package, error) {
	var dir string
	var lp *build.Package
	var err error
	if build.IsLocalImport(path) {
		dir = path
		if !filepath.IsAbs(dir) {
			if abs, err := filepath.Abs(dir); err == nil {
				// interpret relative to current directory
				dir = abs
			}
		}
		lp, err = buildContext.ImportDir(dir, build.FindOnly)
	} else {
		dir, err = os.Getwd()
		if err != nil {
			return nil, err
		}
		lp, err = buildContext.Import(path, dir, build.FindOnly)
		if lp.Goroot {
			// If it's in the GOROOT, just import it
			lp, err = buildContext.Import(path, dir, 0)
		}
	}
	fillPackage(lp)
	p := &Package{
		Dir:            lp.Dir,
		Root:           lp.Root,
		ImportPath:     lp.ImportPath,
		XTestImports:   lp.XTestImports,
		TestImports:    lp.TestImports,
		GoFiles:        lp.GoFiles,
		CgoFiles:       lp.CgoFiles,
		TestGoFiles:    lp.TestGoFiles,
		XTestGoFiles:   lp.XTestGoFiles,
		IgnoredGoFiles: lp.IgnoredGoFiles,
	}
	p.Standard = lp.Goroot && lp.ImportPath != "" && !strings.Contains(lp.ImportPath, ".")
	if err != nil {
		return p, err
	}
	debugln("Looking For Package:", path, "in", dir)
	ppln(lp)

	ds := depScanner{}
	ds.Add(lp, lp.Imports...)
	for ds.Continue() {
		ip, i := ds.Next()

		debugf("Processing import %s for %s\n", i, ip.Dir)
		// We need to check to see if the import exists in vendor/ folders up the hierachy of the importing package,
		var dp *build.Package
		if VendorExperiment && !ip.Goroot {
			for base := ip.Dir; base != ip.Root; base = filepath.Dir(base) {
				dir := filepath.Join(base, "vendor", i)
				debugln("dir:", dir)
				dp, err = buildContext.ImportDir(dir, build.FindOnly)
				if err != nil {
					if os.IsNotExist(err) {
						continue
					}
					debugln(err.Error())
					ppln(err)
					panic("Unknown error attempt to find vendor/")
				}
				goto Found
			}
		}
		// Wasn't found above, so resolve it using the build.Context
		dp, err = buildContext.Import(i, ip.Dir, build.FindOnly)
		if dp.Goroot {
			// If it's in the Goroot, just import the package
			dp, err = buildContext.Import(i, ip.Dir, 0)
		}
		if err != nil {
			debugln("Warning: Error importing dependent package")
			ppln(err)
		}
	Found:
		fillPackage(dp)
		ppln(dp)
		if dp.Goroot {
			// Treat packages discovered to be in the GOROOT as if the package we're looking for is importing them
			ds.Add(lp, dp.Imports...)
		} else {
			ds.Add(dp, dp.Imports...)
		}
		debugln("lp:", lp)
		debugln("ip:", ip)
		if lp == ip {
			debugln("lp == ip")
			p.Imports = append(p.Imports, dp.ImportPath)
		}
		p.Deps = append(p.Deps, dp.ImportPath)
	}
	p.Imports = uniq(p.Imports)
	p.Deps = uniq(p.Deps)
	debugln("Looking For Package:", path, "in", dir)
	ppln(p)
	return p, nil
}

// fillPackage full of info. Assumes a build.Package discovered in build.FindOnly mode
func fillPackage(p *build.Package) error {

	if p.Goroot {
		return nil
	}

	var buildMatch = "+build "
	var buildFieldSplit = func(r rune) bool {
		return unicode.IsSpace(r) || r == ','
	}

	debugln("Filling package:", p.ImportPath, "from", p.Dir)
	gofiles, err := filepath.Glob(filepath.Join(p.Dir, "*.go"))
	if err != nil {
		debugln("Error globbing", err)
		return err
	}

	var testImports []string
	var imports []string
NextFile:
	for _, file := range gofiles {
		debugln(file)
		fset := token.NewFileSet()
		pf, err := parser.ParseFile(fset, file, nil, parser.ParseComments)
		if err != nil {
			return err
		}
		testFile := strings.HasSuffix(file, "_test.go")
		fname := filepath.Base(file)
		if testFile {
			p.TestGoFiles = append(p.TestGoFiles, fname)
		} else {
			p.GoFiles = append(p.GoFiles, fname)
		}
		if len(pf.Comments) > 0 {
			for _, c := range pf.Comments {
				ct := c.Text()
				if i := strings.Index(ct, buildMatch); i != -1 {
					for _, b := range strings.FieldsFunc(ct[i+len(buildMatch):], buildFieldSplit) {
						//TODO: appengine is a special case for now: https://github.com/tools/godep/issues/353
						if b == "ignore" || b == "appengine" {
							p.IgnoredGoFiles = append(p.IgnoredGoFiles, fname)
							continue NextFile
						}
					}
				}
			}
		}
		for _, is := range pf.Imports {
			name, err := strconv.Unquote(is.Path.Value)
			if err != nil {
				return err // can't happen?
			}
			if testFile {
				testImports = append(testImports, name)
			} else {
				imports = append(imports, name)
			}
		}
	}
	imports = uniq(imports)
	testImports = uniq(testImports)
	p.Imports = imports
	p.TestImports = testImports
	return nil
}

// All of the following functions were vendored from go proper. Locations are noted in comments, but may change in future Go versions.

// importPaths returns the import paths to use for the given command line.
// $GOROOT/src/cmd/main.go:366
func importPaths(args []string) []string {
	args = importPathsNoDotExpansion(args)
	var out []string
	for _, a := range args {
		if strings.Contains(a, "...") {
			if build.IsLocalImport(a) {
				out = append(out, allPackagesInFS(a)...)
			} else {
				out = append(out, allPackages(a)...)
			}
			continue
		}
		out = append(out, a)
	}
	return out
}

// importPathsNoDotExpansion returns the import paths to use for the given
// command line, but it does no ... expansion.
// $GOROOT/src/cmd/main.go:332
func importPathsNoDotExpansion(args []string) []string {
	if len(args) == 0 {
		return []string{"."}
	}
	var out []string
	for _, a := range args {
		// Arguments are supposed to be import paths, but
		// as a courtesy to Windows developers, rewrite \ to /
		// in command-line arguments.  Handles .\... and so on.
		if filepath.Separator == '\\' {
			a = strings.Replace(a, `\`, `/`, -1)
		}

		// Put argument in canonical form, but preserve leading ./.
		if strings.HasPrefix(a, "./") {
			a = "./" + pathpkg.Clean(a)
			if a == "./." {
				a = "."
			}
		} else {
			a = pathpkg.Clean(a)
		}
		if a == "all" || a == "std" || a == "cmd" {
			out = append(out, allPackages(a)...)
			continue
		}
		out = append(out, a)
	}
	return out
}

// allPackagesInFS is like allPackages but is passed a pattern
// beginning ./ or ../, meaning it should scan the tree rooted
// at the given directory.  There are ... in the pattern too.
// $GOROOT/src/cmd/main.go:620
func allPackagesInFS(pattern string) []string {
	pkgs := matchPackagesInFS(pattern)
	if len(pkgs) == 0 {
		fmt.Fprintf(os.Stderr, "warning: %q matched no packages\n", pattern)
	}
	return pkgs
}

// allPackages returns all the packages that can be found
// under the $GOPATH directories and $GOROOT matching pattern.
// The pattern is either "all" (all packages), "std" (standard packages),
// "cmd" (standard commands), or a path including "...".
// $GOROOT/src/cmd/main.go:542
func allPackages(pattern string) []string {
	pkgs := matchPackages(pattern)
	if len(pkgs) == 0 {
		fmt.Fprintf(os.Stderr, "warning: %q matched no packages\n", pattern)
	}
	return pkgs
}

// $GOROOT/src/cmd/main.go:554
func matchPackages(pattern string) []string {
	match := func(string) bool { return true }
	treeCanMatch := func(string) bool { return true }
	if pattern != "all" && pattern != "std" && pattern != "cmd" {
		match = matchPattern(pattern)
		treeCanMatch = treeCanMatchPattern(pattern)
	}

	have := map[string]bool{
		"builtin": true, // ignore pseudo-package that exists only for documentation
	}
	if !buildContext.CgoEnabled {
		have["runtime/cgo"] = true // ignore during walk
	}
	var pkgs []string

	for _, src := range buildContext.SrcDirs() {
		if (pattern == "std" || pattern == "cmd") && src != gorootSrc {
			continue
		}
		src = filepath.Clean(src) + string(filepath.Separator)
		root := src
		if pattern == "cmd" {
			root += "cmd" + string(filepath.Separator)
		}
		filepath.Walk(root, func(path string, fi os.FileInfo, err error) error {
			if err != nil || !fi.IsDir() || path == src {
				return nil
			}

			// Avoid .foo, _foo, and testdata directory trees.
			_, elem := filepath.Split(path)
			if strings.HasPrefix(elem, ".") || strings.HasPrefix(elem, "_") || elem == "testdata" {
				return filepath.SkipDir
			}

			name := filepath.ToSlash(path[len(src):])
			if pattern == "std" && (strings.Contains(name, ".") || name == "cmd") {
				// The name "std" is only the standard library.
				// If the name has a dot, assume it's a domain name for go get,
				// and if the name is cmd, it's the root of the command tree.
				return filepath.SkipDir
			}
			if !treeCanMatch(name) {
				return filepath.SkipDir
			}
			if have[name] {
				return nil
			}
			have[name] = true
			if !match(name) {
				return nil
			}
			_, err = buildContext.ImportDir(path, 0)
			if err != nil {
				if _, noGo := err.(*build.NoGoError); noGo {
					return nil
				}
			}
			pkgs = append(pkgs, name)
			return nil
		})
	}
	return pkgs
}

// treeCanMatchPattern(pattern)(name) reports whether
// name or children of name can possibly match pattern.
// Pattern is the same limited glob accepted by matchPattern.
// $GOROOT/src/cmd/main.go:527
func treeCanMatchPattern(pattern string) func(name string) bool {
	wildCard := false
	if i := strings.Index(pattern, "..."); i >= 0 {
		wildCard = true
		pattern = pattern[:i]
	}
	return func(name string) bool {
		return len(name) <= len(pattern) && hasPathPrefix(pattern, name) ||
			wildCard && strings.HasPrefix(name, pattern)
	}
}

// hasPathPrefix reports whether the path s begins with the
// elements in prefix.
// $GOROOT/src/cmd/main.go:489
func hasPathPrefix(s, prefix string) bool {
	switch {
	default:
		return false
	case len(s) == len(prefix):
		return s == prefix
	case len(s) > len(prefix):
		if prefix != "" && prefix[len(prefix)-1] == '/' {
			return strings.HasPrefix(s, prefix)
		}
		return s[len(prefix)] == '/' && s[:len(prefix)] == prefix
	}
}

// $GOROOT/src/cmd/go/main.go:631
func matchPackagesInFS(pattern string) []string {
	// Find directory to begin the scan.
	// Could be smarter but this one optimization
	// is enough for now, since ... is usually at the
	// end of a path.
	i := strings.Index(pattern, "...")
	dir, _ := pathpkg.Split(pattern[:i])

	// pattern begins with ./ or ../.
	// path.Clean will discard the ./ but not the ../.
	// We need to preserve the ./ for pattern matching
	// and in the returned import paths.
	prefix := ""
	if strings.HasPrefix(pattern, "./") {
		prefix = "./"
	}
	match := matchPattern(pattern)

	var pkgs []string
	filepath.Walk(dir, func(path string, fi os.FileInfo, err error) error {
		if err != nil || !fi.IsDir() {
			return nil
		}
		if path == dir {
			// filepath.Walk starts at dir and recurses. For the recursive case,
			// the path is the result of filepath.Join, which calls filepath.Clean.
			// The initial case is not Cleaned, though, so we do this explicitly.
			//
			// This converts a path like "./io/" to "io". Without this step, running
			// "cd $GOROOT/src; go list ./io/..." would incorrectly skip the io
			// package, because prepending the prefix "./" to the unclean path would
			// result in "././io", and match("././io") returns false.
			path = filepath.Clean(path)
		}

		// Avoid .foo, _foo, and testdata directory trees, but do not avoid "." or "..".
		_, elem := filepath.Split(path)
		dot := strings.HasPrefix(elem, ".") && elem != "." && elem != ".."
		if dot || strings.HasPrefix(elem, "_") || elem == "testdata" {
			return filepath.SkipDir
		}

		name := prefix + filepath.ToSlash(path)
		if !match(name) {
			return nil
		}
		if _, err = build.ImportDir(path, 0); err != nil {
			if _, noGo := err.(*build.NoGoError); !noGo {
				log.Print(err)
			}
			return nil
		}
		pkgs = append(pkgs, name)
		return nil
	})
	return pkgs
}
