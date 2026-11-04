// floatlint is a go/analysis checker that forbids floating-point values in
// the monetary packages listed in tools/floatlint/config.yaml (FIN-01).
//
// It reports:
//   - the predeclared float32 and float64 types,
//   - floating-point literals,
//   - strconv.ParseFloat,
//   - math.Round and math.Floor.
//
// A finding on a line that also carries a valid
// "//floatlint:allow <reason>" comment is suppressed. The directive is
// line-scoped and the reason must be non-empty; an empty reason is reported
// as a diagnostic and does not suppress the original finding.
package main

import (
	"go/ast"
	"go/token"
	"go/types"
	"strings"
	"sync"

	"golang.org/x/tools/go/analysis"
)

const analyzerDoc = `floatlint reports floating-point usage in monetary packages.

The packages it enforces are listed in tools/floatlint/config.yaml. It flags
float32, float64, floating-point literals, strconv.ParseFloat, math.Round, and
math.Floor because monetary amounts must be int64 minor units or an exact
decimal type (FIN-01).

A finding is suppressed only by a "//floatlint:allow <reason>" comment on the
same line. The reason is mandatory; an empty reason is itself a diagnostic.`

// New returns the floatlint analyzer for the supplied configuration. It is
// used by tests, where the configuration does not come from a file.
func New(cfg Config) *analysis.Analyzer {
	return newAnalyzer(func() (Config, error) { return cfg, nil })
}

// NewCommand returns the analyzer used by the command. It registers a -config
// flag and loads that file lazily on the first package analysis, after the
// analysis driver has parsed the command line.
func NewCommand(defaultConfigPath string) *analysis.Analyzer {
	configPath := defaultConfigPath
	a := newAnalyzer(func() (Config, error) { return LoadConfig(configPath) })
	a.Flags.StringVar(&configPath, "config", defaultConfigPath, "path to the floatlint configuration file")
	return a
}

func newAnalyzer(load func() (Config, error)) *analysis.Analyzer {
	var once sync.Once
	var cfg Config
	var loadErr error

	return &analysis.Analyzer{
		Name: "floatlint",
		Doc:  analyzerDoc,
		Run: func(pass *analysis.Pass) (any, error) {
			once.Do(func() { cfg, loadErr = load() })
			if loadErr != nil {
				return nil, loadErr
			}
			if !cfg.Applies(pass.Pkg.Path()) {
				return nil, nil
			}
			run(pass)
			return nil, nil
		},
	}
}

func run(pass *analysis.Pass) {
	allow := collectAllowDirectives(pass)

	for line, pos := range allow.invalid {
		pass.Reportf(pos, "//floatlint:allow requires a non-empty reason on line %d", line)
	}

	for _, file := range pass.Files {
		ast.Inspect(file, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.Ident:
				if isPredeclaredFloatType(pass.TypesInfo, n) {
					report(pass, allow, n.Pos(), "use of %s is not allowed in a monetary package (FIN-01); use int64 minor units or an exact decimal type", n.Name)
				}
			case *ast.BasicLit:
				if n.Kind == token.FLOAT {
					report(pass, allow, n.Pos(), "floating-point literal is not allowed in a monetary package (FIN-01); use an exact decimal type")
				}
			case *ast.CallExpr:
				if name, ok := forbiddenCall(pass.TypesInfo, n); ok {
					report(pass, allow, n.Pos(), "%s is not allowed in a monetary package (FIN-01); use exact integer or decimal arithmetic", name)
				}
			}
			return true
		})
	}
}

func report(pass *analysis.Pass, allow allowDirectives, pos token.Pos, format string, args ...any) {
	line := pass.Fset.Position(pos).Line
	if _, ok := allow.reasons[line]; ok {
		return
	}
	pass.Reportf(pos, format, args...)
}

// allowDirectives maps a source line to its directive. reasons holds valid
// directives; invalid holds directives whose reason was empty.
type allowDirectives struct {
	reasons map[int]string
	invalid map[int]token.Pos
}

func collectAllowDirectives(pass *analysis.Pass) allowDirectives {
	allow := allowDirectives{
		reasons: make(map[int]string),
		invalid: make(map[int]token.Pos),
	}
	for _, file := range pass.Files {
		for _, group := range file.Comments {
			for _, comment := range group.List {
				reason, present := parseAllowDirective(comment.Text)
				if !present {
					continue
				}
				line := pass.Fset.Position(comment.Slash).Line
				if reason == "" {
					allow.invalid[line] = comment.Slash
					continue
				}
				allow.reasons[line] = reason
			}
		}
	}
	return allow
}

// parseAllowDirective recognizes the line comment form
// "//floatlint:allow <reason>". Whitespace after "//" and before the reason is
// allowed; the reason must be non-empty and is returned trimmed. A block
// comment is not a directive.
func parseAllowDirective(text string) (reason string, present bool) {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "//") {
		return "", false
	}
	body := strings.TrimSpace(trimmed[2:])
	const directive = "floatlint:allow"
	if !strings.HasPrefix(body, directive) {
		return "", false
	}
	rest := body[len(directive):]
	if rest != "" && !strings.ContainsRune(" \t\r\f\v", rune(rest[0])) {
		// "floatlint:allowed" is a different word, not a directive.
		return "", false
	}
	return strings.TrimSpace(rest), true
}

// isPredeclaredFloatType reports whether id denotes the predeclared float32
// or float64 type. Type information is used so a shadowed identifier is not
// mistaken for the builtin.
func isPredeclaredFloatType(info *types.Info, id *ast.Ident) bool {
	if id.Name != "float32" && id.Name != "float64" {
		return false
	}
	obj := info.Uses[id]
	if obj == nil {
		obj = info.Defs[id]
	}
	typeName, ok := obj.(*types.TypeName)
	if !ok || typeName.Pkg() != nil {
		return false
	}
	basic, ok := typeName.Type().(*types.Basic)
	if !ok {
		return false
	}
	return basic.Kind() == types.Float32 || basic.Kind() == types.Float64
}

// forbiddenCall identifies calls to strconv.ParseFloat, math.Round, and
// math.Floor. Resolving the selector through type information handles import
// aliases and avoids flagging user-defined methods with the same name.
func forbiddenCall(info *types.Info, call *ast.CallExpr) (string, bool) {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	fn, ok := info.Uses[selector.Sel].(*types.Func)
	if !ok || fn.Pkg() == nil {
		return "", false
	}
	name := fn.Name()
	switch fn.Pkg().Path() {
	case "strconv":
		if name == "ParseFloat" {
			return "strconv.ParseFloat", true
		}
	case "math":
		if name == "Round" || name == "Floor" {
			return "math." + name, true
		}
	}
	return "", false
}
