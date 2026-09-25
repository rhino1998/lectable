package main

import (
	"fmt"
	"go/types"
	"reflect"
	"strings"
	"unicode"
)

func (g *gen) ktType(t types.Type) (typ, zero string) {
	switch special(t) {
	case "time":
		return "String", `""`
	case "raw":
		return "JsonElement?", "null"
	}
	switch t := types.Unalias(t).(type) {
	case *types.Basic:
		switch {
		case t.Info()&types.IsString != 0:
			return "String", `""`
		case t.Info()&types.IsBoolean != 0:
			return "Boolean", "false"
		case t.Info()&types.IsFloat != 0:
			return "Double", "0.0"
		case t.Kind() == types.Int64 || t.Kind() == types.Uint64 || t.Kind() == types.Uint32:
			return "Long", "0L"
		case t.Info()&types.IsInteger != 0:
			return "Int", "0"
		}
	case *types.Pointer:
		inner, _ := g.ktType(t.Elem())
		return inner + "?", "null"
	case *types.Slice:
		if b, ok := t.Elem().(*types.Basic); ok && b.Kind() == types.Byte {
			return "String", `""`
		}
		inner, _ := g.ktType(t.Elem())
		return "List<" + inner + ">", "emptyList()"
	case *types.Array:
		inner, _ := g.ktType(t.Elem())
		return "List<" + inner + ">", "emptyList()"
	case *types.Map:
		inner, _ := g.ktType(t.Elem())
		return "Map<String, " + inner + ">", "emptyMap()"
	case *types.Interface:
		return "JsonElement?", "null"
	case *types.Named:
		if d, ok := g.decls[t.Obj()]; ok && d != nil {
			if d.isEnum {
				return d.name, `""`
			}
			return d.name + "Dto", d.name + "Dto()"
		}
		return g.ktType(t.Underlying())
	}
	panic(fmt.Sprintf("apigen: no Kotlin mapping for %s", t))
}

var ktKeywords = map[string]bool{
	"as": true, "break": true, "class": true, "continue": true, "do": true, "else": true,
	"false": true, "for": true, "fun": true, "if": true, "in": true, "interface": true,
	"is": true, "null": true, "object": true, "package": true, "return": true, "super": true,
	"this": true, "throw": true, "true": true, "try": true, "typealias": true, "typeof": true,
	"val": true, "var": true, "when": true, "while": true,
}

func ktIdent(json string) (ident string, needsSerialName bool) {
	var b strings.Builder
	upper := false
	for i, r := range json {
		switch {
		case unicode.IsLetter(r) || (i > 0 && unicode.IsDigit(r)):
			if upper {
				r = unicode.ToUpper(r)
				upper = false
			}
			b.WriteRune(r)
		default:
			upper = b.Len() > 0
		}
	}
	ident = b.String()
	needsSerialName = ident != json
	if ktKeywords[ident] {
		ident = "`" + ident + "`"
	}
	return ident, needsSerialName
}

// ktFields flattens embedded structs, since JSON (and so the Kotlin class)
// has no notion of them.
func (g *gen) ktFields(d *decl) []field {
	var out []field
	for _, e := range d.embeds {
		out = append(out, g.ktFields(g.decls[g.byName[e]])...)
	}
	return append(out, d.fields...)
}

func ktHeader(b *strings.Builder, pkg string, imports ...string) {
	b.WriteString("// " + headerNote + "\n\n")
	b.WriteString("@file:Suppress(\"unused\")\n\n")
	b.WriteString("package " + pkg + "\n\n")
	for _, imp := range imports {
		b.WriteString("import " + imp + "\n")
	}
}

func (g *gen) renderKotlinTypes(cat *catalogData) []byte {
	var b strings.Builder
	ktHeader(&b, ktDTOPackage,
		"kotlinx.serialization.SerialName",
		"kotlinx.serialization.Serializable",
		"kotlinx.serialization.json.JsonElement")

	for _, d := range sortedDecls(g) {
		if !d.isEnum {
			continue
		}
		b.WriteString("\n")
		writeDoc(&b, "", d.doc+"\n\nValues: see ["+plural(d.name)+"].")
		b.WriteString("typealias " + d.name + " = String\n\n")
		writeDoc(&b, "", "["+d.name+"]'s wire values, in declaration order.")
		b.WriteString("object " + plural(d.name) + " {\n")
		var names []string
		for _, v := range d.enum {
			n := upperSnake(v)
			names = append(names, n)
			b.WriteString("    const val " + n + ": " + d.name + " = " + ktString(v) + "\n")
		}
		b.WriteString("    val VALUES: List<" + d.name + "> = listOf(" + strings.Join(names, ", ") + ")\n")
		b.WriteString("}\n")
	}

	b.WriteString("\n/** Backend catalog values - see each constant. */\n")
	b.WriteString("object ApiCatalog {\n")
	for i, l := range cat.lists {
		if i > 0 {
			b.WriteString("\n")
		}
		writeDoc(&b, "    ", l.doc)
		dto := g.elemName(l) + "Dto"
		b.WriteString("    val " + l.name + ": List<" + dto + "> = listOf(\n")
		v := reflect.ValueOf(l.value)
		for j := 0; j < v.Len(); j++ {
			b.WriteString("        " + ktLiteral(v.Index(j), dto) + ",\n")
		}
		b.WriteString("    )\n")
	}
	for _, s := range cat.scalars {
		b.WriteString("\n")
		writeDoc(&b, "    ", s.doc)
		b.WriteString("    const val " + s.name + ": String = " + ktString(s.value) + "\n")
	}
	b.WriteString("}\n")

	for _, d := range sortedDecls(g) {
		if d.isEnum {
			continue
		}
		b.WriteString("\n")
		writeDoc(&b, "", d.doc)
		b.WriteString("@Serializable\ndata class " + d.name + "Dto(\n")
		for _, f := range g.ktFields(d) {
			writeDoc(&b, "    ", f.doc)
			ident, serial := ktIdent(f.json)
			if serial {
				b.WriteString("    @SerialName(\"" + f.json + "\")\n")
			}
			var typ, zero, typ0 string
			if f.ktType == "" {
				typ0, _ = g.ktType(f.typ)
			}
			switch {
			case f.ktType != "":
				typ = f.ktType
				if t, def, ok := strings.Cut(typ, " = "); ok {
					typ, zero = t, def
					break
				}
				switch {
				case strings.HasSuffix(typ, "?"):
					zero = "null"
				case strings.HasPrefix(typ, "List<"):
					zero = "emptyList()"
				}
			case f.optional && (typ0 == "String" || typ0 == "Int" || typ0 == "Long" || typ0 == "Double"):
				typ, zero = typ0+"?", "null"
			case f.optional && g.enumDecl(f.typ) != nil:
				typ, zero = typ0+"?", "null"
			default:
				typ, zero = g.ktType(f.typ)
			}
			if zero == "" {
				b.WriteString("    val " + ident + ": " + typ + ",\n")
			} else {
				b.WriteString("    val " + ident + ": " + typ + " = " + zero + ",\n")
			}
		}
		b.WriteString(")\n")
	}
	return []byte(b.String())
}

func (g *gen) renderKotlinAPI(routes []*route) []byte {
	var b strings.Builder
	ktHeader(&b, ktRemotePackage,
		ktDTOPackage+".*",
		"okhttp3.MultipartBody",
		"okhttp3.ResponseBody",
		"retrofit2.http.Body",
		"retrofit2.http.DELETE",
		"retrofit2.http.GET",
		"retrofit2.http.Multipart",
		"retrofit2.http.POST",
		"retrofit2.http.PUT",
		"retrofit2.http.Part",
		"retrofit2.http.Path",
		"retrofit2.http.Query",
		"retrofit2.http.Streaming")
	b.WriteString("\n")
	writeDoc(&b, "", "Every backend REST route. Paths are relative; the host is injected by [DynamicBaseUrlInterceptor]. A 204 route returns Unit and a file route a streamed [ResponseBody]; any non-2xx throws retrofit2.HttpException.")
	b.WriteString("interface LectableApi {\n")
	for i, r := range routes {
		if i > 0 {
			b.WriteString("\n")
		}
		writeDoc(&b, "    ", r.doc)
		if len(r.multipart) > 0 {
			b.WriteString("    @Multipart\n")
		}
		if r.kind == respBinary {
			b.WriteString("    @Streaming\n")
		}
		if r.method == "DELETE" && r.body != nil {
			panic("apigen: a DELETE route with a body needs @HTTP support")
		}
		b.WriteString("    @" + r.method + "(" + ktString(strings.TrimPrefix(r.path, "/")) + ")\n")
		var params []string
		for _, p := range r.pathParams {
			ident, _ := ktIdent(p.name)
			t, _ := g.ktType(p.typ)
			params = append(params, "@Path("+ktString(p.name)+") "+ident+": "+t)
		}
		for _, p := range r.query {
			ident, _ := ktIdent(p.name)
			t, _ := g.ktType(p.typ)
			params = append(params, "@Query("+ktString(p.name)+") "+ident+": "+t+"? = null")
		}
		for _, f := range r.multipart {
			ident, _ := ktIdent(f)
			params = append(params, "@Part "+ident+": MultipartBody.Part? = null")
		}
		if r.body != nil {
			t, _ := g.ktType(r.body)
			params = append(params, "@Body body: "+t)
		}
		resp := "Unit"
		switch r.kind {
		case respJSON:
			resp, _ = g.ktType(r.resp)
		case respBinary:
			resp = "ResponseBody"
		}
		if len(params) == 0 {
			b.WriteString("    suspend fun " + r.name + "(): " + resp + "\n")
			continue
		}
		b.WriteString("    suspend fun " + r.name + "(\n")
		for _, p := range params {
			b.WriteString("        " + p + ",\n")
		}
		b.WriteString("    ): " + resp + "\n")
	}
	b.WriteString("}\n")
	return []byte(b.String())
}

func (g *gen) renderKotlinLive(topics []liveTopic) []byte {
	var b strings.Builder
	ktHeader(&b, ktRemotePackage,
		ktDTOPackage+".*",
		"kotlinx.serialization.KSerializer",
		"kotlinx.serialization.serializer")
	b.WriteString("\n")
	writeDoc(&b, "", "One live topic instance (GET /api/events): its wire name, params, and how to decode its value.")
	b.WriteString("class LiveTopic<T>(val name: String, val params: Map<String, Any>, val deserializer: KSerializer<T>)\n\n")
	writeDoc(&b, "", "Every live topic the backend serves.")
	b.WriteString("object LiveTopics {\n")
	for i, t := range topics {
		if i > 0 {
			b.WriteString("\n")
		}
		writeDoc(&b, "    ", t.doc)
		data, _ := g.ktType(t.data)
		var params, entries []string
		for _, p := range t.params {
			ident, _ := ktIdent(p.name)
			typ := "String"
			if p.typ == "int" {
				typ = "Int"
			}
			params = append(params, ident+": "+typ)
			entries = append(entries, ktString(p.name)+" to "+ident)
		}
		pm := "emptyMap()"
		if len(entries) > 0 {
			pm = "mapOf(" + strings.Join(entries, ", ") + ")"
		}
		fname, _ := ktIdent(t.name)
		b.WriteString("    fun " + fname + "(" + strings.Join(params, ", ") + "): LiveTopic<" + data + "> =\n")
		b.WriteString("        LiveTopic(" + ktString(t.name) + ", " + pm + ", serializer<" + data + ">())\n")
	}
	b.WriteString("}\n")
	return []byte(b.String())
}
