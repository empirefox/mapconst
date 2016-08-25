// +build go1.5

package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/build"
	"go/format"
	"go/parser"
	"go/token"
	"go/types"
	"io/ioutil"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
	"text/template"
)

var headerTmpl string = `// Code generated by \"mapconst %[1]s\"; DO NOT EDIT"

package %[2]s
`

type mapConstData struct {
	Type   string
	Consts []string
}

var mapConstTpl string = `
var {{.Type}}NameToValue = map[string]{{.Type}} {
	{{range .Consts}} "{{.}}":{{.}},
	{{end}}
}
`

var (
	config struct {
		typeNames string
		output    string
	}
)

func init() {
	flag.StringVar(&config.typeNames, "type", "", "comma-separated list of type names; must be set")
	flag.StringVar(&config.output, "output", "", "output file name; default srcdir/<type>_mapconst.go")
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("const_list: ")

	flag.Parse()
	if len(config.typeNames) == 0 {
		flag.Usage()
		os.Exit(2)
	}
	types := strings.Split(config.typeNames, ",")

	// We accept either one directory or a list of files. Which do we have?
	args := flag.Args()
	if len(args) == 0 {
		// Default: process whole package in current directory.
		args = []string{"."}
	}

	// Parse the package once.
	dir := ""
	var gen Generator
	if len(args) == 1 && isDirectory(args[0]) {
		dir = args[0]
		gen.parsePackageDir(args[0])
	} else {
		dir = filepath.Dir(args[0])
		gen.parsePackageFiles(args)
	}

	fmt.Fprintf(&gen.buf, headerTmpl, strings.Join(os.Args[1:], " "), gen.pkg.name)
	// Run generate for each type.
	for _, typeName := range types {
		gen.generate(typeName)
	}

	// Format the output.
	src := gen.format()

	// Write to file.
	outFilename := ""
	var err error
	switch config.output {
	case "stdout":
		fmt.Println(string(src))
	case "":
		outFilename = path.Join(dir, strings.ToLower(types[0])+"_mapconst.go")
	default:
		outFilename = config.output
	}

	if ioutil.WriteFile(outFilename, src, 0644); err != nil {
		log.Fatalf("writing output: %s", err)
	}
}

// isDirectory reports whether the named file is a directory.
func isDirectory(name string) bool {
	info, err := os.Stat(name)
	if err != nil {
		log.Fatal(err)
	}
	return info.IsDir()
}

// Generator holds the state of the analysis. Primarily used to buffer
// the output for format.Source.
type Generator struct {
	buf bytes.Buffer // Accumulated output.
	pkg *Package     // Package we are scanning.
}

func (g *Generator) Printf(format string, args ...interface{}) {
	fmt.Fprintf(&g.buf, format, args...)
}

// File holds a single parsed file and associated data.
type File struct {
	pkg  *Package  // Package to which this file belongs.
	file *ast.File // Parsed AST.
	// These fields are reset for each type being generated.
	typeName string // Name of the constant type.
	consts   []string
}

type Package struct {
	dir      string
	name     string
	defs     map[*ast.Ident]types.Object
	files    []*File
	typesPkg *types.Package
}

// parsePackageDir parses the package residing in the directory.
func (g *Generator) parsePackageDir(directory string) {
	pkg, err := build.Default.ImportDir(directory, 0)
	if err != nil {
		log.Fatalf("cannot process directory %s: %s", directory, err)
	}
	var names []string
	names = append(names, pkg.GoFiles...)
	names = append(names, pkg.CgoFiles...)
	// TODO: Need to think about constants in test files. Maybe write type_string_test.go
	// in a separate pass? For later.
	// names = append(names, pkg.TestGoFiles...) // These are also in the "foo" package.
	names = append(names, pkg.SFiles...)
	names = prefixDirectory(directory, names)
	g.parsePackage(directory, names, nil)
}

// parsePackageFiles parses the package occupying the named files.
func (g *Generator) parsePackageFiles(names []string) {
	g.parsePackage(".", names, nil)
}

// prefixDirectory places the directory name on the beginning of each name in the list.
func prefixDirectory(directory string, names []string) []string {
	if directory == "." {
		return names
	}
	ret := make([]string, len(names))
	for i, name := range names {
		ret[i] = filepath.Join(directory, name)
	}
	return ret
}

// parsePackage analyzes the single package constructed from the named files.
// If text is non-nil, it is a string to be used instead of the content of the file,
// to be used for testing. parsePackage exits if there is an error.
func (g *Generator) parsePackage(directory string, names []string, text interface{}) {
	var files []*File
	var astFiles []*ast.File
	g.pkg = new(Package)
	fs := token.NewFileSet()
	for _, name := range names {
		if !strings.HasSuffix(name, ".go") {
			continue
		}
		parsedFile, err := parser.ParseFile(fs, name, text, 0)
		if err != nil {
			log.Fatalf("parsing package: %s: %s", name, err)
		}
		astFiles = append(astFiles, parsedFile)
		files = append(files, &File{
			file: parsedFile,
			pkg:  g.pkg,
		})
	}
	if len(astFiles) == 0 {
		log.Fatalf("%s: no buildable Go files", directory)
	}
	g.pkg.name = astFiles[0].Name.Name
	g.pkg.files = files
	g.pkg.dir = directory
}

func (g *Generator) generate(typeName string) {
	consts := make([]string, 0, 100)
	for _, file := range g.pkg.files {
		// Set the state for this run of the walker.
		file.typeName = typeName
		file.consts = make([]string, 0)
		if file.file != nil {
			ast.Inspect(file.file, file.genDecl)
			consts = append(consts, file.consts...)
		}
	}

	if len(consts) == 0 {
		log.Fatalf("no const defined for type %s", typeName)
	}

	tpl := template.Must(template.New("mapConstTpl").Parse(mapConstTpl))
	tpl.Execute(&g.buf, &mapConstData{
		Type:   typeName,
		Consts: consts,
	})
}

// format returns the gofmt-ed contents of the Generator's buffer.
func (g *Generator) format() []byte {
	src, err := format.Source(g.buf.Bytes())
	if err != nil {
		// Should never happen, but can arise when developing this code.
		// The user can compile the output to see the error.
		log.Printf("warning: internal error: invalid Go generated: %s", err)
		log.Print("warning: compile the package to analyze the error")
		return g.buf.Bytes()
	}
	return src
}

// genDecl processes one declaration clause.
func (f *File) genDecl(node ast.Node) bool {
	decl, ok := node.(*ast.GenDecl)
	if !ok || decl.Tok != token.CONST {
		// We only care about const declarations.
		return true
	}
	// The name of the type of the constants we are declaring.
	// Can change if this is a multi-element declaration.
	typ := ""
	// Loop over the elements of the declaration. Each element is a ValueSpec:
	// a list of names possibly followed by a type, possibly followed by values.
	// If the type and value are both missing, we carry down the type (and value,
	// but the "go/types" package takes care of that).
	for _, spec := range decl.Specs {
		vspec := spec.(*ast.ValueSpec) // Guaranteed to succeed as this is CONST.
		if vspec.Type == nil && len(vspec.Values) > 0 {
			// "X = 1". With no type but a value, the constant is untyped.
			// Skip this vspec and reset the remembered type.
			typ = ""
			continue
		}
		if vspec.Type != nil {
			// "X T". We have a type. Remember it.
			ident, ok := vspec.Type.(*ast.Ident)
			if !ok {
				continue
			}
			typ = ident.Name
		}
		if typ == f.typeName {
			f.consts = append(f.consts, vspec.Names[0].Name)
		}
	}
	return false
}
