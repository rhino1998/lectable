package main

import (
	"fmt"
	"go/ast"
	"go/types"
	"strconv"
	"strings"
)

// topicParam is one param a live topic takes on the wire.
type topicParam struct {
	name string
	typ  string // "string" or "int"
}

// liveTopic is one httpapi.registerLiveTopics registration.
type liveTopic struct {
	name   string
	doc    string
	params []topicParam
	data   types.Type
}

// liveTopics reads httpapi.registerLiveTopics: each
// h.Register("<name>", <wrapper>(..., build)) call, whose wrapper's doc
// line "apigen:topic-params <name>:<string|int>..." names its params and
// whose type argument is the topic's value type.
func (g *gen) liveTopics() ([]liveTopic, error) {
	obj, err := g.lookup(httpapiPkg, "Server")
	if err != nil {
		return nil, err
	}
	var reg *types.Func
	ms := types.NewMethodSet(types.NewPointer(obj.Type()))
	for i := 0; i < ms.Len(); i++ {
		if f := ms.At(i).Obj().(*types.Func); f.Name() == "registerLiveTopics" {
			reg = f
		}
	}
	fi, ok := g.funcs[reg]
	if reg == nil || !ok {
		return nil, fmt.Errorf("httpapi.Server.registerLiveTopics not found")
	}
	info := fi.pkg.TypesInfo
	var topics []liveTopic
	var walkErr error
	ast.Inspect(fi.decl.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || walkErr != nil {
			return walkErr == nil
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Register" || len(call.Args) != 2 {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok {
			walkErr = fmt.Errorf("live topic name must be a string literal")
			return false
		}
		name, _ := strconv.Unquote(lit.Value)
		wcall, ok := call.Args[1].(*ast.CallExpr)
		if !ok {
			walkErr = fmt.Errorf("live topic %s: resolver must be a wrapper call", name)
			return false
		}
		wid := wrapperIdent(wcall.Fun)
		if wid == nil {
			walkErr = fmt.Errorf("live topic %s: unrecognized wrapper", name)
			return false
		}
		inst, ok := info.Instances[wid]
		wfn, _ := info.Uses[wid].(*types.Func)
		if !ok || inst.TypeArgs.Len() != 1 || wfn == nil {
			walkErr = fmt.Errorf("live topic %s: wrapper %s must be generic over the topic's value type", name, wid.Name)
			return false
		}
		params, err := g.topicParams(wfn)
		if err != nil {
			walkErr = fmt.Errorf("live topic %s: %w", name, err)
			return false
		}
		t := liveTopic{name: name, params: params, data: inst.TypeArgs.At(0)}
		// The builder's doc, when it's a named method.
		if bsel, ok := wcall.Args[len(wcall.Args)-1].(*ast.SelectorExpr); ok {
			if bfn, ok := info.Uses[bsel.Sel].(*types.Func); ok {
				if bfi, ok := g.funcs[bfn]; ok {
					t.doc = bfi.decl.Doc.Text()
				}
			}
		}
		if err := g.visitType("live topic "+name, t.data); err != nil {
			walkErr = err
			return false
		}
		topics = append(topics, t)
		return true
	})
	return topics, walkErr
}

func wrapperIdent(e ast.Expr) *ast.Ident {
	switch e := e.(type) {
	case *ast.Ident:
		return e
	case *ast.IndexExpr:
		return wrapperIdent(e.X)
	case *ast.IndexListExpr:
		return wrapperIdent(e.X)
	}
	return nil
}

func (g *gen) topicParams(wrapper *types.Func) ([]topicParam, error) {
	fi, ok := g.funcs[wrapper.Origin()]
	if !ok {
		return nil, fmt.Errorf("wrapper %s has no source", wrapper.Name())
	}
	for _, line := range strings.Split(fi.decl.Doc.Text(), "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "apigen:topic-params")
		if !ok {
			continue
		}
		var ps []topicParam
		for _, f := range strings.Fields(rest) {
			name, typ, ok := strings.Cut(f, ":")
			if !ok || (typ != "string" && typ != "int") {
				return nil, fmt.Errorf("wrapper %s: bad topic param %q", wrapper.Name(), f)
			}
			ps = append(ps, topicParam{name: name, typ: typ})
		}
		return ps, nil
	}
	return nil, fmt.Errorf("wrapper %s has no apigen:topic-params doc line", wrapper.Name())
}
