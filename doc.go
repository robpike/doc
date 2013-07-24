// Copyright 2013 The rspace Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Doc is a simple document printer that produces the doc comments for its
// argument symbols, plus a link to the full documentation and a pointer to
// the source. It has a more Go-like UI than godoc. It can also search for
// symbols by looking in all packages, and case is ignored. For instance:
//	doc isupper
// will find unicode.IsUpper.
//
// The -pkg flag retrieves package-level doc comments only.
//
// Usage:
//	doc pkg.name   # "doc io.Writer"
//	doc pkg name   # "doc fmt Printf"
//	doc name       # "doc isupper" (finds unicode.IsUpper)
//	doc -pkg pkg   # "doc fmt"
//
// The pkg is the last element of the package path;
// no slashes (ast.Node not go/ast.Node).
//
// Flags
//	-c(onst) -f(unc) -i(nterface) -m(ethod) -s(truct) -t(ype) -v(ar)
// restrict hits to declarations of the corresponding kind.
// Flags
//	-doc -src -url
// restrict printing to the documentation, source path, or godoc URL.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"unicode"
	"unicode/utf8"

	"code.google.com/p/go.tools/go/types"
)

const usageDoc = `Find documentation for names.
usage:
	doc pkg.name   # "doc io.Writer"
	doc pkg name   # "doc fmt Printf"
	doc name       # "doc isupper" finds unicode.IsUpper
	doc -pkg pkg   # "doc fmt"
pkg is the last component of any package, e.g. fmt, parser
name is the name of an exported symbol; case is ignored in matches.
Flags
	-c(onst) -f(unc) -i(nterface) -m(ethod) -s(truct) -t(ype) -v(ar)
restrict hits to declarations of the corresponding kind.
Flags
	-doc -src -url
restrict printing to the documentation, source path, or godoc URL.
`

func usage() {
	fmt.Fprintf(os.Stderr, usageDoc)
	os.Exit(2)
}

var (
	// If none is set, all are set.
	constantFlag  = flag.Bool("const", false, "show doc for consts only")
	functionFlag  = flag.Bool("func", false, "show doc for funcs only")
	interfaceFlag = flag.Bool("interface", false, "show doc for interfaces only")
	methodFlag    = flag.Bool("method", false, "show doc for methods only")
	packageFlag   = flag.Bool("package", false, "show top-level package doc only")
	structFlag    = flag.Bool("struct", false, "show doc for structs only")
	typeFlag      = flag.Bool("type", false, "show doc for types only")
	variableFlag  = flag.Bool("var", false, "show  doc for vars only")
)

var (
	// If none is set, all are set.
	docFlag = flag.Bool("doc", false, "restrict output to documentation only")
	srcFlag = flag.Bool("src", false, "restrict output to source file only")
	urlFlag = flag.Bool("url", false, "restrict output to godoc URL only")
)

func init() {
	flag.BoolVar(constantFlag, "c", false, "alias for -const")
	flag.BoolVar(functionFlag, "f", false, "alias for -func")
	flag.BoolVar(interfaceFlag, "i", false, "alias for -interface")
	flag.BoolVar(methodFlag, "m", false, "alias for -method")
	flag.BoolVar(packageFlag, "pkg", false, "alias for -package")
	flag.BoolVar(structFlag, "s", false, "alias for -struct")
	flag.BoolVar(typeFlag, "t", false, "alias for -type")
	flag.BoolVar(variableFlag, "v", false, "alias for -var")
}

func main() {
	flag.Parse()
	if !(*constantFlag || *functionFlag || *interfaceFlag || *methodFlag || *packageFlag || *structFlag || *typeFlag || *variableFlag) { // none set
		*constantFlag = true
		*functionFlag = true
		*methodFlag = true
		// Not package! It's special.
		*typeFlag = true
		*variableFlag = true
	}
	if !(*docFlag || *srcFlag || *urlFlag) {
		*docFlag = true
		*srcFlag = true
		*urlFlag = true
	}
	var pkg, name string
	switch flag.NArg() {
	case 1:
		if *packageFlag {
			pkg = flag.Arg(0)
		} else if strings.Contains(flag.Arg(0), ".") {
			pkg, name = split(flag.Arg(0))
		} else {
			name = flag.Arg(0)
		}
	case 2:
		if *packageFlag {
			usage()
		}
		pkg, name = flag.Arg(0), flag.Arg(1)
	default:
		usage()
	}
	for _, path := range paths(pkg) {
		lookInDirectory(path, name)
	}
}

var slash = string(filepath.Separator)
var slashDot = string(filepath.Separator) + "."
var goRootSrcPkg = filepath.Join(runtime.GOROOT(), "src", "pkg")
var goRootSrcCmd = filepath.Join(runtime.GOROOT(), "src", "cmd")
var goPaths = splitGopath()

func split(arg string) (pkg, name string) {
	str := strings.Split(arg, ".")
	if len(str) != 2 {
		usage()
	}
	return str[0], str[1]
}

func paths(pkg string) []string {
	pkgs := pathsFor(runtime.GOROOT(), pkg)
	for _, root := range goPaths {
		pkgs = append(pkgs, pathsFor(root, pkg)...)
	}
	return pkgs
}

func splitGopath() []string {
	gopath := os.Getenv("GOPATH")
	if gopath == "" {
		return nil
	}
	return strings.Split(gopath, string(os.PathListSeparator))
}

// pathsFor recursively walks the tree looking for possible directories for the package:
// those whose basename is pkg.
func pathsFor(root, pkg string) []string {
	root = path.Join(root, "src")
	pkgPaths := make([]string, 0, 10)
	visit := func(pathName string, f os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		// One package per directory. Ignore the files themselves.
		if !f.IsDir() {
			return nil
		}
		// No .hg or other dot nonsense please.
		if strings.Contains(pathName, slashDot) {
			return filepath.SkipDir
		}
		// Is the last element of the path correct
		if pkg == "" || filepath.Base(pathName) == pkg {
			pkgPaths = append(pkgPaths, pathName)
		}
		return nil
	}

	filepath.Walk(root, visit)
	return pkgPaths
}

// lookInDirectory looks in the package (if any) in the directory for the named exported identifier.
func lookInDirectory(directory, name string) {
	fset := token.NewFileSet()
	pkgs, _ := parser.ParseDir(fset, directory, nil, parser.ParseComments) // Ignore the error.
	for _, pkg := range pkgs {
		doPackage(pkg, fset, name)
	}
}

// prefixDirectory places the directory name on the beginning of each name in the list.
func prefixDirectory(directory string, names []string) {
	if directory != "." {
		for i, name := range names {
			names[i] = filepath.Join(directory, name)
		}
	}
}

// File is a wrapper for the state of a file used in the parser.
// The parse tree walkers are all methods of this type.
type File struct {
	fset       *token.FileSet
	name       string // Name of file.
	ident      string // Identifier we are searching for.
	pathPrefix string // Prefix from GOROOT/GOPATH.
	urlPrefix  string // Start of corresponding URL for golang.org or godoc.org.
	file       *ast.File
	comments   ast.CommentMap
	objs       map[*ast.Ident]types.Object
	doPrint    bool
	found      bool
	allFiles   []*File // All files in the package.
}

const godocOrg = "http://godoc.org"

// doPackage analyzes the single package constructed from the named files, looking for
// the definition of ident.
func doPackage(pkg *ast.Package, fset *token.FileSet, ident string) {
	var files []*File
	found := false
	for name, astFile := range pkg.Files {
		if *packageFlag && astFile.Doc == nil {
			continue
		}
		file := &File{
			fset:     fset,
			name:     name,
			ident:    ident,
			file:     astFile,
			comments: ast.NewCommentMap(fset, astFile, astFile.Comments),
		}
		switch {
		case strings.HasPrefix(name, goRootSrcPkg):
			file.urlPrefix = "http://golang.org/pkg"
			file.pathPrefix = goRootSrcPkg
		case strings.HasPrefix(name, goRootSrcCmd):
			file.urlPrefix = "http://golang.org/cmd"
			file.pathPrefix = goRootSrcCmd
		default:
			file.urlPrefix = godocOrg
			for _, path := range goPaths {
				p := filepath.Join(path, "src")
				if strings.HasPrefix(name, p) {
					file.pathPrefix = p
					break
				}
			}
		}
		files = append(files, file)
		if found {
			continue
		}
		file.doPrint = false
		if *packageFlag {
			file.pkgComments()
		} else {
			ast.Walk(file, file.file)
			if file.found {
				found = true
			}
		}
	}

	if !found {
		return
	}

	// Type check to build map from name to type.
	objects := make(map[*ast.Ident]types.Object)
	// By providing the Context with our own error function, it will continue
	// past the first error. There is no need for that function to do anything.
	config := types.Config{
		Error: func(error) {},
	}
	info := &types.Info{
		Objects: objects,
	}
	path := ""
	var astFiles []*ast.File
	for name, astFile := range pkg.Files {
		if path == "" {
			path = name
		}
		astFiles = append(astFiles, astFile)
	}
	config.Check(path, fset, astFiles, info) // Ignore errors.

	// We need to search all files for methods, so record the full list in each file.
	for _, file := range files {
		file.allFiles = files
	}
	for _, file := range files {
		file.doPrint = true
		file.objs = objects
		if *packageFlag {
			file.pkgComments()
		} else {
			ast.Walk(file, file.file)
		}
	}
}

// Visit implements the ast.Visitor interface.
func (f *File) Visit(node ast.Node) ast.Visitor {
	switch n := node.(type) {
	case *ast.GenDecl:
		// Variables, constants, types.
		for _, spec := range n.Specs {
			switch spec := spec.(type) {
			case *ast.ValueSpec:
				if *constantFlag && n.Tok == token.CONST || *variableFlag && n.Tok == token.VAR {
					for _, ident := range spec.Names {
						if equal(ident.Name, f.ident) {
							f.printNode(n, ident, f.nameURL(ident.Name))
							break
						}
					}
				}
			case *ast.TypeSpec:
				// If there is only one Spec, there are probably no parens and the
				// comment we want appears before the type keyword, bound to
				// the GenDecl. If the Specs are parenthesized, the comment we want
				// is bound to the Spec. Hence we dig into the GenDecl to the Spec,
				// but only if there are no parens.
				node := ast.Node(n)
				if n.Lparen.IsValid() {
					node = spec
				}
				if equal(spec.Name.Name, f.ident) {
					if *typeFlag {
						f.printNode(node, spec.Name, f.nameURL(spec.Name.Name))
					} else {
						switch spec.Type.(type) {
						case *ast.InterfaceType:
							if *interfaceFlag {
								f.printNode(node, spec.Name, f.nameURL(spec.Name.Name))
							}
						case *ast.StructType:
							if *structFlag {
								f.printNode(node, spec.Name, f.nameURL(spec.Name.Name))
							}
						}
					}
					if f.doPrint {
						ms := f.objs[spec.Name].Type().MethodSet()
						if ms.Len() == 0 {
							ms = types.NewPointer(f.objs[spec.Name].Type()).MethodSet()
						}
						f.methodSet(ms)
					}
				}
			case *ast.ImportSpec:
				continue // Don't care.
			}
		}
	case *ast.FuncDecl:
		// Methods, top-level functions.
		if equal(n.Name.Name, f.ident) {
			n.Body = nil // Do not print the function body.
			if *methodFlag && n.Recv != nil {
				f.printNode(n, n.Name, f.methodURL(n.Recv.List[0].Type, n.Name.Name))
			} else if *functionFlag && n.Recv == nil {
				f.printNode(n, n.Name, f.nameURL(n.Name.Name))
			}
		}
	}
	return f
}

func exported(name string) bool {
	r, _ := utf8.DecodeRuneInString(name)
	return unicode.IsUpper(r)
}

func equal(n1, n2 string) bool {
	// n1 must  be exported.
	if !exported(n1) {
		return false
	}
	return strings.ToLower(n1) == strings.ToLower(n2)
}

func (f *File) printNode(node, ident ast.Node, url string) {
	if !f.doPrint {
		f.found = true
		return
	}
	fmt.Printf("%s%s%s", url, f.sourcePos(f.fset.Position(ident.Pos())), f.docs(node))
}

func (f *File) docs(node ast.Node) []byte {
	if !*docFlag {
		return nil
	}
	commentedNode := printer.CommentedNode{Node: node}
	if comments := f.comments.Filter(node).Comments(); comments != nil {
		commentedNode.Comments = comments
	}
	var b bytes.Buffer
	printer.Fprint(&b, f.fset, &commentedNode)
	b.Write([]byte("\n\n")) // Add a blank line between entries if we print documentation.
	return b.Bytes()
}

func (f *File) pkgComments() {
	doc := f.file.Doc
	if doc == nil {
		return
	}
	url := ""
	if *urlFlag {
		url = f.packageURL() + "\n"
	}
	docText := ""
	if *docFlag {
		docText = fmt.Sprintf("package %s\n%s\n\n", f.file.Name.Name, doc.Text())
	}
	fmt.Printf("%s%s%s", url, f.sourcePos(f.fset.Position(doc.Pos())), docText)
}

func (f *File) packageURL() string {
	s := strings.TrimPrefix(f.name, f.pathPrefix)
	// Now we have a path with a final file name. Drop it.
	if i := strings.LastIndex(s, slash); i > 0 {
		s = s[:i+1]
	}
	return f.urlPrefix + s
}

func (f *File) sourcePos(posn token.Position) string {
	if !*srcFlag {
		return ""
	}
	return fmt.Sprintf("%s:%d:\n", posn.Filename, posn.Line)
}

func (f *File) nameURL(name string) string {
	if !*urlFlag {
		return ""
	}
	return fmt.Sprintf("%s#%s\n", f.packageURL(), name)
}

func (f *File) methodURL(typ ast.Expr, name string) string {
	if !*urlFlag {
		return ""
	}
	var b bytes.Buffer
	printer.Fprint(&b, f.fset, typ)
	typeName := b.Bytes()
	if len(typeName) > 0 && typeName[0] == '*' {
		typeName = typeName[1:]
	}
	return fmt.Sprintf("%s#%s.%s\n", f.packageURL(), typeName, name)
}

// Here follows the code to find and print a method (actually a method set, because
// we want to do only one redundant tree walk, not one per method).
// It should be much easier than walking the whole tree again, but that's what we must do.
// TODO.

type methodVisitor struct {
	*File
	methods []*types.Method
}

func (f *File) methodSet(set *types.MethodSet) {
	// Build the set of things we're looking for.
	methods := make([]*types.Method, 0, set.Len())
	for i := 0; i < set.Len(); i++ {
		if exported(set.At(i).Name()) {
			methods = append(methods, set.At(i))
		}
	}
	if len(methods) == 0 {
		return
	}
	for _, file := range f.allFiles {
		m := &methodVisitor{
			File:    file,
			methods: methods,
		}
		ast.Walk(m, file.file)
	}
}

// Visit implements the ast.Visitor interface.
func (m *methodVisitor) Visit(node ast.Node) ast.Visitor {
	switch n := node.(type) {
	case *ast.FuncDecl:
		for i, method := range m.methods {
			// If this is the right one, the position of the name of its identifier will match.
			if method.Pos() == n.Name.Pos() {
				n.Body = nil // TODO. Ugly - don't print the function body.
				m.printNode(n, n.Name, m.nameURL(n.Name.Name))
				// If this was the last method, we're done.
				if len(m.methods) == 1 {
					return nil
				}
				// Drop this one from the list.
				m.methods = append(m.methods[:i], m.methods[i+1:]...)
				return m
			}
		}
	}
	return m
}
