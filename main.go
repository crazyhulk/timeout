// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Stringer is a tool to automate the creation of methods that satisfy the fmt.Stringer
// interface. Given the name of a (signed or unsigned) integer type T that has constants
// defined, stringer will create a new self-contained Go source file implementing
//
//	func (t T) String() string
//
// The file is created in the same package and directory as the package that defines T.
// It has helpful defaults designed for use with go generate.
//
// Stringer works best with constants that are consecutive values such as created using iota,
// but creates good code regardless. In the future it might also provide custom support for
// constant sets that are bit patterns.
//
// For example, given this snippet,
//
//	package painkiller
//
//	type Pill int
//
//	const (
//		Placebo Pill = iota
//		Aspirin
//		Ibuprofen
//		Paracetamol
//		Acetaminophen = Paracetamol
//	)
//
// running this command
//
//	stringer -type=Pill
//
// in the same directory will create the file pill_string.go, in package painkiller,
// containing a definition of
//
//	func (Pill) String() string
//
// That method will translate the value of a Pill constant to the string representation
// of the respective constant name, so that the call fmt.Print(painkiller.Aspirin) will
// print the string "Aspirin".
//
// Typically this process would be run using go generate, like this:
//
//	//go:generate stringer -type=Pill
//
// If multiple constants have the same value, the lexically first matching name will
// be used (in the example, Acetaminophen will print as "Paracetamol").
//
// With no arguments, it processes the package in the current directory.
// Otherwise, the arguments must name a single directory holding a Go package
// or a set of Go source files that represent a single Go package.
//
// The -type flag accepts a comma-separated list of types so a single run can
// generate methods for multiple types. The default output file is t_string.go,
// where t is the lower-cased name of the first type listed. It can be overridden
// with the -output flag.
//
// The -linecomment flag tells stringer to generate the text of any line comment, trimmed
// of leading spaces, instead of the constant name. For instance, if the constants above had a
// Pill prefix, one could write
//
//	PillAspirin // Aspirin
//
// to suppress it in the output.
package main // import "golang.org/x/tools/cmd/stringer"

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/constant"
	"go/format"
	"go/token"
	"go/types"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/packages"
)

var (
	typeNames   = flag.String("type", "", "comma-separated list of type names; must be set")
	path        = flag.String("path", ".", "comma-separated list of type names; must be set")
	recursive   = flag.Bool("r", false, "is recursive")
	output      = flag.String("output", "", "output file name; default srcdir/<type>_string.go")
	trimprefix  = flag.String("trimprefix", "", "trim the `prefix` from the generated constant names")
	linecomment = flag.Bool("linecomment", false, "use line comment text as printed text when present")
	buildTags   = flag.String("tags", "", "comma-separated list of build tags to apply")
)

// Usage is a replacement usage function for the flags package.
func Usage() {
	fmt.Fprintf(os.Stderr, "Usage of stringer:\n")
	fmt.Fprintf(os.Stderr, "\tstringer [flags] -type T [directory]\n")
	fmt.Fprintf(os.Stderr, "\tstringer [flags] -type T files... # Must be a single package\n")
	fmt.Fprintf(os.Stderr, "For more information, see:\n")
	fmt.Fprintf(os.Stderr, "\thttps://pkg.go.dev/golang.org/x/tools/cmd/stringer\n")
	fmt.Fprintf(os.Stderr, "Flags:\n")
	flag.PrintDefaults()
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("stringer: ")
	flag.Usage = Usage
	flag.Parse()
	// if len(*typeNames) == 0 {
	// 	flag.Usage()
	// 	os.Exit(2)
	// }
	// types := strings.Split(*typeNames, ",")
	var tags []string
	if len(*buildTags) > 0 {
		tags = strings.Split(*buildTags, ",")
	}

	// We accept either one directory or a list of files. Which do we have?
	args := flag.Args()
	if len(args) == 0 {
		// Default: process whole package in current directory.
		args = []string{"."}
	}

	// Parse the package once.
	var dir string
	// TODO(suzmue): accept other patterns for packages (directories, list of files, import paths, etc).
	if len(args) == 1 && isDirectory(args[0]) {
		dir = args[0]
	} else {
		if len(tags) != 0 {
			log.Fatal("-tags option applies only to directories, not when files are specified")
		}
		dir = filepath.Dir(args[0])
	}
	_ = dir

	paths := map[string]bool{}
	// 列出当前目录及所有子目录中的 Go 包
	if err := filepath.Walk(args[0], func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}
		// 检查是否是目录
		// if !info.IsDir() {
		// 	return err
		// }

		if paths[filepath.Dir(path)] {
			return err
		}

		// 仅检查 .go 文件
		if !info.IsDir() /* && strings.HasSuffix(info.Name(), ".go") */ {
			if containsString(path, "// @timeout") {
				fmt.Println("=================", path, info.Name())
				paths[filepath.Dir(path)] = true
			}
		}

		// 尝试获取包的信息
		// pkg, err := build.ImportDir(path, 0)
		// if err == nil {
		// 	fmt.Println("=====", pkg.Name)
		// 	// 打印包的名字
		// 	paths = append(paths, args[0]+"/"+path)
		// }
		return nil
	}); err != nil {
		log.Fatal(err)
	}
	for path := range paths {
		g := Generator{
			trimPrefix:  *trimprefix,
			lineComment: *linecomment,
		}

		g.genForPath(path, []string{args[0] + "/" + path}, tags)
	}

	// g.parsePackage(args, tags)
}

// 检查文件中是否包含目标字符串
func containsString(filePath, target string) bool {
	file, err := os.Open(filePath)
	if err != nil {
		log.Printf("Error opening file: %v\n", err)
		return false
	}
	defer file.Close()

	// 使用扫描器逐行读取文件
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, target) {
			return true
		}
	}

	if err := scanner.Err(); err != nil {
		log.Printf("Error reading file: %v\n", err)
	}

	return false
}

func (g *Generator) genForPath(dir string, args, tags []string) {
	g.parsePackage(args, tags)
	if len(g.timeFuncs) == 0 {
		return
	}

	// Print the header and package clause.
	g.Printf("// Code generated by \"timeout %s\"; DO NOT EDIT.\n", strings.Join(os.Args[1:], " "))
	g.Printf("\n")
	g.Printf("package %s", g.pkg.name)
	g.Printf("\n")
	g.Printf("\n")
	g.Printf("import \"context\"\n") // Used by all methods.
	g.Printf("import \"time\"\n")    // Used by all methods.
	g.Printf("import \"errors\"\n")  // Used by all methods.
	g.genImports()

	g.Printf("\n")
	g.genVariable()
	g.Printf("\n")

	for _, fd := range g.timeFuncs {
		g.genTimeFunc(fd)
	}

	// Format the output.
	src := g.format()

	// Write to file.
	outputName := *output
	if outputName == "" {
		// baseName := fmt.Sprintf("%s_string.go", types[0])
		baseName := "timeout_gen.go"
		outputName = filepath.Join(dir, strings.ToLower(baseName))
	}
	err := os.WriteFile(outputName, src, 0644)
	if err != nil {
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

	timeFuncs []*ast.FuncDecl

	trimPrefix  string
	lineComment bool

	logf func(format string, args ...interface{}) // test logging hook; nil when not testing
}

func (g *Generator) Printf(format string, args ...interface{}) {
	fmt.Fprintf(&g.buf, format, args...)
}

// File holds a single parsed file and associated data.
type File struct {
	pkg  *Package  // Package to which this file belongs.
	file *ast.File // Parsed AST.
	// These fields are reset for each type being generated.
	typeName string  // Name of the constant type.
	values   []Value // Accumulator for constant values of that type.

	trimPrefix  string
	lineComment bool
}

type Package struct {
	name     string
	defs     map[*ast.Ident]types.Object
	impts    map[string]*packages.Package
	typeInfo *types.Info
	files    []*File
}

// parsePackage analyzes the single package constructed from the patterns and tags.
// parsePackage exits if there is an error.
func (g *Generator) parsePackage(patterns []string, tags []string) {
	fmt.Println("=first")
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedTypes | packages.NeedTypesInfo | packages.NeedSyntax | packages.NeedImports,
		// TODO: Need to think about constants in test files. Maybe write type_string_test.go
		// in a separate pass? For later.
		Tests:      false,
		BuildFlags: []string{fmt.Sprintf("-tags=%s", strings.Join(tags, " "))},
		Logf:       g.logf,
	}

	pkgs, err := packages.Load(cfg, patterns...)
	// 遍历所有包
	for _, pkg := range pkgs {
		// 遍历包中的每个 Go 文件
		for _, syntax := range pkg.Syntax {
			// 遍历文件中的声明
			for _, decl := range syntax.Decls {
				// 只处理函数声明
				if funcDecl, ok := decl.(*ast.FuncDecl); ok {
					// 检查函数声明中的注释
					if funcDecl.Doc != nil {
						for _, comment := range funcDecl.Doc.List {
							// 查找 go:generate timeout 指令
							if strings.HasPrefix(comment.Text, "// @timeout") {
								// fmt.Printf("======%+v\n", len(funcDecl.Doc.List))
								pos := pkg.Fset.Position(funcDecl.Pos())
								fmt.Printf("Found @timeout in %s at line %d for function %s: %s, type:%+v\n",
									pos.Filename, pos.Line, funcDecl.Name.Name, comment.Text, funcDecl.Type)
								g.timeFuncs = append(g.timeFuncs, funcDecl)
							}
						}
					}
				}
			}
		}
	}
	if err != nil {
		log.Fatal(err)
	}
	if len(pkgs) != 1 {
		log.Fatalf("error: %d packages matching %v", len(pkgs), strings.Join(patterns, " "))
	}
	g.addPackage(pkgs[0])
}

// addPackage adds a type checked Package and its syntax files to the generator.
func (g *Generator) addPackage(pkg *packages.Package) {
	g.pkg = &Package{
		name:     pkg.Name,
		defs:     pkg.TypesInfo.Defs,
		impts:    pkg.Imports,
		typeInfo: pkg.TypesInfo,
		files:    make([]*File, len(pkg.Syntax)),
	}

	for i, file := range pkg.Syntax {
		g.pkg.files[i] = &File{
			file:        file,
			pkg:         g.pkg,
			trimPrefix:  g.trimPrefix,
			lineComment: g.lineComment,
		}
	}
}

func (g *Generator) genImports() {
	// 遍历文件中的声明
	for _, fn := range g.timeFuncs {
		if fn.Type.Params != nil {
			for _, field := range fn.Type.Params.List {
				if id, ok := field.Type.(*ast.SelectorExpr); ok {
					packageName := id.X.(*ast.Ident).Name
					if packageName == "context" || packageName == "time" {
						continue
					}
					for _, p := range g.pkg.impts {
						if strings.HasSuffix(p.PkgPath, packageName) {
							g.Printf("import \"%s\"\n", p.PkgPath)
						}
					}

				}
			}
		}
		if fn.Type.Results != nil {
			for _, field := range fn.Type.Results.List {
				if id, ok := field.Type.(*ast.SelectorExpr); ok {
					packageName := id.X.(*ast.Ident).Name
					if packageName == "context" || packageName == "time" {
						continue
					}

					for _, p := range g.pkg.impts {
						if strings.HasSuffix(p.PkgPath, packageName) {
							g.Printf("import \"%s\"\n", p.PkgPath)
						}
					}

				}
			}
		}
	}
}

// 将表达式转换为字符串（包括处理选择器表达式，如 foo.ErrCode）
func formatExpr(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		// 基础类型，如 int, string 等
		return e.Name
	case *ast.SelectorExpr:
		// 选择器表达式，如 foo.ErrCode
		pkg := formatExpr(e.X) // 获取包名
		return fmt.Sprintf("%s.%s", pkg, e.Sel.Name)
	case *ast.StarExpr:
		// 指针类型，如 *foo.ErrCode
		return fmt.Sprintf("*%s", formatExpr(e.X))
	case *ast.ArrayType:
		// 数组类型，如 []int
		return fmt.Sprintf("[]%s", formatExpr(e.Elt))
	case *ast.MapType:
		// map 类型，如 map[string]int
		return fmt.Sprintf("map[%s]%s", formatExpr(e.Key), formatExpr(e.Value))
	case *ast.FuncType:
		// 函数类型，返回 func signature
		return "func" // 简单返回 func，详细可以扩展
	default:
		// 其他类型暂时忽略，可以继续扩展
		return "<unknown>"
	}
}

// 获取函数签名的字符串表示
func getFuncSignature(fn *ast.FuncDecl, suffix string, args []string) string {
	receiver := []string{}
	if fn.Recv != nil {
		for _, field := range fn.Recv.List {
			receNames := []string{}
			for _, name := range field.Names {
				receNames = append(receNames, name.Name)
			}

			receiver = append(receiver, fmt.Sprintf("(%s %s)", strings.Join(receNames, ", "), formatExpr(field.Type)))
		}
	}

	// 获取函数名称
	funcName := fn.Name.Name

	// 获取参数
	var params []string
	if fn.Type.Params != nil {
		for _, param := range fn.Type.Params.List {
			paramType := param.Type

			argNames := []string{}
			for _, name := range param.Names {
				argNames = append(argNames, name.Name)
			}
			params = append(params, fmt.Sprintf("%s %s", strings.Join(argNames, ", "), formatExpr(paramType)))
		}
	}

	resNoName := false
	// 获取返回值
	var results []string
	if fn.Type.Results != nil {
		for _, result := range fn.Type.Results.List {
			resultType := result.Type
			resNames := []string{}
			for _, name := range result.Names {
				resNames = append(resNames, name.Name)
				resNoName = false
			}
			results = append(results, fmt.Sprintf("%s %s", strings.Join(resNames, ", "), formatExpr(resultType)))
		}
	}

	prefix := "func"
	if len(receiver) > 0 {
		prefix += strings.Join(receiver, ", ")
	}
	params = append(params, args...)
	if len(results) == 0 {
		return fmt.Sprintf(prefix+" %s(%s)", funcName+suffix, strings.Join(params, ", "))
	} else if len(results) == 1 {
		if resNoName {
			return fmt.Sprintf(prefix+" %s(%s) %s", funcName+suffix, strings.Join(params, ", "), results[0])
		} else {
			return fmt.Sprintf(prefix+" %s(%s) (%s)", funcName+suffix, strings.Join(params, ", "), results[0])
		}
	}

	// 拼接函数签名
	return fmt.Sprintf(prefix+" %s(%s) (%s)", funcName+suffix, strings.Join(params, ", "), strings.Join(results, ", "))
}

func (g *Generator) genVariable() {
	g.Printf("var (\n")
	g.Printf("\tErrTimeout = errors.New(\"chronos timeout\")\n")
	g.Printf(")\n")
}

func (g *Generator) genTimeFunc(fn *ast.FuncDecl) {
	var receName string
	if fn.Recv != nil {
		receName = fn.Recv.List[0].Names[0].Name
	}

	args := []string{}
	if fn.Type.Params != nil {
		for _, v := range fn.Type.Params.List {
			for _, n := range v.Names {
				args = append(args, n.Name)
			}
		}
	}

	g.Printf("%s {\n", getFuncSignature(fn, "WithTimeout", []string{"timeout time.Duration"}))
	g.Printf("\tif timeout == 0 {\n")

	call := ""
	if receName != "" {
		call = receName + "."
	}
	if len(args) == 0 {
		call += fmt.Sprintf("%s()", fn.Name.Name)
	} else {
		call += fmt.Sprintf("%s(%s)", fn.Name.Name, strings.Join(args, ", "))
	}

	if fn.Type.Results != nil && len(fn.Type.Results.List) > 0 {
		g.Printf("\t\treturn %s\n", call)
	} else {
		g.Printf("\t\t%s\n", call)
		g.Printf("\t\treturn\n")
	}

	g.Printf("\t}\n\n")
	// g.Printf("\ttraceID := ctx.Value(ctxkit.TraceIDKey)\n")
	g.Printf("\tnctx := context.Background()\n")
	// g.Printf("\tnctx = context.WithValue(nctx, ctxkit.TraceIDKey, traceID)\n")
	g.Printf("\tnctx, cancel := context.WithTimeout(nctx, timeout)\n")
	g.Printf("\tdefer cancel()\n\n")
	g.Printf("\tresultChan := make(chan error, 1)\n")

	res := []string{}
	if fn.Type.Results != nil {
		for _, v := range fn.Type.Results.List {
			for _, n := range v.Names {
				res = append(res, n.Name)
			}
		}

		if len(res) == 0 {
			for i, v := range fn.Type.Results.List {
				res = append(res, "r"+fmt.Sprint(i))
				g.Printf("\tvar r%d %s\n", i, formatExpr(v.Type))
			}
		}
	}

	g.Printf("\tgo func() {\n")
	if len(res) == 0 {
		g.Printf("\t\t%s\n", call)
	} else {
		g.Printf("\t\t%s = %s\n", strings.Join(res, ", "), call)
	}

	g.Printf("\t\tresultChan <- nil\n")
	g.Printf("\t}()\n\n")
	g.Printf("\tselect {\n")
	g.Printf("\tcase <-nctx.Done():\n")
	g.Printf("\t\t\treturn ErrTimeout\n")
	g.Printf("\tcase _ = <-resultChan:\n")
	g.Printf("\t\t\treturn %s\n", strings.Join(res, ", "))
	g.Printf("\t}\n\n")
	g.Printf("\treturn %s\n", strings.Join(res, ", "))
	g.Printf("}\n")
}

// format returns the gofmt-ed contents of the Generator's buffer.
func (g *Generator) format() []byte {
	src, err := format.Source(g.buf.Bytes())
	if err != nil {
		// Should never happen, but can arise when developing this code.
		// The user can compile the output to see the error.
		log.Printf("warning: internal error: invalid Go generated: %s", err)
		log.Printf("warning: compile the package to analyze the error")
		return g.buf.Bytes()
	}
	return src
}

// Value represents a declared constant.
type Value struct {
	originalName string // The name of the constant.
	name         string // The name with trimmed prefix.
	// The value is stored as a bit pattern alone. The boolean tells us
	// whether to interpret it as an int64 or a uint64; the only place
	// this matters is when sorting.
	// Much of the time the str field is all we need; it is printed
	// by Value.String.
	value  uint64 // Will be converted to int64 when needed.
	signed bool   // Whether the constant is a signed type.
	str    string // The string representation given by the "go/constant" package.
}

func (v *Value) String() string {
	return v.str
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
			// "X = 1". With no type but a value. If the constant is untyped,
			// skip this vspec and reset the remembered type.
			typ = ""

			// If this is a simple type conversion, remember the type.
			// We don't mind if this is actually a call; a qualified call won't
			// be matched (that will be SelectorExpr, not Ident), and only unusual
			// situations will result in a function call that appears to be
			// a type conversion.
			ce, ok := vspec.Values[0].(*ast.CallExpr)
			if !ok {
				continue
			}
			id, ok := ce.Fun.(*ast.Ident)
			if !ok {
				continue
			}
			typ = id.Name
		}
		if vspec.Type != nil {
			// "X T". We have a type. Remember it.
			ident, ok := vspec.Type.(*ast.Ident)
			if !ok {
				continue
			}
			typ = ident.Name
		}
		if typ != f.typeName {
			// This is not the type we're looking for.
			continue
		}
		// We now have a list of names (from one line of source code) all being
		// declared with the desired type.
		// Grab their names and actual values and store them in f.values.
		for _, name := range vspec.Names {
			if name.Name == "_" {
				continue
			}
			// This dance lets the type checker find the values for us. It's a
			// bit tricky: look up the object declared by the name, find its
			// types.Const, and extract its value.
			obj, ok := f.pkg.defs[name]
			if !ok {
				log.Fatalf("no value for constant %s", name)
			}
			info := obj.Type().Underlying().(*types.Basic).Info()
			if info&types.IsInteger == 0 {
				log.Fatalf("can't handle non-integer constant type %s", typ)
			}
			value := obj.(*types.Const).Val() // Guaranteed to succeed as this is CONST.
			if value.Kind() != constant.Int {
				log.Fatalf("can't happen: constant is not an integer %s", name)
			}
			i64, isInt := constant.Int64Val(value)
			u64, isUint := constant.Uint64Val(value)
			if !isInt && !isUint {
				log.Fatalf("internal error: value of %s is not an integer: %s", name, value.String())
			}
			if !isInt {
				u64 = uint64(i64)
			}
			v := Value{
				originalName: name.Name,
				value:        u64,
				signed:       info&types.IsUnsigned == 0,
				str:          value.String(),
			}
			if c := vspec.Comment; f.lineComment && c != nil && len(c.List) == 1 {
				v.name = strings.TrimSpace(c.Text())
			} else {
				v.name = strings.TrimPrefix(v.originalName, f.trimPrefix)
			}
			f.values = append(f.values, v)
		}
	}
	return false
}

// Helpers

// usize returns the number of bits of the smallest unsigned integer
// type that will hold n. Used to create the smallest possible slice of
// integers to use as indexes into the concatenated strings.
func usize(n int) int {
	switch {
	case n < 1<<8:
		return 8
	case n < 1<<16:
		return 16
	default:
		// 2^32 is enough constants for anyone.
		return 32
	}
}

// declareIndexAndNameVars declares the index slices and concatenated names
// strings representing the runs of values.
func (g *Generator) declareIndexAndNameVars(runs [][]Value, typeName string) {
	var indexes, names []string
	for i, run := range runs {
		index, name := g.createIndexAndNameDecl(run, typeName, fmt.Sprintf("_%d", i))
		if len(run) != 1 {
			indexes = append(indexes, index)
		}
		names = append(names, name)
	}
	g.Printf("const (\n")
	for _, name := range names {
		g.Printf("\t%s\n", name)
	}
	g.Printf(")\n\n")

	if len(indexes) > 0 {
		g.Printf("var (")
		for _, index := range indexes {
			g.Printf("\t%s\n", index)
		}
		g.Printf(")\n\n")
	}
}

// declareIndexAndNameVar is the single-run version of declareIndexAndNameVars
func (g *Generator) declareIndexAndNameVar(run []Value, typeName string) {
	index, name := g.createIndexAndNameDecl(run, typeName, "")
	g.Printf("const %s\n", name)
	g.Printf("var %s\n", index)
}

// createIndexAndNameDecl returns the pair of declarations for the run. The caller will add "const" and "var".
func (g *Generator) createIndexAndNameDecl(run []Value, typeName string, suffix string) (string, string) {
	b := new(bytes.Buffer)
	indexes := make([]int, len(run))
	for i := range run {
		b.WriteString(run[i].name)
		indexes[i] = b.Len()
	}
	nameConst := fmt.Sprintf("_%s_name%s = %q", typeName, suffix, b.String())
	nameLen := b.Len()
	b.Reset()
	fmt.Fprintf(b, "_%s_index%s = [...]uint%d{0, ", typeName, suffix, usize(nameLen))
	for i, v := range indexes {
		if i > 0 {
			fmt.Fprintf(b, ", ")
		}
		fmt.Fprintf(b, "%d", v)
	}
	fmt.Fprintf(b, "}")
	return b.String(), nameConst
}

// declareNameVars declares the concatenated names string representing all the values in the runs.
func (g *Generator) declareNameVars(runs [][]Value, typeName string, suffix string) {
	g.Printf("const _%s_name%s = \"", typeName, suffix)
	for _, run := range runs {
		for i := range run {
			g.Printf("%s", run[i].name)
		}
	}
	g.Printf("\"\n")
}

// buildOneRun generates the variables and String method for a single run of contiguous values.
func (g *Generator) buildOneRun(runs [][]Value, typeName string) {
	values := runs[0]
	g.Printf("\n")
	g.declareIndexAndNameVar(values, typeName)
	// The generated code is simple enough to write as a Printf format.
	lessThanZero := ""
	if values[0].signed {
		lessThanZero = "i < 0 || "
	}
	if values[0].value == 0 { // Signed or unsigned, 0 is still 0.
		g.Printf(stringOneRun, typeName, usize(len(values)), lessThanZero)
	} else {
		g.Printf(stringOneRunWithOffset, typeName, values[0].String(), usize(len(values)), lessThanZero)
	}
}

// Arguments to format are:
//
//	[1]: type name
//	[2]: size of index element (8 for uint8 etc.)
//	[3]: less than zero check (for signed types)
const stringOneRun = `func (i %[1]s) String() string {
	if %[3]si >= %[1]s(len(_%[1]s_index)-1) {
		return "%[1]s(" + strconv.FormatInt(int64(i), 10) + ")"
	}
	return _%[1]s_name[_%[1]s_index[i]:_%[1]s_index[i+1]]
}
`

// Arguments to format are:
//	[1]: type name
//	[2]: lowest defined value for type, as a string
//	[3]: size of index element (8 for uint8 etc.)
//	[4]: less than zero check (for signed types)
/*
 */
const stringOneRunWithOffset = `func (i %[1]s) String() string {
	i -= %[2]s
	if %[4]si >= %[1]s(len(_%[1]s_index)-1) {
		return "%[1]s(" + strconv.FormatInt(int64(i + %[2]s), 10) + ")"
	}
	return _%[1]s_name[_%[1]s_index[i] : _%[1]s_index[i+1]]
}
`

// buildMultipleRuns generates the variables and String method for multiple runs of contiguous values.
// For this pattern, a single Printf format won't do.
func (g *Generator) buildMultipleRuns(runs [][]Value, typeName string) {
	g.Printf("\n")
	g.declareIndexAndNameVars(runs, typeName)
	g.Printf("func (i %s) String() string {\n", typeName)
	g.Printf("\tswitch {\n")
	for i, values := range runs {
		if len(values) == 1 {
			g.Printf("\tcase i == %s:\n", &values[0])
			g.Printf("\t\treturn _%s_name_%d\n", typeName, i)
			continue
		}
		if values[0].value == 0 && !values[0].signed {
			// For an unsigned lower bound of 0, "0 <= i" would be redundant.
			g.Printf("\tcase i <= %s:\n", &values[len(values)-1])
		} else {
			g.Printf("\tcase %s <= i && i <= %s:\n", &values[0], &values[len(values)-1])
		}
		if values[0].value != 0 {
			g.Printf("\t\ti -= %s\n", &values[0])
		}
		g.Printf("\t\treturn _%s_name_%d[_%s_index_%d[i]:_%s_index_%d[i+1]]\n",
			typeName, i, typeName, i, typeName, i)
	}
	g.Printf("\tdefault:\n")
	g.Printf("\t\treturn \"%s(\" + strconv.FormatInt(int64(i), 10) + \")\"\n", typeName)
	g.Printf("\t}\n")
	g.Printf("}\n")
}

// buildMap handles the case where the space is so sparse a map is a reasonable fallback.
// It's a rare situation but has simple code.
func (g *Generator) buildMap(runs [][]Value, typeName string) {
	g.Printf("\n")
	g.declareNameVars(runs, typeName, "")
	g.Printf("\nvar _%s_map = map[%s]string{\n", typeName, typeName)
	n := 0
	for _, values := range runs {
		for _, value := range values {
			g.Printf("\t%s: _%s_name[%d:%d],\n", &value, typeName, n, n+len(value.name))
			n += len(value.name)
		}
	}
	g.Printf("}\n\n")
	g.Printf(stringMap, typeName)
}

// Argument to format is the type name.
const stringMap = `func (i %[1]s) String() string {
	if str, ok := _%[1]s_map[i]; ok {
		return str
	}
	return "%[1]s(" + strconv.FormatInt(int64(i), 10) + ")"
}
`
