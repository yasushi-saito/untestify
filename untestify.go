// The eg command performs example-based refactoring.
// For documentation, run the command, or see Help in
// golang.org/x/tools/refactor/eg.
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
	helpFlag       = flag.Bool("help", false, "show detailed help message")
	transitiveFlag = flag.Bool("transitive", false, "apply refactoring to all dependencies too")
	verboseFlag    = flag.Bool("v", false, "show verbose matcher diagnostics")
)

func init() {
	flag.Var((*buildutil.TagsFlag)(&build.Default.BuildTags), "tags", buildutil.TagsFlagDoc)
}

const usage = `eg: an example-based refactoring tool.

Usage: eg -t template.go [-w] [-transitive] <args>...

-help            show detailed help message
-t template.go	 specifies the template file (use -help to see explanation)
-w          	 causes files to be re-written in place.
-transitive 	 causes all dependencies to be refactored too.
-v               show verbose matcher diagnostics
` + loader.FromArgsUsage

type substitution struct {
	signature, beforeBody, afterBody string
}

var substitutions = []substitution{
	{"t TT, err error ARGDECLS", "XX.NoError(t, err ARGS)", "YY.NoError(t, err ARGS)"},
	{"t TT, err error, f string ARGDECLS", "XX.NoErrorf(t, err, f ARGS)", "YY.NoError(t, err, f ARGS)"},
	{"t TT, err error ARGDECLS", "XX.Error(t, err ARGS)", "YY.NotNil(t, err ARGS)"},
	{"t TT, a interface{}, f string ARGDECLS", "XX.NotNilf(t, a, f ARGS)", "YY.NotNil(t, a, f ARGS)"},
	{"t TT, a interface{} ARGDECLS", "XX.Nil(t, a ARGS)", "YY.Nil(t, a ARGS)"},
	{"t TT, a, b interface{} ARGDECLS", "XX.Equal(t, a, b ARGS)", "YY.EQ(t, b, a ARGS)"},
	{"t TT, a, b interface{}, f string ARGDECLS", "XX.Equalf(t, a, b, f ARGS)", "YY.EQ(t, b, a, f ARGS)"},
	{"t TT, a, b interface{} ARGDECLS", "XX.Regexp(t, a, b ARGS)", "YY.Regexp(t, b, a ARGS)"},
	{"t TT, a bool ARGDECLS", "XX.True(t, a ARGS)", "YY.True(t, a ARGS)"},
	{"t TT, a bool  ARGDECLS", "XX.False(t, a ARGS)", "YY.False(t, a ARGS)"},
	{"t TT, a bool, f string  ARGDECLS", "XX.Truef(t, a, f ARGS)", "YY.True(t, a, f ARGS)"},
	{"t TT, a bool, f string ARGDECLS", "XX.Falsef(t, a, f ARGS)", "YY.False(t, a, f ARGS)"},
}

var templateSeq = 0

type rewriteType int

const (
	RewriteRequire rewriteType = iota
	RewriteAssert
)

func addTemplates(conf *loader.Config, rType rewriteType, subs []substitution) int {
	const dir = "/tmp/.templatestmp"
	os.Mkdir(dir, 0700) // nolint: errcheck

	n := 0
	for _, sub := range subs {
		var before, after, imports string
		switch rType {
		case RewriteAssert:
			before = strings.Replace(sub.beforeBody, "XX", "assert", -1)
			after = strings.Replace(sub.afterBody, "YY", "gexpect", -1)
			imports = `
 "vendor/github.com/stretchr/testify/assert"
gexpect "github.com/grailbio/testutil/expect"
`
		case RewriteRequire:
			before = strings.Replace(sub.beforeBody, "XX", "require", -1)
			after = strings.Replace(sub.afterBody, "YY", "gassert", -1)
			imports = `
 "vendor/github.com/stretchr/testify/require"
gassert "github.com/grailbio/testutil/assert"
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
			sig := strings.Replace(sub.signature, "ARGDECLS", arg.decl, -1)
			sig = strings.Replace(sig, "TT", "testing.TB", -1)
			before := strings.Replace(before, "ARGS", arg.arg, -1)
			after := strings.Replace(after, "ARGS", arg.arg, -1)
			pkgName := fmt.Sprintf("template%04d", templateSeq)
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

	doMain(args)
}

func doMain(args []string) {
	conf := loader.Config{
		Fset:       token.NewFileSet(),
		ParserMode: parser.ParseComments,
	}

	nTemplate := addTemplates(&conf, RewriteRequire, substitutions)
	nTemplate += addTemplates(&conf, RewriteAssert, substitutions)
	_, err := conf.FromArgs(args, true)
	if err != nil {
		log.Panic(err)
	}

	// Load, parse and type-check the whole program.
	iprog, err := conf.Load()
	if err != nil {
		log.Panic(err)
	}

	// Analyze the template.
	xforms := []*eg.Transformer{}
	templates := []*loader.PackageInfo{}
	for i := 0; i < nTemplate; i++ {
		template := iprog.Created[i]
		templates = append(templates, template)
		xform, err := eg.NewTransformer(iprog.Fset, template.Pkg, template.Files[0], &template.Info, *verboseFlag)
		if err != nil {
			log.Panic(err)
		}
		xforms = append(xforms, xform)
	}

	// Apply it to the input packages.
	var pkgs []*loader.PackageInfo
	if *transitiveFlag {
		for _, info := range iprog.AllPackages {
			pkgs = append(pkgs, info)
		}
	} else {
		pkgs = iprog.InitialPackages()
	}
	for _, pkg := range pkgs {
		isTemplate := false
		for _, t := range templates {
			if pkg == t {
				isTemplate = true
				break
			}
		}
		if strings.Index(pkg.String(), "template0") >= 0 {
			continue
		}
		if isTemplate {
			continue
		}
		fmt.Printf("Handel package %s\n", pkg.String())
		for _, file := range pkg.Files {
			n := 0
			for _, xform := range xforms {
				n += xform.Transform(&pkg.Info, pkg.Pkg, file)
			}
			filename := iprog.Fset.File(file.Pos()).Name()
			if true {
				j := 0
				for _, imp := range file.Imports {
					if strings.Index(imp.Path.Value, "/testify/require") >= 0 {
						continue
					}
					if strings.Index(imp.Path.Value, "/testify/assert") >= 0 {
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
			}
			if n == 0 {
				continue
			}
			fmt.Fprintf(os.Stderr, "=== %s (%d matches)\n", filename, n)
			if err := eg.WriteAST(iprog.Fset, filename, file); err != nil {
				log.Panic(err)
			}
		}
	}
}
