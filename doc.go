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
	"go/build"
	"go/parser"
	"go/printer"
	"go/token"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"unicode"
	"unicode/utf8"
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
	pkg, err := build.Default.ImportDir(directory, 0)
	if err != nil {
		// If it's just that there are no go source files, that's fine.
		if _, nogo := err.(*build.NoGoError); nogo {
			return
		}
		// Non-fatal: we are doing a recursive walk and there may be other directories.
		return
	}
	var fileNames []string
	fileNames = append(fileNames, pkg.GoFiles...)
	prefixDirectory(directory, fileNames)
	doPackage(fileNames, name)
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
	name       string
	ident      string
	pathPrefix string
	urlPrefix  string
	file       *ast.File
	comments   ast.CommentMap
}

// doPackage analyzes the single package constructed from the named files, looking for
// the definition of ident.
func doPackage(fileNames []string, ident string) {
	var files []*File
	var astFiles []*ast.File
	fs := token.NewFileSet()
	for _, name := range fileNames {
		f, err := os.Open(name)
		if err != nil {
			// Warn but continue to next package.
			fmt.Fprintf(os.Stderr, "%s: %s", name, err)
			return
		}
		defer f.Close()
		data, err := ioutil.ReadAll(f)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %s", name, err)
			return
		}
		parsedFile, err := parser.ParseFile(fs, name, bytes.NewReader(data), parser.ParseComments)
		if err != nil {
			// Noisy - just ignore.
			// fmt.Fprintf(os.Stderr, "%s: %s", name, err)
			return
		}
		var urlPrefix, pathPrefix string
		switch {
		case strings.HasPrefix(name, goRootSrcPkg):
			urlPrefix = "http://golang.org/pkg"
			pathPrefix = goRootSrcPkg
		case strings.HasPrefix(name, goRootSrcCmd):
			urlPrefix = "http://golang.org/cmd"
			pathPrefix = goRootSrcCmd
		default:
			urlPrefix = "http://godoc.org"
			for _, path := range goPaths {
				p := filepath.Join(path, "src")
				if strings.HasPrefix(name, p) {
					pathPrefix = p
					break
				}
			}
		}
		thisFile := &File{
			fset:       fs,
			name:       name,
			ident:      ident,
			file:       parsedFile,
			pathPrefix: pathPrefix,
			urlPrefix:  urlPrefix,
			comments:   ast.NewCommentMap(fs, parsedFile, parsedFile.Comments),
		}
		files = append(files, thisFile)
		astFiles = append(astFiles, parsedFile)
	}
	for _, file := range files {
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
					tag := "pkg-constants"
					if n.Tok == token.VAR {
						tag = "pkg-variables"
					}
					for _, ident := range spec.Names {
						if equal(ident.Name, f.ident) {
							f.printNode(n, ident, f.nameURL(tag))
							break
						}
					}
				}
			case *ast.TypeSpec:
				if equal(spec.Name.Name, f.ident) {
					if *typeFlag {
						f.printNode(spec, spec.Name, f.nameURL(spec.Name.Name))
						break
					}
					switch spec.Type.(type) {
					case *ast.InterfaceType:
						if *interfaceFlag {
							f.printNode(spec, spec.Name, f.nameURL(spec.Name.Name))
						}
					case *ast.StructType:
						if *structFlag {
							f.printNode(spec, spec.Name, f.nameURL(spec.Name.Name))
						}
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

func equal(n1, n2 string) bool {
	// n1 must  be exported.
	r, _ := utf8.DecodeRuneInString(n1)
	if !unicode.IsUpper(r) {
		return false
	}
	return strings.ToLower(n1) == strings.ToLower(n2)
}

func (f *File) printNode(node, ident ast.Node, url string) {
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
