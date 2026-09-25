package main

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/types"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/tools/go/types/typeutil"
)

type respKind string

const (
	respJSON   respKind = "json"
	respNone   respKind = "none"   // 204
	respBinary respKind = "binary" // a file/audio body
)

type param struct {
	name string
	typ  types.Type
}

// route is one mux.HandleFunc registration, as its handler uses it.
type route struct {
	method, path string
	name         string // handleGetBook -> getBook
	doc          string
	pathParams   []param
	query        []param
	body         types.Type // decoded JSON body, nil if none
	multipart    []string   // file fields, for a multipart/form-data body
	kind         respKind
	resp         types.Type // for respJSON
	status       int
	errorCodes   []string
}

func (r *route) key() string { return r.method + " " + r.path }

var pathParamRe = regexp.MustCompile(`\{([A-Za-z0-9_]+)\}`)

// routes reads every mux.HandleFunc("<METHOD> <path>", s.<handler>) call
// in httpapi and analyzes each handler.
func (g *gen) routes() ([]*route, error) {
	p := g.pkgs[httpapiPkg]
	type reg struct {
		pattern string
		fn      *types.Func
	}
	var regs []reg
	for _, f := range p.Syntax {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "HandleFunc" || len(call.Args) != 2 {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			hsel, ok2 := call.Args[1].(*ast.SelectorExpr)
			if !ok || !ok2 {
				return true
			}
			pattern, _ := strconv.Unquote(lit.Value)
			if fn, ok := p.TypesInfo.Uses[hsel.Sel].(*types.Func); ok {
				regs = append(regs, reg{pattern, fn})
			}
			return true
		})
	}
	var out []*route
	for _, rg := range regs {
		fi, ok := g.funcs[rg.fn]
		if !ok {
			return nil, fmt.Errorf("route %s: handler %s has no source", rg.pattern, rg.fn.Name())
		}
		doc := fi.decl.Doc.Text()
		if strings.Contains(doc, "apigen:skip") {
			continue
		}
		method, path, ok := strings.Cut(rg.pattern, " ")
		if !ok {
			return nil, fmt.Errorf("route %q: pattern needs a method", rg.pattern)
		}
		r := &route{method: method, path: path, name: lowerFirst(strings.TrimPrefix(rg.fn.Name(), "handle")), doc: doc}
		a := &analysis{g: g, r: r, visited: map[*types.Func]bool{}, pathTypes: map[string]types.Type{}, queryTypes: map[string]types.Type{}}
		a.walk(fi)
		if err := a.finish(); err != nil {
			return nil, fmt.Errorf("route %s (%s): %w", rg.pattern, rg.fn.Name(), err)
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out, nil
}

type jsonResp struct {
	status int
	typ    types.Type
}

type analysis struct {
	g          *gen
	r          *route
	visited    map[*types.Func]bool
	pathTypes  map[string]types.Type
	queryTypes map[string]types.Type
	queryOrder []string
	jsonResps  []jsonResp
	noContent  bool
	binary     bool
	multipart  bool
}

// walk scans one function body, following calls into package-local
// helpers that are handed the ResponseWriter or Request.
func (a *analysis) walk(fi funcInfo) {
	info := fi.pkg.TypesInfo
	var stack []ast.Node
	ast.Inspect(fi.decl.Body, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return false
		}
		stack = append(stack, n)
		if call, ok := n.(*ast.CallExpr); ok {
			var parent ast.Node
			if len(stack) >= 2 {
				parent = stack[len(stack)-2]
			}
			a.call(info, call, parent)
		}
		return true
	})
}

func (a *analysis) call(info *types.Info, call *ast.CallExpr, parent ast.Node) {
	fn, _ := typeutil.Callee(info, call).(*types.Func)
	if fn == nil || fn.Pkg() == nil {
		return
	}
	fn = fn.Origin()
	pkg, name := fn.Pkg().Path(), fn.Name()
	switch {
	case pkg == httpapiPkg && name == "writeJSON":
		a.jsonResps = append(a.jsonResps, jsonResp{constInt(info, call.Args[1]), info.TypeOf(call.Args[2])})
	case pkg == httpapiPkg && name == "writeBuilt":
		a.jsonResps = append(a.jsonResps, jsonResp{200, info.TypeOf(call.Args[1])})
	case pkg == httpapiPkg && name == "writeNoContent":
		a.noContent = true
	case pkg == httpapiPkg && name == "writeErrorCode":
		if tv, ok := info.Types[call.Args[2]]; ok && tv.Value != nil {
			a.r.errorCodes = appendUnique(a.r.errorCodes, constant.StringVal(tv.Value))
		}
	case pkg == httpapiPkg && (name == "writeError" || name == "writeBuildError"):
	case pkg == "net/http" && (name == "ServeFile" || name == "ServeContent"):
		a.binary = true
	case pkg == "net/http" && name == "Write" && isRecv(fn, "net/http.ResponseWriter"):
		a.binary = true
	case pkg == "encoding/json" && name == "Decode" && decodesBody(info, call):
		a.r.body = deref(info.TypeOf(call.Args[0]))
	case pkg == "net/http" && name == "ParseMultipartForm":
		a.multipart = true
	case pkg == "net/http" && name == "FormFile":
		a.multipart = true
		if s, ok := constStr(info, call.Args[0]); ok {
			a.r.multipart = appendUnique(a.r.multipart, s)
		}
	case pkg == "net/http" && name == "PathValue":
		if s, ok := constStr(info, call.Args[0]); ok {
			a.pathTypes[s] = better(a.pathTypes[s], paramType(info, parent))
		}
	case pkg == "net/url" && name == "Get":
		if s, ok := constStr(info, call.Args[0]); ok {
			if _, seen := a.queryTypes[s]; !seen {
				a.queryOrder = append(a.queryOrder, s)
			}
			a.queryTypes[s] = better(a.queryTypes[s], paramType(info, parent))
		}
	case pkg == httpapiPkg:
		if a.visited[fn] || !passesRequest(info, call) {
			return
		}
		a.visited[fn] = true
		if fi, ok := a.g.funcs[fn]; ok {
			a.walk(fi)
		}
	}
}

// finish settles the route's response and params from what walk saw.
func (a *analysis) finish() error {
	r := a.r
	for _, m := range pathParamRe.FindAllStringSubmatch(r.path, -1) {
		t := a.pathTypes[m[1]]
		if t == nil {
			t = types.Typ[types.String]
		}
		r.pathParams = append(r.pathParams, param{m[1], t})
	}
	for _, q := range a.queryOrder {
		r.query = append(r.query, param{q, a.queryTypes[q]})
	}
	if a.multipart && r.body != nil {
		return fmt.Errorf("both a JSON and a multipart body")
	}
	var distinct []jsonResp
	for _, jr := range a.jsonResps {
		dup := false
		for _, d := range distinct {
			if types.Identical(d.typ, jr.typ) {
				dup = true
			}
		}
		if !dup {
			distinct = append(distinct, jr)
		}
	}
	switch {
	case len(distinct) > 1:
		var names []string
		for _, d := range distinct {
			names = append(names, d.typ.String())
		}
		return fmt.Errorf("responds with more than one JSON type (%s) - give it one response shape", strings.Join(names, ", "))
	case len(distinct) == 1 && (a.noContent || a.binary):
		return fmt.Errorf("mixes a JSON response with a 204/binary one")
	case len(distinct) == 1:
		r.kind, r.resp, r.status = respJSON, distinct[0].typ, distinct[0].status
		if err := a.g.visitType("response of "+r.key(), r.resp); err != nil {
			return err
		}
	case a.binary && a.noContent:
		return fmt.Errorf("mixes a binary response with a 204")
	case a.binary:
		r.kind = respBinary
	case a.noContent:
		r.kind = respNone
	default:
		return fmt.Errorf("no response found (writeJSON/writeBuilt/writeNoContent/a file)")
	}
	if r.body != nil {
		if err := a.g.visitType("body of "+r.key(), r.body); err != nil {
			return err
		}
	}
	for _, ps := range [][]param{r.pathParams, r.query} {
		for _, p := range ps {
			if err := a.g.visitType("param "+p.name+" of "+r.key(), p.typ); err != nil {
				return err
			}
		}
	}
	return nil
}

// paramType is the type a PathValue/Query().Get result is used as: the
// named type it's converted to, int if it goes straight into
// strconv.Atoi, string otherwise.
func paramType(info *types.Info, parent ast.Node) types.Type {
	if call, ok := parent.(*ast.CallExpr); ok {
		if tv, ok := info.Types[call.Fun]; ok && tv.IsType() {
			return tv.Type
		}
		if fn, ok := typeutil.Callee(info, call).(*types.Func); ok && fn.Pkg() != nil && fn.Pkg().Path() == "strconv" && fn.Name() == "Atoi" {
			return types.Typ[types.Int]
		}
	}
	return types.Typ[types.String]
}

// better keeps the more specific of two observed param types.
func better(prev, next types.Type) types.Type {
	if prev == nil {
		return next
	}
	if b, ok := prev.(*types.Basic); ok && b.Kind() == types.String {
		return next
	}
	return prev
}

func constInt(info *types.Info, e ast.Expr) int {
	if tv, ok := info.Types[e]; ok && tv.Value != nil {
		if v, ok := constant.Int64Val(tv.Value); ok {
			return int(v)
		}
	}
	return 0
}

func constStr(info *types.Info, e ast.Expr) (string, bool) {
	if tv, ok := info.Types[e]; ok && tv.Value != nil && tv.Value.Kind() == constant.String {
		return constant.StringVal(tv.Value), true
	}
	return "", false
}

func deref(t types.Type) types.Type {
	if p, ok := t.(*types.Pointer); ok {
		return p.Elem()
	}
	return t
}

func isRecv(fn *types.Func, typ string) bool {
	sig, ok := fn.Type().(*types.Signature)
	return ok && sig.Recv() != nil && types.TypeString(sig.Recv().Type(), nil) == typ
}

// decodesBody reports whether call is json.NewDecoder(r.Body).Decode(...).
func decodesBody(info *types.Info, call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	inner, ok := sel.X.(*ast.CallExpr)
	if !ok || len(inner.Args) != 1 {
		return false
	}
	fn, ok := typeutil.Callee(info, inner).(*types.Func)
	if !ok || fn.Name() != "NewDecoder" {
		return false
	}
	arg, ok := inner.Args[0].(*ast.SelectorExpr)
	return ok && arg.Sel.Name == "Body"
}

// passesRequest reports whether a call hands on the handler's
// ResponseWriter or Request - a helper worth following.
func passesRequest(info *types.Info, call *ast.CallExpr) bool {
	for _, arg := range call.Args {
		switch types.TypeString(info.TypeOf(arg), nil) {
		case "net/http.ResponseWriter", "*net/http.Request":
			return true
		}
	}
	return false
}
