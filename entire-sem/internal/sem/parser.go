package sem

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"sort"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/golang"
	"github.com/smacker/go-tree-sitter/javascript"
	"github.com/smacker/go-tree-sitter/python"
	"github.com/smacker/go-tree-sitter/rust"
	treesittertsx "github.com/smacker/go-tree-sitter/typescript/tsx"
	treesitterts "github.com/smacker/go-tree-sitter/typescript/typescript"
)

type languageSpec struct {
	language string
	grammar  *sitter.Language
}

var treeSitterLanguages = map[string]languageSpec{
	".go":  {language: "Go", grammar: golang.GetLanguage()},
	".py":  {language: "Python", grammar: python.GetLanguage()},
	".js":  {language: "JavaScript", grammar: javascript.GetLanguage()},
	".jsx": {language: "JavaScript", grammar: treesittertsx.GetLanguage()},
	".ts":  {language: "TypeScript", grammar: treesitterts.GetLanguage()},
	".tsx": {language: "TypeScript", grammar: treesittertsx.GetLanguage()},
	".rs":  {language: "Rust", grammar: rust.GetLanguage()},
}

type TreeSitterParser struct{}

func (TreeSitterParser) Parse(path, content string) ([]Entity, string) {
	spec, ok := languageForPath(path)
	if !ok {
		return nil, ""
	}
	src := []byte(content)
	root, err := sitter.ParseCtx(context.Background(), src, spec.grammar)
	if err != nil || root == nil || root.IsNull() {
		return nil, spec.language
	}

	var entities []Entity
	walkEntities(root, src, "", &entities)
	sort.Slice(entities, func(i, j int) bool {
		if entities[i].StartLine == entities[j].StartLine {
			return entities[i].Name < entities[j].Name
		}
		return entities[i].StartLine < entities[j].StartLine
	})
	return entities, spec.language
}

func Supported(path string) bool {
	_, ok := languageForPath(path)
	return ok
}

func languageForPath(path string) (languageSpec, bool) {
	spec, ok := treeSitterLanguages[strings.ToLower(filepath.Ext(path))]
	return spec, ok
}

func walkEntities(node *sitter.Node, src []byte, scope string, entities *[]Entity) {
	if !validNode(node) {
		return
	}
	entity, ok := entityFromNode(node, src, scope)
	childScope := scope
	if ok {
		*entities = append(*entities, entity)
		if scopesChildren(entity.Kind) {
			childScope = entity.Name
		}
	}
	for i := 0; i < int(node.NamedChildCount()); i++ {
		walkEntities(node.NamedChild(i), src, childScope, entities)
	}
}

func entityFromNode(node *sitter.Node, src []byte, scope string) (Entity, bool) {
	var kind string
	var name string
	switch node.Type() {
	case "class_definition", "class_declaration":
		kind = "class"
		name = nodeName(node, src)
	case "function_definition":
		kind = "function"
		name = nodeName(node, src)
		if scope != "" {
			kind = "method"
			name = qualify(scope, name)
		}
	case "function_declaration", "function_item":
		kind = "function"
		name = nodeName(node, src)
	case "function_signature_item":
		kind = "function"
		name = nodeName(node, src)
		if scope != "" {
			kind = "method"
			name = qualify(scope, name)
		}
	case "method_declaration":
		kind = "method"
		name = nodeName(node, src)
		if receiver := goReceiverName(node, src); receiver != "" {
			name = qualify(receiver, name)
		}
	case "method_definition":
		kind = "method"
		name = nodeName(node, src)
		if scope != "" {
			name = qualify(scope, name)
		}
	case "type_spec", "type_alias_declaration":
		kind = "type"
		name = nodeName(node, src)
	case "interface_declaration":
		kind = "interface"
		name = nodeName(node, src)
	case "struct_item":
		kind = "struct"
		name = nodeName(node, src)
	case "enum_item":
		kind = "enum"
		name = nodeName(node, src)
	case "trait_item":
		kind = "trait"
		name = nodeName(node, src)
	case "variable_declarator":
		value := node.ChildByFieldName("value")
		if !functionLikeValue(value) {
			return Entity{}, false
		}
		kind = "function"
		name = nodeName(node, src)
	default:
		return Entity{}, false
	}
	if name == "" {
		return Entity{}, false
	}

	block := node.Content(src)
	entity := Entity{
		Kind:        kind,
		Name:        name,
		Signature:   signatureFromNode(node, src),
		StartLine:   int(node.StartPoint().Row) + 1,
		EndLine:     int(node.EndPoint().Row) + 1,
		BodyHash:    hash(normalize(block)),
		Fingerprint: hash(normalize(entityFingerprintSource(Entity{Name: name, Signature: signatureFromNode(node, src)}, block))),
	}
	return entity, true
}

func nodeName(node *sitter.Node, src []byte) string {
	for _, field := range []string{"name", "property", "type"} {
		child := node.ChildByFieldName(field)
		if validNode(child) {
			return child.Content(src)
		}
	}
	for i := 0; i < int(node.NamedChildCount()); i++ {
		child := node.NamedChild(i)
		if validNode(child) && isNameNode(child.Type()) {
			return child.Content(src)
		}
	}
	return ""
}

func isNameNode(nodeType string) bool {
	switch nodeType {
	case "identifier", "type_identifier", "field_identifier", "property_identifier", "package_identifier":
		return true
	default:
		return false
	}
}

func signatureFromNode(node *sitter.Node, src []byte) string {
	start := node.StartByte()
	end := node.EndByte()
	if body := node.ChildByFieldName("body"); validNode(body) {
		end = body.StartByte()
	} else if body := firstBodyLikeChild(node); validNode(body) {
		end = body.StartByte()
	}
	if end <= start || int(end) > len(src) {
		end = node.EndByte()
	}
	signature := strings.TrimSpace(string(src[start:end]))
	if index := strings.IndexByte(signature, '\n'); index >= 0 {
		signature = signature[:index]
	}
	return strings.TrimSpace(strings.TrimRight(signature, "{:; \t\r\n"))
}

func firstBodyLikeChild(node *sitter.Node) *sitter.Node {
	for i := 0; i < int(node.NamedChildCount()); i++ {
		child := node.NamedChild(i)
		if !validNode(child) {
			continue
		}
		switch child.Type() {
		case "block", "statement_block", "class_body", "declaration_list", "field_declaration_list", "interface_body":
			return child
		}
	}
	return nil
}

func functionLikeValue(node *sitter.Node) bool {
	if !validNode(node) {
		return false
	}
	switch node.Type() {
	case "arrow_function", "function", "function_expression", "generator_function":
		return true
	default:
		return false
	}
}

func scopesChildren(kind string) bool {
	switch kind {
	case "class", "struct", "trait", "interface":
		return true
	default:
		return false
	}
}

func goReceiverName(node *sitter.Node, src []byte) string {
	signature := signatureFromNode(node, src)
	receiverStart := strings.Index(signature, "func (")
	if receiverStart < 0 {
		return ""
	}
	receiver := signature[receiverStart+len("func ("):]
	receiverEnd := strings.Index(receiver, ")")
	if receiverEnd < 0 {
		return ""
	}
	receiver = strings.TrimSpace(receiver[:receiverEnd])
	fields := strings.Fields(receiver)
	if len(fields) == 0 {
		return ""
	}
	name := strings.Trim(fields[len(fields)-1], "*[]")
	if index := strings.LastIndex(name, "."); index >= 0 {
		name = name[index+1:]
	}
	return strings.TrimSpace(name)
}

func qualify(scope, name string) string {
	if scope == "" || name == "" || strings.HasPrefix(name, scope+".") {
		return name
	}
	return scope + "." + name
}

func validNode(node *sitter.Node) bool {
	return node != nil && !node.IsNull()
}

func normalize(value string) string {
	fields := strings.Fields(value)
	return strings.Join(fields, " ")
}

func entityFingerprintSource(entity Entity, block string) string {
	lines := strings.Split(block, "\n")
	if len(lines) <= 1 {
		return strings.ReplaceAll(entity.Signature, entity.Name, "<name>")
	}
	return strings.Join(lines[1:], "\n")
}

func hash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:16]
}
