package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/types"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/tools/go/packages"
)

var (
	recursive   = flag.Bool("r", false, "is recursive")
	output      = flag.String("output", "", "output file name; default srcdir/<type>_string.go")
	trimprefix  = flag.String("trimprefix", "", "trim the `prefix` from the generated constant names")
	linecomment = flag.Bool("linecomment", false, "use line comment text as printed text when present")
)

// Usage is a replacement usage function for the flags package.
func Usage() {
	fmt.Fprintf(os.Stderr, "Usage of timeout:\n")
	fmt.Fprintf(os.Stderr, "\ttimeout [flags] -type T [directory]\n")
	flag.PrintDefaults()
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("timeout: ")
	flag.Usage = Usage
	flag.Parse()

	// We accept either one directory or a list of files. Which do we have?
	args := flag.Args()
	if len(args) == 0 {
		// Default: process whole package in current directory.
		args = []string{"."}
	}

	if !*recursive {
		g := Generator{
			trimPrefix:  *trimprefix,
			lineComment: *linecomment,
		}

		g.genForPath(args[0], []string{args[0]}, []string{})
		return
	}

	eg := sync.WaitGroup{}
	maxWorkers := 10
	workChan := make(chan string, maxWorkers)

	locker := sync.Mutex{}
	paths := map[string][]string{}
	// 列出当前目录及所有子目录中的 Go 包
	if err := filepath.Walk(args[0], func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() || strings.HasSuffix(info.Name(), ".go") {
			return nil
		}

		go func() {
			workChan <- path
			defer func() { <-workChan }()
			eg.Add(1)
			defer eg.Done()
			if !containsString(path, "// @timeout") {
				return
			}
			locker.Lock()
			defer locker.Unlock()
			paths[filepath.Dir(path)] = append(paths[filepath.Dir(path)], path)

			return
		}()

		return nil
	}); err != nil {
		log.Fatal(err)
	}
	eg.Wait()

	eg = sync.WaitGroup{}
	for path, patterns := range paths {
		g := Generator{
			trimPrefix:  *trimprefix,
			lineComment: *linecomment,
		}

		xpath := path
		xpatterns := patterns
		go func() {
			workChan <- xpath
			defer func() { <-workChan }()
			eg.Add(1)
			defer eg.Done()

			g.genForPath(xpath, xpatterns, []string{})
		}()
	}

	eg.Wait()
}

// 检查文件中是否包含目标字符串
func containsString(filePath, target string) bool {
	file, err := os.Open(filePath)
	if err != nil {
		log.Printf("Error opening file: %v\n", err)
		return false
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	for {
		line, err := reader.ReadString('\n') // 逐行读取直到遇到换行符
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			log.Printf("Error reading file: %v\n", err)
			return false
		}
		if strings.Contains(line, target) {
			return true
		}
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
		for _, fn := range fd.fns {
			g.genTimeFunc(fn)
			g.Printf("\n")
		}
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

type timeFunc struct {
	fns  []*ast.FuncDecl
	file *ast.File
}

// ImportAliasMap 存储 import 路径和别名的映射关系
type ImportAliasMap map[string]string

// 从 AST 文件中提取 import 别名信息
func (t timeFunc) getImportAliases() ImportAliasMap {
	aliases := ImportAliasMap{}
	for _, imp := range t.file.Imports {
		path := imp.Path.Value[1 : len(imp.Path.Value)-1] // 去掉引号
		alias := ""
		if imp.Name != nil {
			alias = imp.Name.Name
		}
		aliases[path] = alias
	}
	return aliases
}

// Generator holds the state of the analysis. Primarily used to buffer
// the output for format.Source.
type Generator struct {
	rawPkg *packages.Package
	buf    bytes.Buffer // Accumulated output.
	pkg    *Package     // Package we are scanning.

	timeFuncs []timeFunc

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
	typeName string // Name of the constant type.

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
			t := timeFunc{
				fns:  []*ast.FuncDecl{},
				file: syntax,
			}
			// 遍历文件中的声明
			for _, decl := range syntax.Decls {
				// 只处理函数声明
				funcDecl, ok := decl.(*ast.FuncDecl)
				if !ok {
					continue
				}
				// 检查函数声明中的注释
				if funcDecl.Doc == nil {
					continue

				}
				for _, comment := range funcDecl.Doc.List {
					// 查找 go:generate timeout 指令
					if strings.HasPrefix(comment.Text, "// @timeout") {
						continue
					}
					pos := pkg.Fset.Position(funcDecl.Pos())
					fmt.Printf("Found @timeout in %s at line %d for function %s: %s, type:%+v\n",
						pos.Filename, pos.Line, funcDecl.Name.Name, comment.Text, funcDecl.Type)
					t.fns = append(t.fns, funcDecl)
				}
			}
			if len(t.fns) > 0 {
				g.timeFuncs = append(g.timeFuncs, t)
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
	g.rawPkg = pkg
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

type Elem interface {
	Elem() types.Type
}

// getNamedType 递归解析类型，返回命名类型的包路径和名称
func getNamedType(typ types.Type) types.Type {
	if _, ok := typ.(*types.Named); ok {
		return typ
	}
	elem, ok := typ.(Elem)
	if ok {
		return getNamedType(elem.Elem())
	}

	return nil
}

func (g *Generator) genImports() {
	// 跳过当前包
	exist := map[string]bool{
		g.rawPkg.PkgPath: true,
	}

	// 遍历文件中的声明
	for _, f := range g.timeFuncs {
		aliases := f.getImportAliases()

		for _, fn := range f.fns {
			if fn.Type.Params != nil {
				for _, field := range fn.Type.Params.List {
					typ := g.pkg.typeInfo.TypeOf(field.Type)
					typ = getNamedType(typ)

					if named, ok := typ.(*types.Named); typ != nil && ok {
						obj := named.Obj()
						if obj != nil && obj.Pkg() != nil {
							packageName := obj.Name()
							if packageName == "Context" || packageName == "Time" {
								continue
							}

							path := obj.Pkg().Path()
							if _, ok := exist[path]; ok {
								continue
							}
							exist[path] = true

							if alias := aliases[path]; alias != "" {
								g.Printf("import %s \"%s\"\n", alias, obj.Pkg().Path())
							} else {
								g.Printf("import \"%s\"\n", obj.Pkg().Path())
							}
						}
					}
				}
			}
			if fn.Type.Results != nil {
				for _, field := range fn.Type.Results.List {
					typ := g.pkg.typeInfo.TypeOf(field.Type)
					typ = getNamedType(typ)

					if named, ok := typ.(*types.Named); ok {
						obj := named.Obj()
						if obj != nil && obj.Pkg() != nil {
							packageName := obj.Name()
							if packageName == "Context" || packageName == "Time" {
								continue
							}

							path := obj.Pkg().Path()
							if _, ok := exist[path]; ok {
								continue
							}
							exist[path] = true

							if alias := aliases[path]; alias != "" {
								g.Printf("import %s \"%s\"\n", alias, obj.Pkg().Path())
							} else {
								g.Printf("import \"%s\"\n", obj.Pkg().Path())
							}
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
			typ := formatExpr(v.Type)
			for _, n := range v.Names {
				res = append(res, n.Name)
				switch v.Type.(type) {
				case *ast.MapType, *ast.SliceExpr:
					g.Printf("\t%s = make(%s)\n", n.Name, typ)
				default:
				}
			}
		}

		if len(res) == 0 {
			for i, v := range fn.Type.Results.List {
				res = append(res, "r"+fmt.Sprint(i))
				typ := formatExpr(v.Type)
				switch v.Type.(type) {
				case *ast.MapType, *ast.SliceExpr:
					g.Printf("\tvar r%d %s = make(%s)\n", i, typ, typ)
				default:
					g.Printf("\tvar r%d %s\n", i, typ)
				}
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

	if len(res) == 0 {
		g.Printf("\t\t\treturn\n")
	} else {
		// if last result is error, return it
		if id, ok := fn.Type.Results.List[len(fn.Type.Results.List)-1].Type.(*ast.Ident); ok && id.Name == "error" {
			if len(fn.Type.Results.List) > 1 {
				g.Printf("\t\t\treturn %s, ErrTimeout\n", strings.Join(res[:len(res)-1], ", "))
			} else if len(fn.Type.Results.List) == 1 {
				g.Printf("\t\t\treturn ErrTimeout\n")
			}
		} else {
			g.Printf("\t\t\treturn %s\n", strings.Join(res, ", "))
		}
	}
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
