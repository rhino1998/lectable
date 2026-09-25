package main

import (
	"fmt"
	"go/constant"
	"go/types"
	"reflect"
	"strconv"
	"strings"

	"github.com/rhino1998/lectable/backend/internal/emotions"
	"github.com/rhino1998/lectable/backend/internal/voices"
)

// catalogList is a runtime Go slice emitted as a constant list, plus a
// union of its entries' ids.
type catalogList struct {
	name   string // CLONE_MODELS (TS and Kotlin)
	idType string // CloneModel: the union of the entries' ids
	doc    string
	value  any    // a slice of json-tagged structs with an "id" field
	elem   string // the elements' Go type, qualified: generated as a type too
}

// catalogScalar is a Go string constant emitted as a typed constant.
type catalogScalar struct {
	name   string // DEFAULT_CLONE_MODEL
	goPkg  string
	goName string
	typ    string // the TS type it's declared as (an idType or enum)
	doc    string
	value  string // filled in by catalog()
}

type catalogData struct {
	lists   []catalogList
	scalars []catalogScalar
}

var catalogLists = []catalogList{
	{
		name: "CLONE_MODELS", idType: "CloneModel", value: voices.CloneModels, elem: voicesPkg + ".CloneModelInfo",
		doc: "Every user-selectable clone model (a book's VoiceSettings.cloneModel), in display order - backend voices.CloneModels.",
	},
	{
		name: "DESIGN_MODELS", idType: "DesignModel", value: voices.DesignModels, elem: voicesPkg + ".DesignModelInfo",
		doc: "Every VoiceDesign engine (a preset's designModel), in display order - backend voices.DesignModels.",
	},
	{
		name: "EMOTIONS", idType: "Emotion", value: emotions.All, elem: emotionsPkg + ".Emotion",
		doc: "The fixed emotion set a dialogue line can carry (Paragraph.emotion; \"\" is neutral), in display order - backend emotions.All.",
	},
}

var catalogScalars = []catalogScalar{
	{name: "DEFAULT_CLONE_MODEL", goPkg: voicesPkg, goName: "DefaultCloneModel", typ: "CloneModel",
		doc: "The factory default clone model new books start with, and the fallback for a book with none set."},
	{name: "INSTRUCTED_CLONE_MODEL", goPkg: voicesPkg, goName: "InstructedCloneModel", typ: "CloneModel",
		doc: "The one clone model that accepts a clone-time style instruction alongside its reference clip."},
	{name: "HIGGS_CLONE_MODEL", goPkg: voicesPkg, goName: "HiggsCloneModel", typ: "CloneModel",
		doc: "Higgs Audio - the only clone model that reads the <|prosody:pause|> tag (Paragraph.directionMarks)."},
	{name: "DEFAULT_DESIGN_MODEL", goPkg: voicesPkg, goName: "DefaultDesignModel", typ: "DesignModel",
		doc: "What a new custom voice preset is designed with when the request doesn't say."},
	{name: "DEFAULT_CHARACTER_VOICE_MODE", goPkg: storePkg, goName: "DefaultCharacterVoiceMode", typ: "CharacterVoiceMode",
		doc: "What a new book's characterVoiceMode starts as."},
}

// catalog resolves the catalog's Go-side values and enqueues its types.
func (g *gen) catalog() (*catalogData, error) {
	cat := &catalogData{lists: catalogLists}
	for _, l := range cat.lists {
		i := strings.LastIndex(l.elem, ".")
		obj, err := g.lookup(l.elem[:i], l.elem[i+1:])
		if err != nil {
			return nil, err
		}
		g.want(obj.Type().(*types.Named))
	}
	for _, s := range catalogScalars {
		obj, err := g.lookup(s.goPkg, s.goName)
		if err != nil {
			return nil, err
		}
		c, ok := obj.(*types.Const)
		if !ok || c.Val().Kind() != constant.String {
			return nil, fmt.Errorf("%s.%s: want a string constant", s.goPkg, s.goName)
		}
		s.value = constant.StringVal(c.Val())
		if n, ok := c.Type().(*types.Named); ok && len(g.consts[n.Obj()]) > 0 {
			g.want(n)
		}
		cat.scalars = append(cat.scalars, s)
	}
	return cat, nil
}

// jsonFields walks a struct value's json-visible fields: name, value, and
// whether omitempty drops it.
func jsonFields(v reflect.Value, fn func(name string, fv reflect.Value)) {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if !sf.IsExported() {
			continue
		}
		name, opts, _ := strings.Cut(sf.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = sf.Name
		}
		fv := v.Field(i)
		if strings.Contains(","+opts+",", ",omitempty,") && fv.IsZero() {
			continue
		}
		fn(name, fv)
	}
}

func tsLiteral(v reflect.Value) string {
	switch v.Kind() {
	case reflect.Pointer:
		return tsLiteral(v.Elem())
	case reflect.String:
		return tsString(v.String())
	case reflect.Bool:
		return strconv.FormatBool(v.Bool())
	case reflect.Float32, reflect.Float64:
		return strconv.FormatFloat(v.Float(), 'g', -1, 64)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(v.Int(), 10)
	case reflect.Struct:
		var parts []string
		jsonFields(v, func(name string, fv reflect.Value) {
			parts = append(parts, tsKey(name)+": "+tsLiteral(fv))
		})
		return "{ " + strings.Join(parts, ", ") + " }"
	}
	panic(fmt.Sprintf("apigen: no TS literal for %s", v.Type()))
}

func tsString(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`, "\n", `\n`).Replace(s) + "'"
}

func ktString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "$", `\$`).Replace(s) + `"`
}

// ktLiteral renders v as a Kotlin expression - a struct as its generated
// data class's constructor (dto names the class).
func ktLiteral(v reflect.Value, dto string) string {
	switch v.Kind() {
	case reflect.Pointer:
		return ktLiteral(v.Elem(), dto)
	case reflect.String:
		return ktString(v.String())
	case reflect.Bool:
		return strconv.FormatBool(v.Bool())
	case reflect.Float32, reflect.Float64:
		s := strconv.FormatFloat(v.Float(), 'g', -1, 64)
		if !strings.ContainsAny(s, ".e") {
			s += ".0"
		}
		return s
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(v.Int(), 10)
	case reflect.Struct:
		var parts []string
		jsonFields(v, func(name string, fv reflect.Value) {
			ident, _ := ktIdent(name)
			parts = append(parts, ident+" = "+ktLiteral(fv, ""))
		})
		return dto + "(" + strings.Join(parts, ", ") + ")"
	}
	panic(fmt.Sprintf("apigen: no Kotlin literal for %s", v.Type()))
}

// elemName is the generated TS name of a catalog list's element type.
func (g *gen) elemName(l catalogList) string {
	i := strings.LastIndex(l.elem, ".")
	obj, _ := g.lookup(l.elem[:i], l.elem[i+1:])
	return tsName(obj.(*types.TypeName))
}
