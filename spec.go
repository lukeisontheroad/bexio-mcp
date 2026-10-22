package main

import (
	"cmp"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
)

//go:embed openapi.json
var specJSON []byte

//go:embed guide.md
var guide string

type operation struct {
	ID, Method, Path, Tag string
	raw                   map[string]any
	params                []any // path-level parameters followed by the operation's own
}

// readOnly reports whether the operation only reads: GETs and the POST
// search endpoints.
func (o *operation) readOnly() bool {
	return o.Method == "GET" || strings.HasSuffix(o.Path, "/search")
}

// area is the consent-page row of a call: by its group, or for document
// positions and comments by the document type.
func (o *operation) area(in callInput) module {
	doc := strings.HasPrefix(o.Path, "/2.0/{kb_document_type}")
	kind := param(in.PathParams["kb_document_type"])
	for _, m := range modules {
		if doc && m.Doc != "" && m.Doc == kind || !doc && m.Tag == o.Tag {
			return m
		}
	}
	return module{}
}

// usable reports whether a connector with this access can run the
// operation; document positions and comments count under any sales row.
func (o *operation) usable(access map[string]bool) bool {
	doc := strings.HasPrefix(o.Path, "/2.0/{kb_document_type}")
	for _, m := range modules {
		write, granted := access[m.Name()]
		if granted && (write || o.readOnly()) && (doc && m.Doc != "" || !doc && m.Tag == o.Tag) {
			return true
		}
	}
	return false
}

func usableOps(ops []*operation, access map[string]bool) []*operation {
	return slices.DeleteFunc(slices.Clone(ops), func(op *operation) bool { return !op.usable(access) })
}

type api struct {
	root  map[string]any
	ops   map[string]*operation
	byTag map[string][]*operation
}

// bexioAPI is the embedded spec, parsed once.
var bexioAPI = loadAPI()

func loadAPI() *api {
	a := &api{ops: map[string]*operation{}, byTag: map[string][]*operation{}}
	if err := json.Unmarshal(specJSON, &a.root); err != nil {
		panic(err)
	}
	for _, path := range slices.Sorted(maps.Keys(obj(a.root["paths"]))) {
		item := obj(obj(a.root["paths"])[path])
		for _, method := range []string{"get", "post", "put", "patch", "delete"} {
			raw := obj(item[method])
			tags, _ := raw["tags"].([]any)
			if raw == nil || len(tags) == 0 || !servable(path, raw) {
				continue
			}
			op := &operation{
				ID: str(raw["operationId"]), Method: strings.ToUpper(method), Path: path, Tag: str(tags[0]),
				raw: raw, params: append(list(item["parameters"]), list(raw["parameters"])...),
			}
			a.ops[op.ID] = op
			a.byTag[op.Tag] = append(a.byTag[op.Tag], op)
		}
	}
	return a
}

// servable drops what a chat connector must not or can't carry: banking
// payments (they move money) and payroll (salary data), which are neither
// documented nor callable and whose scopes are never requested; PDF and file
// downloads (binary); and multipart uploads.
func servable(path string, raw map[string]any) bool {
	if strings.HasPrefix(path, "/4.0/banking/payments") || strings.HasPrefix(path, "/4.0/purchase/outgoing-payments") || strings.HasPrefix(path, "/4.0/payroll") {
		return false
	}
	for _, suffix := range []string{"/pdf", "/download", "/preview"} {
		if strings.HasSuffix(path, suffix) {
			return false
		}
	}
	content := obj(obj(raw["requestBody"])["content"])
	return content == nil || content["application/json"] != nil
}

// find looks an operation up by id, ignoring case. A miss returns close
// ids instead: bexio's naming is uneven (v2SearchContact, v2SearchInvoices).
func (a *api) find(id string) (*operation, []string) {
	var near []string
	for _, op := range a.ops {
		l, g := strings.ToLower(op.ID), strings.ToLower(id)
		if l == g {
			return op, nil
		}
		if g != "" && (strings.Contains(l, g) || strings.Contains(g, l)) {
			near = append(near, op.ID)
		}
	}
	slices.Sort(near)
	return nil, near[:min(len(near), 5)]
}

func didYouMean(near []string) string {
	if len(near) == 0 {
		return ""
	}
	return " (did you mean " + strings.Join(near, ", ") + "?)"
}

// docs answers bexio_docs for comma-separated topics, the guide plus group
// index for none. A failed topic shows its error in place; only if all
// fail is the answer an error.
func (a *api) docs(topics string, access map[string]bool) (string, error) {
	var parts []string
	failed := 0
	for _, t := range strings.Split(topics, ",") {
		if t = strings.TrimSpace(t); t == "" {
			continue
		}
		text, err := a.doc(t, access)
		if err != nil {
			text, failed = err.Error(), failed+1
		}
		parts = append(parts, text)
	}
	if len(parts) == 0 {
		return a.index(access), nil
	}
	text := strings.Join(parts, "\n\n")
	if failed == len(parts) {
		return "", errors.New(text)
	}
	return text, nil
}

// doc is an operation list for a tag or the full reference for an
// operationId, limited to what the connector can run.
func (a *api) doc(topic string, access map[string]bool) (string, error) {
	op, near := a.find(topic)
	if op != nil {
		if !op.usable(access) {
			return "", fmt.Errorf("%s is not available on this connector: its area is off or read only", op.ID)
		}
		return a.opDoc(op), nil
	}
	for tag, ops := range a.byTag {
		if strings.EqualFold(tag, topic) {
			ops = usableOps(ops, access)
			if len(ops) == 0 {
				return "", fmt.Errorf("%s is not available on this connector: its area is off", tag)
			}
			var b strings.Builder
			fmt.Fprintf(&b, "# %s\n", tag)
			for _, op := range ops {
				fmt.Fprintf(&b, "%s  %s %s  %s\n", op.ID, op.Method, op.Path, str(op.raw["summary"]))
			}
			return b.String(), nil
		}
	}
	return "", fmt.Errorf("unknown topic %q%s: call bexio_docs without a topic for the list of groups", topic, didYouMean(near))
}

func (a *api) index(access map[string]bool) string {
	var b strings.Builder
	b.WriteString(guide)
	b.WriteString("\n## Operation groups (pass one as topic)\n")
	for _, g := range list(a.root["x-tagGroups"]) {
		var tags []string
		for _, t := range list(obj(g)["tags"]) {
			if n := len(usableOps(a.byTag[str(t)], access)); n > 0 {
				tags = append(tags, fmt.Sprintf("%s (%d)", t, n))
			}
		}
		if len(tags) > 0 {
			fmt.Fprintf(&b, "- %s: %s\n", str(obj(g)["name"]), strings.Join(tags, ", "))
		}
	}
	return b.String()
}

func (a *api) opDoc(op *operation) string {
	var b strings.Builder
	tool := "bexio_write"
	if op.readOnly() {
		tool = "bexio_read"
	}
	fmt.Fprintf(&b, "%s %s  (%s via %s)\n%s\n", op.Method, op.Path, op.ID, tool, str(op.raw["summary"]))
	if d := links(str(op.raw["description"])); d != "" {
		fmt.Fprintf(&b, "\n%s\n", d)
	}
	for _, sec := range list(op.raw["security"]) {
		for _, scopes := range obj(sec) {
			if s := list(scopes); len(s) > 0 {
				fmt.Fprintf(&b, "\nScopes: %v\n", s)
			}
		}
	}
	var params strings.Builder
	for _, p := range op.params {
		p := a.deref(p)
		if in := str(p["in"]); in == "path" || in == "query" {
			fmt.Fprintf(&params, "  %s (%s%s): %s%s\n", p["name"], in, flag(p["required"] == true, ", required"),
				a.typeOf(a.deref(p["schema"])), dash(str(p["description"])))
		}
	}
	if params.Len() > 0 {
		b.WriteString("\nParameters:\n" + params.String())
	}
	if s := obj(obj(obj(obj(op.raw["requestBody"])["content"])["application/json"])["schema"]); s != nil {
		s = a.deref(s)
		if str(s["type"]) == "array" {
			b.WriteString("\nBody: JSON array of\n")
			s = a.deref(s["items"])
		} else {
			b.WriteString("\nBody:\n")
		}
		a.fields(&b, s, "  ", 0)
	}
	return b.String()
}

// fields writes one line per writable property, recursing into nested
// objects and arrays of objects.
func (a *api) fields(b *strings.Builder, s map[string]any, indent string, depth int) {
	if depth > 5 { // guards against self-referencing schemas
		return
	}
	for i, alt := range append(list(s["oneOf"]), list(s["anyOf"])...) {
		alt := a.deref(alt)
		fmt.Fprintf(b, "%soption %d%s:\n", indent, i+1, dash(str(alt["title"])))
		a.fields(b, alt, indent+"  ", depth+1)
	}
	props, required := a.flatten(s)
	for _, name := range slices.Sorted(maps.Keys(props)) {
		p := a.deref(props[name])
		if p["readOnly"] == true {
			continue
		}
		fmt.Fprintf(b, "%s%s: %s%s%s\n", indent, name, a.typeOf(p), flag(required[name], ", required"), dash(str(p["description"])))
		if str(p["type"]) == "array" {
			p = a.deref(p["items"])
		}
		a.fields(b, p, indent+"  ", depth+1)
	}
}

// flatten merges allOf parts into one property set.
func (a *api) flatten(s map[string]any) (map[string]any, map[string]bool) {
	props, required := map[string]any{}, map[string]bool{}
	maps.Copy(props, obj(s["properties"]))
	for _, r := range list(s["required"]) {
		required[str(r)] = true
	}
	for _, part := range list(s["allOf"]) {
		p, r := a.flatten(a.deref(part))
		maps.Copy(props, p)
		maps.Copy(required, r)
	}
	return props, required
}

// deref follows a local "$ref" (e.g. #/components/schemas/Contact).
func (a *api) deref(v any) map[string]any {
	m := obj(v)
	ref, ok := m["$ref"].(string)
	if !ok {
		return m
	}
	var target any = a.root
	for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		target = obj(target)[part]
	}
	return a.deref(target)
}

func (a *api) typeOf(s map[string]any) string {
	t := cmp.Or(str(s["type"]), "object") // oneOf variants carry no type
	if t == "array" {
		t = "array of " + a.typeOf(a.deref(s["items"]))
	}
	if f := str(s["format"]); f != "" {
		t += " (" + f + ")"
	}
	if e := list(s["enum"]); len(e) > 0 {
		t += fmt.Sprintf(" %v", e)
	}
	if n, ok := s["maxLength"].(float64); ok {
		t += fmt.Sprintf(", max %d chars", int(n))
	}
	return t + flag(s["nullable"] == true, ", nullable")
}

var opLink = regexp.MustCompile(`\[([^\]]+)\]\(#operation/([^)]+)\)`)

// links rewrites Redoc cross-references into operationIds Claude can look up.
func links(s string) string {
	return strings.TrimSpace(opLink.ReplaceAllString(s, "$1 ($2)"))
}

func dash(s string) string {
	if s = strings.Join(strings.Fields(links(s)), " "); s == "" {
		return ""
	}
	return " — " + s
}

func flag(ok bool, s string) string {
	if ok {
		return s
	}
	return ""
}

func obj(v any) map[string]any { m, _ := v.(map[string]any); return m }
func list(v any) []any         { l, _ := v.([]any); return l }
func str(v any) string         { s, _ := v.(string); return s }
