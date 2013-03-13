// Copyright 2013 The rspace Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Doc is a simple document printer that produces the doc comments
// for its argument symbols, using a more Go-like UI than godoc.
// It can also search for symbols by looking in all packages, and
// case is ignored. For instance:
//	doc isupper
// will find unicode.IsUpper.
//
// usage:
//	doc pkg.name   # "doc io.Writer"
//	doc pkg name   # "doc fmt Printf"
//	doc name       # "doc isupper" (finds unicode.IsUpper)
//
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
pkg is the last component of any package, e.g. fmt, parser
name is the name of an exported symbol; case is ignored in matches.
`

func usage() {
	fmt.Fprintf(os.Stderr, usageDoc)
	os.Exit(2)
}

func main() {
	flag.Parse()
	var pkg, name string
	switch flag.NArg() {
	case 1:
		if strings.Contains(flag.Arg(0), ".") {
			pkg, name = split(flag.Arg(0))
		} else {
			name = flag.Arg(0)
		}
	case 2:
		pkg, name = flag.Arg(0), flag.Arg(1)
	default:
		usage()
	}
	for _, path := range paths(pkg) {
		lookInDirectory(path, name)
	}
}

func split(arg string) (pkg, name string) {
	str := strings.Split(arg, ".")
	if len(str) != 2 {
		usage()
	}
	return str[0], str[1]
}

func paths(pkg string) []string {
	pkgs := pathsFor(runtime.GOROOT(), pkg)
	gopath := os.Getenv("GOPATH")
	if gopath != "" {
		for _, root := range splitGopath(gopath) {
			pkgs = append(pkgs, pathsFor(root, pkg)...)
		}
	}
	return pkgs
}

func splitGopath(gopath string) []string {
	// TODO: Assumes Unix.
	return strings.Split(gopath, ":")
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
		if strings.Contains(pathName, "/.") { // TODO: Unix-specific?
			return filepath.SkipDir
		}
		// Is the last element of the path correct
		if pkg == "" || path.Base(pathName) == pkg {
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
	fset     *token.FileSet
	name     string
	ident    string
	file     *ast.File
	comments ast.CommentMap
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
		thisFile := &File{
			fset:     fs,
			name:     name,
			ident:    ident,
			file:     parsedFile,
			comments: ast.NewCommentMap(fs, parsedFile, parsedFile.Comments),
		}
		files = append(files, thisFile)
		astFiles = append(astFiles, parsedFile)
	}
	for _, file := range files {
		ast.Walk(file, file.file)
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
				for _, ident := range spec.Names {
					if equal(ident.Name, f.ident) {
						f.printNode(n, n.Doc)
						break
					}
				}
			case *ast.TypeSpec:
				if equal(spec.Name.Name, f.ident) {
					f.printNode(n, n.Doc)
				}
			case *ast.ImportSpec:
				continue // Don't care.
			}
		}
	case *ast.FuncDecl:
		// Methods, top-level functions.
		if equal(n.Name.Name, f.ident) {
			n.Body = nil // Do not print the function body.
			f.printNode(n, n.Doc)
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

func (f *File) printNode(node ast.Node, comments *ast.CommentGroup) {
	commentedNode := printer.CommentedNode{Node: node}
	if comments != nil {
		commentedNode.Comments = []*ast.CommentGroup{comments}
	}
	var b bytes.Buffer
	printer.Fprint(&b, f.fset, &commentedNode)
	posn := f.fset.Position(node.Pos())
	fmt.Printf("%s:%d:\n%s\n\n", posn.Filename, posn.Line, b.Bytes())
}
