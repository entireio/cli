package sem

import (
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

type yamlBlock struct {
	Key       string
	StartLine int
	EndLine   int
}

func yamlEntities(path, content string) []Entity {
	lines := strings.Split(content, "\n")
	topLevel := yamlTopLevelBlocks(lines)
	if len(topLevel) == 0 {
		return nil
	}

	var entities []Entity
	entities = append(entities, yamlEntity("workflow", yamlWorkflowEntityName(path), yamlWorkflowSignature(path, lines, topLevel), 1, len(lines), lines))
	for _, block := range topLevel {
		switch block.Key {
		case "name":
			continue
		case "jobs":
			entities = append(entities, yamlJobEntities(block, lines)...)
		default:
			entities = append(entities, yamlEntity("section", block.Key, "section "+block.Key, block.StartLine, block.EndLine, lines))
		}
	}

	sort.Slice(entities, func(i, j int) bool {
		if entities[i].StartLine == entities[j].StartLine {
			return entities[i].Name < entities[j].Name
		}
		return entities[i].StartLine < entities[j].StartLine
	})
	return entities
}

func yamlTopLevelBlocks(lines []string) []yamlBlock {
	var blocks []yamlBlock
	for index, line := range lines {
		if yamlIndent(line) != 0 || yamlIgnoreLine(line) {
			continue
		}
		key, ok := yamlLineKey(line)
		if !ok {
			continue
		}
		if len(blocks) > 0 {
			blocks[len(blocks)-1].EndLine = index
		}
		blocks = append(blocks, yamlBlock{Key: key, StartLine: index + 1, EndLine: len(lines)})
	}
	return blocks
}

func yamlJobEntities(jobs yamlBlock, lines []string) []Entity {
	jobIndent := yamlDirectChildIndent(jobs, lines)
	if jobIndent < 0 {
		return nil
	}

	var blocks []yamlBlock
	for index := jobs.StartLine; index < jobs.EndLine && index < len(lines); index++ {
		line := lines[index]
		if yamlIndent(line) != jobIndent || yamlIgnoreLine(line) {
			continue
		}
		key, ok := yamlLineKey(line)
		if !ok {
			continue
		}
		if len(blocks) > 0 {
			blocks[len(blocks)-1].EndLine = index
		}
		blocks = append(blocks, yamlBlock{Key: key, StartLine: index + 1, EndLine: jobs.EndLine})
	}

	entities := make([]Entity, 0, len(blocks))
	for _, block := range blocks {
		name := "jobs." + block.Key
		entities = append(entities, yamlEntity("job", name, "job "+name, block.StartLine, block.EndLine, lines))
	}
	return entities
}

func yamlDirectChildIndent(parent yamlBlock, lines []string) int {
	parentIndent := yamlIndent(lines[parent.StartLine-1])
	childIndent := -1
	for index := parent.StartLine; index < parent.EndLine && index < len(lines); index++ {
		line := lines[index]
		if yamlIgnoreLine(line) {
			continue
		}
		indent := yamlIndent(line)
		if indent <= parentIndent {
			continue
		}
		if _, ok := yamlLineKey(line); !ok {
			continue
		}
		if childIndent < 0 || indent < childIndent {
			childIndent = indent
		}
	}
	return childIndent
}

func yamlEntity(kind, name, signature string, startLine, endLine int, lines []string) Entity {
	if startLine < 1 {
		startLine = 1
	}
	if endLine < startLine {
		endLine = startLine
	}
	if endLine > len(lines) {
		endLine = len(lines)
	}
	block := strings.Join(lines[startLine-1:endLine], "\n")
	return Entity{
		Kind:        kind,
		Name:        name,
		Signature:   normalize(signature),
		StartLine:   startLine,
		EndLine:     endLine,
		BodyHash:    hash(normalize(block)),
		Fingerprint: hash(normalize(entityFingerprintSource(Entity{Name: name, Signature: signature}, block))),
	}
}

func yamlWorkflowEntityName(path string) string {
	base := filepath.Base(path)
	ext := filepath.Ext(base)
	name := strings.TrimSuffix(base, ext)
	if name == "" || name == "." {
		return "workflow"
	}
	return name
}

func yamlWorkflowSignature(path string, lines []string, topLevel []yamlBlock) string {
	name := yamlTopLevelScalar("name", lines, topLevel)
	if name == "" {
		name = yamlWorkflowEntityName(path)
	}
	return "workflow " + name
}

func yamlTopLevelScalar(key string, lines []string, blocks []yamlBlock) string {
	for _, block := range blocks {
		if block.Key != key || block.StartLine < 1 || block.StartLine > len(lines) {
			continue
		}
		return yamlLineValue(lines[block.StartLine-1])
	}
	return ""
}

func yamlLineKey(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if yamlIgnoreTrimmedLine(trimmed) || strings.HasPrefix(trimmed, "- ") {
		return "", false
	}
	colon := yamlKeyColonIndex(trimmed)
	if colon <= 0 {
		return "", false
	}
	key := strings.TrimSpace(trimmed[:colon])
	if key == "" || strings.HasPrefix(key, "{") || strings.HasPrefix(key, "[") {
		return "", false
	}
	return yamlUnquote(key), true
}

func yamlLineValue(line string) string {
	trimmed := strings.TrimSpace(line)
	colon := yamlKeyColonIndex(trimmed)
	if colon < 0 || colon+1 >= len(trimmed) {
		return ""
	}
	return yamlUnquote(strings.TrimSpace(yamlStripInlineComment(trimmed[colon+1:])))
}

func yamlKeyColonIndex(value string) int {
	var quote rune
	escaped := false
	for index, char := range value {
		if quote != 0 {
			if quote == '"' && char == '\\' && !escaped {
				escaped = true
				continue
			}
			if char == quote && !escaped {
				quote = 0
			}
			escaped = false
			continue
		}
		switch char {
		case '\'', '"':
			quote = char
		case ':':
			return index
		}
	}
	return -1
}

func yamlStripInlineComment(value string) string {
	var quote rune
	escaped := false
	for index, char := range value {
		if quote != 0 {
			if quote == '"' && char == '\\' && !escaped {
				escaped = true
				continue
			}
			if char == quote && !escaped {
				quote = 0
			}
			escaped = false
			continue
		}
		switch char {
		case '\'', '"':
			quote = char
		case '#':
			if index == 0 || unicode.IsSpace(rune(value[index-1])) {
				return strings.TrimSpace(value[:index])
			}
		}
	}
	return strings.TrimSpace(value)
}

func yamlUnquote(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 {
		first := value[0]
		last := value[len(value)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			return value[1 : len(value)-1]
		}
	}
	return value
}

func yamlIndent(line string) int {
	count := 0
	for _, char := range line {
		switch char {
		case ' ':
			count++
		case '\t':
			count += 2
		default:
			return count
		}
	}
	return count
}

func yamlIgnoreLine(line string) bool {
	return yamlIgnoreTrimmedLine(strings.TrimSpace(line))
}

func yamlIgnoreTrimmedLine(line string) bool {
	return line == "" || strings.HasPrefix(line, "#") || line == "---" || line == "..."
}
