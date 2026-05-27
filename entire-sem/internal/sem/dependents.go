package sem

import (
	"context"
	"regexp"
	"strings"

	"github.com/entireio/cli/entire-sem/internal/gitutil"
)

var identifierBoundary = regexp.MustCompile(`[A-Za-z0-9_$]+`)

type referenceIndex map[string]map[string]struct{}

func addDependentCounts(ctx context.Context, repo, head string, result *Result) error {
	names := changedReferenceNames(*result)
	if len(names) == 0 {
		return nil
	}

	index, err := buildReferenceIndex(ctx, repo, head, names)
	if err != nil {
		return err
	}

	for fileIndex := range result.Files {
		for changeIndex := range result.Files[fileIndex].Changes {
			change := &result.Files[fileIndex].Changes[changeIndex]
			name := referenceName(*change)
			change.DependentsCount = len(index[name])
		}
	}
	return nil
}

func changedReferenceNames(result Result) map[string]struct{} {
	out := map[string]struct{}{}
	for _, file := range result.Files {
		for _, change := range file.Changes {
			name := referenceName(change)
			if name != "" {
				out[name] = struct{}{}
			}
		}
	}
	return out
}

func buildReferenceIndex(ctx context.Context, repo, head string, names map[string]struct{}) (referenceIndex, error) {
	files, err := gitutil.ListFiles(ctx, repo, head)
	if err != nil {
		return nil, err
	}

	parser := TreeSitterParser{}
	index := referenceIndex{}
	for name := range names {
		index[name] = map[string]struct{}{}
	}

	for _, path := range files {
		if !Supported(path) {
			continue
		}
		content, ok, err := gitutil.ShowFile(ctx, repo, head, path)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}

		entities, _ := parser.Parse(path, content)
		lines := strings.Split(content, "\n")
		for _, entity := range entities {
			block := entityBlock(lines, entity)
			for name := range names {
				if shortEntityName(entity.Name) == name {
					continue
				}
				if containsIdentifier(block, name) {
					index[name][path+"#"+entity.Kind+":"+entity.Name] = struct{}{}
				}
			}
		}
	}

	return index, nil
}

func entityBlock(lines []string, entity Entity) string {
	start := entity.StartLine - 1
	if start < 0 {
		start = 0
	}
	end := entity.EndLine
	if end > len(lines) {
		end = len(lines)
	}
	if end <= start {
		return ""
	}
	return strings.Join(lines[start:end], "\n")
}

func containsIdentifier(content, name string) bool {
	for _, token := range identifierBoundary.FindAllString(content, -1) {
		if token == name {
			return true
		}
	}
	return false
}

func referenceName(change EntityChange) string {
	switch change.Type {
	case "renamed":
		if change.NewName != "" {
			return shortEntityName(change.NewName)
		}
		if change.OldName != "" {
			return shortEntityName(change.OldName)
		}
	}
	return shortEntityName(change.Name)
}

func shortEntityName(name string) string {
	if index := strings.LastIndex(name, "."); index >= 0 {
		return name[index+1:]
	}
	return name
}
