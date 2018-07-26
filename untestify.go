// To use this tool, you must modify golang.org/x/tools/refactor/eg/eg.go and comment out line 233:
//
// if types.AssignableTo(Tb, Ta) {
// 	// safe: replacement is assignable to pattern.
// } else if tuple, ok := Tb.(*types.Tuple); ok && tuple.Len() == 0 {
// 	// safe: pattern has void type (must appear in an ExprStmt).
// } else {
// 	return nil, fmt.Errorf("%s is not a safe replacement for %s", Ta, Tb)  <<<< comment out this line
// }
//
package main

import (
	"flag"
	"fmt"
	"go/build"
	"go/parser"
	"go/token"
	"os"

	"github.com/grailbio/base/log"
	"go/ast"
	"golang.org/x/tools/go/buildutil"
	"golang.org/x/tools/go/loader"
	"golang.org/x/tools/refactor/eg"
	"io/ioutil"
	"path/filepath"
	"strings"
)

var (
	helpFlag    = flag.Bool("help", false, "show detailed help message")
	verboseFlag = flag.Bool("v", false, "show verbose matcher diagnostics")
)

func init() {
	flag.Var((*buildutil.TagsFlag)(&build.Default.BuildTags), "tags", buildutil.TagsFlagDoc)
}

const usage = `untestify: convert stretcher/testify to grailbio.com/testutil.

Usage: untestify [flags] packages...

-help            show detailed help message
-v               show verbose matcher diagnostics
` + loader.FromArgsUsage

type substitution struct {
	signature, beforeBody, afterBody string
}

var substitutions = []substitution{
	{"t TT, err error DECLS", "XX.NoError(t, err ARGS)", "YY.NoError(t, err ARGS)"},
	{"t TT, err error, f string DECLS", "XX.NoErrorf(t, err, f ARGS)", "YY.NoError(t, err, f ARGS)"},
	{"t TT, err error DECLS", "XX.Error(t, err ARGS)", "YY.NotNil(t, err ARGS)"},
	{"t TT, err error, a string DECLS", "XX.EqualError(t, err, a ARGS)", "YY.EQ(t, err, a ARGS)"},
	{"t TT, err error, a, f string DECLS", "XX.EqualErrorf(t, err, a, f ARGS)", "YY.EQ(t, err, a, f ARGS)"},
	{"t TT, a interface{}, f string DECLS", "XX.NotNilf(t, a, f ARGS)", "YY.NotNil(t, a, f ARGS)"},
	{"t TT, a interface{} DECLS", "XX.Nil(t, a ARGS)", "YY.Nil(t, a ARGS)"},
	{"t TT, a, b interface{} DECLS", "XX.Equal(t, a, b ARGS)", "YY.EQ(t, b, a ARGS)"},
	{"t TT, a, b interface{}, f string DECLS", "XX.Equalf(t, a, b, f ARGS)", "YY.EQ(t, b, a, f ARGS)"},
	{"t TT, a, b interface{} DECLS", "XX.NotEqual(t, a, b ARGS)", "YY.EQ(t, b, a ARGS)"},
	{"t TT, a, b interface{}, f string DECLS", "XX.NotEqualf(t, a, b, f ARGS)", "YY.NEQ(t, b, a, f ARGS)"},
	{"t TT, a, b interface{} DECLS", "XX.Regexp(t, a, b ARGS)", "YY.Regexp(t, b, a ARGS)"},
	{"t TT, a bool DECLS", "XX.True(t, a ARGS)", "YY.True(t, a ARGS)"},
	{"t TT, a bool  DECLS", "XX.False(t, a ARGS)", "YY.False(t, a ARGS)"},
	{"t TT, a bool, f string  DECLS", "XX.Truef(t, a, f ARGS)", "YY.True(t, a, f ARGS)"},
	{"t TT, a bool, f string DECLS", "XX.Falsef(t, a, f ARGS)", "YY.False(t, a, f ARGS)"},
	{"t TT, a, b interface{} DECLS", "XX.Contains(t, a, b ARGS)", "YY.That(t, a, h.Contains(b) ARGS)"},
	{"t TT, a, b interface{}, f string DECLS", "XX.Containsf(t, a, b, f ARGS)", "YY.That(t, a, h.Contains(b), f ARGS)"},
	{"t TT, a interface{} DECLS", "XX.Zero(t, a ARGS)", "YY.That(t, a, h.Zero() ARGS)"},
	{"t TT, a interface{}, f string DECLS", "XX.Zerof(t, a, f ARGS)", "YY.EQ(t, a, h.Zero(), f ARGS)"},
	{"t TT, a interface{} DECLS", "XX.NotZero(t, a ARGS)", "YY.That(t, a, h.Not(h.Zero()) ARGS)"},
	{"t TT, a interface{}, f string DECLS", "XX.NotZerof(t, a, f ARGS)", "YY.EQ(t, a, h.Not(h.Zero()), f ARGS)"},
}

var templateSeq = 0

type rewriteType int

const (
	rewriteRequire rewriteType = iota
	rewriteAssert
)

func addTemplates(conf *loader.Config, rType rewriteType, subs []substitution) int {
	const dir = "/tmp/.templatestmp"
	os.Mkdir(dir, 0700) // nolint: errcheck

	n := 0
	for _, sub := range subs {
		var before, after, imports string
		switch rType {
		case rewriteAssert:
			before = strings.Replace(sub.beforeBody, "XX", "assert", -1)
			after = strings.Replace(sub.afterBody, "YY", "gexpect", -1)
			imports = `
 "vendor/github.com/stretchr/testify/assert"
gexpect "github.com/grailbio/testutil/expect"
`
		case rewriteRequire:
			before = strings.Replace(sub.beforeBody, "XX", "require", -1)
			after = strings.Replace(sub.afterBody, "YY", "gassert", -1)
			imports = `
 "vendor/github.com/stretchr/testify/require"
gassert "github.com/grailbio/testutil/assert"
`
		}

		if strings.Contains(after, "h.") {
			imports += `
"github.com/grailbio/testutil/h"
`
		}
		type argList struct {
			decl, arg string
		}

		for _, arg := range []argList{
			{"", ""},
			{", m0 interface{}", ", m0"},
			{", m0, m1 interface{}", ", m0, m1"},
			{", m0, m1, m2 interface{}", ", m0, m1, m2"},
			{", m0, m1, m2, m3 interface{}", ", m0, m1, m2, m3"},
			{", m0, m1, m2, m3, m4 interface{}", ", m0, m1, m2, m3, m4"},
		} {
			sig := strings.Replace(sub.signature, "DECLS", arg.decl, -1)
			sig = strings.Replace(sig, "TT", "testing.TB", -1)
			before := strings.Replace(before, "ARGS", arg.arg, -1)
			after := strings.Replace(after, "ARGS", arg.arg, -1)
			pkgName := fmt.Sprintf("template%08d", templateSeq)
			templateSeq++
			path := filepath.Join(dir, pkgName+".go")
			body := fmt.Sprintf(`
package %s
import (
	"testing"
  %s
)

func before(%s) { %s }
func after(%s) { %s }
`, pkgName, imports, sig, before, sig, after)
			if err := ioutil.WriteFile(path, []byte(body), 0600); err != nil {
				log.Panic(err)
			}
			conf.CreateFromFilenames(pkgName, path)
			n++
		}
	}
	return n
}

func rewriteImports(file *ast.File) int {
	n := 0
	j := 0
	for _, imp := range file.Imports {
		if strings.Contains(imp.Path.Value, "/testify/require") {
			continue
		}
		if strings.Contains(imp.Path.Value, "/testify/assert") {
			continue
		}
		file.Imports[j] = imp
		j++
	}
	if j != len(file.Imports) {
		file.Imports = file.Imports[:j]
		n++
	}

	for _, d := range file.Decls {
		d, ok := d.(*ast.GenDecl)
		if ok && d.Tok == token.IMPORT {
			j = 0
			for _, x := range d.Specs {
				imp := x.(*ast.ImportSpec)
				if strings.Index(imp.Path.Value, "/testify/require") >= 0 {
					continue
				}
				if strings.Index(imp.Path.Value, "/testify/assert") >= 0 {
					continue
				}
				if strings.Index(imp.Path.Value, "github.com/grailbio/testutil/expect") >= 0 {
					tmp := ast.Ident{}
					tmp.Name = "gexpect"
					imp.Name = &tmp
				}
				if strings.Index(imp.Path.Value, "github.com/grailbio/testutil/assert") >= 0 {
					tmp := ast.Ident{}
					tmp.Name = "gassert"
					imp.Name = &tmp
				}
				d.Specs[j] = x
				j++
			}
			if j != len(d.Specs) {
				d.Specs = d.Specs[:j]
				n++
			}
		}
	}
	return n
}

func main() {
	flag.Parse()
	args := flag.Args()

	if *helpFlag {
		fmt.Fprint(os.Stderr, eg.Help)
		os.Exit(2)
	}

	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
	}

	conf := loader.Config{
		Fset:       token.NewFileSet(),
		ParserMode: parser.ParseComments,
	}

	nTemplate := addTemplates(&conf, rewriteRequire, substitutions)
	nTemplate += addTemplates(&conf, rewriteAssert, substitutions)
	_, err := conf.FromArgs(args, true)
	if err != nil {
		log.Panic(err)
	}

	iprog, err := conf.Load()
	if err != nil {
		log.Panic(err)
	}

	xforms := []*eg.Transformer{}
	for i := 0; i < nTemplate; i++ {
		template := iprog.Created[i]
		xform, err := eg.NewTransformer(iprog.Fset, template.Pkg, template.Files[0], &template.Info, *verboseFlag)
		if err != nil {
			log.Panic(err)
		}
		xforms = append(xforms, xform)
	}

	for _, pkg := range iprog.InitialPackages() {
		if strings.Contains(pkg.String(), "template000") {
			continue
		}
		fmt.Fprintf(os.Stderr, "=== Package %s (%d files)\n", pkg.String(), len(pkg.Files))
		for _, file := range pkg.Files {
			n := 0
			for _, xform := range xforms {
				n += xform.Transform(&pkg.Info, pkg.Pkg, file)
			}
			n += rewriteImports(file)
			if n == 0 {
				continue
			}
			filename := iprog.Fset.File(file.Pos()).Name()
			fmt.Fprintf(os.Stderr, "=== %s (%d matches)\n", filename, n)
			if err := eg.WriteAST(iprog.Fset, filename, file); err != nil {
				log.Panic(err)
			}
		}
	}
}
