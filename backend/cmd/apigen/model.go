package main

import (
	"fmt"
	"go/ast"
	"go/types"
	"reflect"
	"sort"
	"strings"
	"unicode"

	"golang.org/x/tools/go/packages"
)

type field struct {
	json     string
	doc      string
	optional bool // omitempty/omitzero: may be absent on the wire
	typ      types.Type
	tsType   string // tstype override, "" if none
	ktType   string // kttype override, "" if none
}

type decl struct {
	name   string // generated TS name
	doc    string
	embeds []string // TS names of embedded structs (flattened on the wire)
	fields []field  // own fields, not counting embeds
	enum   []string // wire values, in declaration order, for a string enum
	isEnum bool
	tsHand bool // doc says apigen:ts-hand: emitted for Kotlin only
	obj    *types.TypeName
}

// funcInfo is a function's syntax and the package it was type-checked in.
type funcInfo struct {
	decl *ast.FuncDecl
	pkg  *packages.Package
}

type gen struct {
	pkgs      map[string]*packages.Package
	funcs     map[*types.Func]funcInfo
	docs      map[*types.TypeName]string
	fieldDocs map[*types.Var]string
	consts    map[*types.TypeName][]string
	decls     map[*types.TypeName]*decl
	byName    map[string]*types.TypeName
	queue     []*types.Named
}

func newGen() *gen {
	return &gen{
		pkgs:      map[string]*packages.Package{},
		funcs:     map[*types.Func]funcInfo{},
		docs:      map[*types.TypeName]string{},
		fieldDocs: map[*types.Var]string{},
		consts:    map[*types.TypeName][]string{},
		decls:     map[*types.TypeName]*decl{},
		byName:    map[string]*types.TypeName{},
	}
}

// nameOverrides renames a Go type whose derived name would collide with
// something else generated (emotions.Emotion vs the Emotion id union).
var nameOverrides = map[string]string{
	emotionsPkg + ".Emotion": "EmotionInfo",
}

// index records doc comments, functions, and string constants from one
// package's syntax, so they're available whichever package a referenced
// type lives in.
func (g *gen) index(p *packages.Package) {
	g.pkgs[p.PkgPath] = p
	if p.TypesInfo == nil {
		return
	}
	for _, f := range p.Syntax {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok {
				if fn, ok := p.TypesInfo.Defs[fd.Name].(*types.Func); ok {
					g.funcs[fn] = funcInfo{decl: fd, pkg: p}
				}
				continue
			}
			gd, ok := d.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range gd.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					tn, _ := p.TypesInfo.Defs[s.Name].(*types.TypeName)
					if tn == nil {
						continue
					}
					doc := s.Doc
					if doc == nil && len(gd.Specs) == 1 {
						doc = gd.Doc
					}
					g.docs[tn] = doc.Text()
					if st, ok := s.Type.(*ast.StructType); ok {
						g.indexFields(p, st)
					}
				case *ast.ValueSpec:
					for _, n := range s.Names {
						c, ok := p.TypesInfo.Defs[n].(*types.Const)
						if !ok {
							continue
						}
						named, ok := c.Type().(*types.Named)
						if !ok || named.Obj().Pkg() != c.Pkg() {
							continue
						}
						if b, ok := named.Underlying().(*types.Basic); ok && b.Info()&types.IsString != 0 {
							g.consts[named.Obj()] = appendUnique(g.consts[named.Obj()], constString(c))
						}
					}
				}
			}
		}
	}
}

func appendUnique(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

func (g *gen) indexFields(p *packages.Package, st *ast.StructType) {
	for _, f := range st.Fields.List {
		text := f.Doc.Text()
		if f.Comment != nil {
			if text != "" {
				text += "\n"
			}
			text += f.Comment.Text()
		}
		for _, n := range f.Names {
			if v, ok := p.TypesInfo.Defs[n].(*types.Var); ok {
				g.fieldDocs[v] = text
			}
		}
		if inner, ok := f.Type.(*ast.StructType); ok {
			g.indexFields(p, inner)
		}
	}
}

func constString(c *types.Const) string {
	s := c.Val().ExactString()
	return strings.Trim(s, `"`)
}

// lookup finds a package-level object by package path and name.
func (g *gen) lookup(pkgPath, name string) (types.Object, error) {
	p, ok := g.pkgs[pkgPath]
	if !ok {
		return nil, fmt.Errorf("package %s not loaded", pkgPath)
	}
	obj := p.Types.Scope().Lookup(name)
	if obj == nil {
		return nil, fmt.Errorf("%s.%s not found", pkgPath, name)
	}
	return obj, nil
}

func hasJSONField(st *types.Struct) bool {
	for i := 0; i < st.NumFields(); i++ {
		if _, ok := reflect.StructTag(st.Tag(i)).Lookup("json"); ok {
			return true
		}
	}
	return false
}

func (g *gen) want(n *types.Named) {
	if _, ok := g.decls[n.Obj()]; ok {
		return
	}
	g.decls[n.Obj()] = nil // reserved; filled by drain
	g.queue = append(g.queue, n)
}

func (g *gen) drain() error {
	for len(g.queue) > 0 {
		n := g.queue[0]
		g.queue = g.queue[1:]
		d, err := g.build(n)
		if err != nil {
			return err
		}
		if prev, ok := g.byName[d.name]; ok {
			return fmt.Errorf("generated name %s is used by both %s and %s", d.name, prev, n.Obj())
		}
		g.byName[d.name] = n.Obj()
		g.decls[n.Obj()] = d
	}
	return nil
}

func tsName(obj *types.TypeName) string {
	if obj.Pkg() != nil {
		if n, ok := nameOverrides[obj.Pkg().Path()+"."+obj.Name()]; ok {
			return n
		}
	}
	name := obj.Name()
	for _, suf := range []string{"DTOOut", "DTO"} {
		name = strings.TrimSuffix(name, suf)
	}
	return upperFirst(name)
}

func upperFirst(s string) string {
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

func lowerFirst(s string) string {
	r := []rune(s)
	r[0] = unicode.ToLower(r[0])
	return string(r)
}

// upperSnake turns a Go/TS identifier or a wire value into
// UPPER_SNAKE_CASE: SFXEngine -> SFX_ENGINE, instruct_all -> INSTRUCT_ALL.
func upperSnake(s string) string {
	r := []rune(s)
	var b strings.Builder
	for i, c := range r {
		if !unicode.IsLetter(c) && !unicode.IsDigit(c) {
			if b.Len() > 0 && !strings.HasSuffix(b.String(), "_") {
				b.WriteByte('_')
			}
			continue
		}
		if i > 0 && unicode.IsUpper(c) {
			prev := r[i-1]
			nextLower := i+1 < len(r) && unicode.IsLower(r[i+1])
			if unicode.IsLower(prev) || unicode.IsDigit(prev) || (unicode.IsUpper(prev) && nextLower) {
				if !strings.HasSuffix(b.String(), "_") {
					b.WriteByte('_')
				}
			}
		}
		b.WriteRune(unicode.ToUpper(c))
	}
	return strings.TrimSuffix(b.String(), "_")
}

// plural pluralizes an English noun well enough for type names.
func plural(s string) string {
	switch {
	case strings.HasSuffix(s, "s"), strings.HasSuffix(s, "x"), strings.HasSuffix(s, "sh"), strings.HasSuffix(s, "ch"):
		return s + "es"
	case strings.HasSuffix(s, "y") && len(s) > 1 && !strings.ContainsRune("aeiou", rune(s[len(s)-2])):
		return s[:len(s)-1] + "ies"
	}
	return s + "s"
}

func hasMarshaler(t types.Type) bool {
	for _, tt := range []types.Type{t, types.NewPointer(t)} {
		ms := types.NewMethodSet(tt)
		for i := 0; i < ms.Len(); i++ {
			if ms.At(i).Obj().Name() == "MarshalJSON" {
				return true
			}
		}
	}
	return false
}

func (g *gen) build(n *types.Named) (*decl, error) {
	obj := n.Obj()
	d := &decl{name: tsName(obj), doc: g.docs[obj], obj: obj}
	d.tsHand = strings.Contains(d.doc, "apigen:ts-hand")
	if hasMarshaler(n) {
		return nil, fmt.Errorf("%s has a custom MarshalJSON; give every field using it tstype/kttype tags", obj)
	}
	switch u := n.Underlying().(type) {
	case *types.Basic:
		if u.Info()&types.IsString == 0 || len(g.consts[obj]) == 0 {
			return nil, fmt.Errorf("%s: only string enums (named string types with constants) get their own declaration", obj)
		}
		d.isEnum = true
		d.enum = g.consts[obj]
		return d, nil
	case *types.Struct:
		fs, embeds, err := g.structFields(obj.String(), u)
		if err != nil {
			return nil, err
		}
		d.fields, d.embeds = fs, embeds
		return d, nil
	default:
		return nil, fmt.Errorf("%s: unsupported underlying type %s", obj, u)
	}
}

func (g *gen) structFields(owner string, st *types.Struct) ([]field, []string, error) {
	var fs []field
	var embeds []string
	for i := 0; i < st.NumFields(); i++ {
		v := st.Field(i)
		tag := reflect.StructTag(st.Tag(i))
		jsonTag, hasTag := tag.Lookup("json")
		name, opts, _ := strings.Cut(jsonTag, ",")
		if name == "-" && opts == "" {
			continue
		}
		if v.Embedded() && !hasTag {
			named, ok := v.Type().(*types.Named)
			if !ok {
				return nil, nil, fmt.Errorf("%s: unsupported embedded field %s", owner, v.Name())
			}
			if _, ok := named.Underlying().(*types.Struct); !ok {
				return nil, nil, fmt.Errorf("%s: embedded non-struct %s", owner, v.Name())
			}
			g.want(named)
			embeds = append(embeds, tsName(named.Obj()))
			continue
		}
		if !v.Exported() {
			continue
		}
		if name == "" {
			name = v.Name()
		}
		f := field{
			json:     name,
			doc:      g.fieldDocs[v],
			typ:      v.Type(),
			optional: strings.Contains(","+opts+",", ",omitempty,") || strings.Contains(","+opts+",", ",omitzero,"),
			tsType:   tag.Get("tstype"),
			ktType:   tag.Get("kttype"),
		}
		if strings.Contains(","+opts+",", ",string,") {
			f.typ = types.Typ[types.String]
		}
		if f.tsType == "" || f.ktType == "" {
			if err := g.visitType(owner+"."+v.Name(), f.typ); err != nil {
				return nil, nil, err
			}
		}
		fs = append(fs, f)
	}
	return fs, embeds, nil
}

// visitType enqueues every named type t references, and rejects shapes the
// renderers can't express.
func (g *gen) visitType(where string, t types.Type) error {
	if special(t) != "" {
		return nil
	}
	switch t := types.Unalias(t).(type) {
	case *types.Basic:
		return nil
	case *types.Pointer:
		return g.visitType(where, t.Elem())
	case *types.Slice:
		return g.visitType(where, t.Elem())
	case *types.Array:
		return g.visitType(where, t.Elem())
	case *types.Map:
		if b, ok := t.Key().Underlying().(*types.Basic); !ok || b.Info()&types.IsString == 0 {
			return fmt.Errorf("%s: map keys must be strings", where)
		}
		return g.visitType(where, t.Elem())
	case *types.Interface:
		return nil
	case *types.Struct:
		return fmt.Errorf("%s: anonymous struct; give it a name", where)
	case *types.Named:
		if _, ok := t.Underlying().(*types.Basic); ok && len(g.consts[t.Obj()]) == 0 {
			return nil // plain alias of a basic type: rendered as that basic type
		}
		if s, ok := t.Underlying().(*types.Slice); ok {
			return g.visitType(where, s.Elem())
		}
		if m, ok := t.Underlying().(*types.Map); ok {
			return g.visitType(where, m)
		}
		g.want(t)
		return nil
	}
	return fmt.Errorf("%s: unsupported type %s", where, t)
}

// special names the well-known library types with a fixed wire shape.
// Checked before unaliasing: since json/v2, json.RawMessage is an alias of
// jsontext.Value.
func special(t types.Type) string {
	var obj *types.TypeName
	switch t := t.(type) {
	case *types.Named:
		obj = t.Obj()
	case *types.Alias:
		obj = t.Obj()
	default:
		return ""
	}
	if obj.Pkg() == nil {
		return ""
	}
	switch obj.Pkg().Path() + "." + obj.Name() {
	case "time.Time":
		return "time"
	case "encoding/json.RawMessage":
		return "raw"
	}
	return ""
}

func sortedDecls(g *gen) []*decl {
	var ds []*decl
	for _, d := range g.decls {
		ds = append(ds, d)
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i].name < ds[j].name })
	return ds
}

// enumDecl returns t's decl when it's a generated string enum.
func (g *gen) enumDecl(t types.Type) *decl {
	n, ok := types.Unalias(t).(*types.Named)
	if !ok {
		return nil
	}
	if d := g.decls[n.Obj()]; d != nil && d.isEnum {
		return d
	}
	return nil
}

func writeDoc(b *strings.Builder, indent, doc string) {
	doc = strings.TrimSpace(doc)
	if doc == "" {
		return
	}
	doc = strings.ReplaceAll(doc, "*/", "* /")
	b.WriteString(indent + "/**\n")
	for _, line := range strings.Split(doc, "\n") {
		if line == "" {
			b.WriteString(indent + " *\n")
		} else {
			b.WriteString(indent + " * " + line + "\n")
		}
	}
	b.WriteString(indent + " */\n")
}
