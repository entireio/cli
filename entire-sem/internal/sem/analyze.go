package sem

import (
	"context"
	"fmt"
	"math"
	"sort"

	"github.com/entireio/cli/entire-sem/internal/gitutil"
)

func AnalyzeGitRange(ctx context.Context, repo, base, head string, paths []string) (Result, error) {
	changed, err := gitutil.ChangedFiles(ctx, repo, base, head, paths)
	if err != nil {
		return Result{}, err
	}
	parser := TreeSitterParser{}
	result := Result{Base: base, Head: head}
	for _, file := range changed {
		path := file.Path
		oldPath := file.OldPath
		if oldPath == "" {
			oldPath = path
		}
		if !Supported(path) && !Supported(oldPath) {
			continue
		}

		var before, after string
		var beforeOK, afterOK bool
		if file.Status != "A" {
			before, beforeOK, err = gitutil.ShowFile(ctx, repo, base, oldPath)
			if err != nil {
				return Result{}, err
			}
		}
		if file.Status != "D" {
			after, afterOK, err = gitutil.ShowFile(ctx, repo, head, path)
			if err != nil {
				return Result{}, err
			}
		}

		beforeEntities, language := parser.Parse(oldPath, before)
		afterEntities, afterLanguage := parser.Parse(path, after)
		if language == "" {
			language = afterLanguage
		}
		if !beforeOK {
			beforeEntities = nil
		}
		if !afterOK {
			afterEntities = nil
		}

		changes := Compare(beforeEntities, afterEntities)
		if len(changes) == 0 {
			continue
		}
		result.Files = append(result.Files, FileChange{
			Path:     path,
			OldPath:  file.OldPath,
			Status:   file.Status,
			Language: language,
			Changes:  changes,
		})
	}
	if err := addDependentCounts(ctx, repo, head, &result); err != nil {
		return Result{}, err
	}
	return result, nil
}

func AnalyzeCheckpoint(ctx context.Context, repo, checkpointID string) (Result, error) {
	head, err := gitutil.FindCommitWithCheckpoint(ctx, repo, checkpointID)
	if err != nil {
		return Result{}, err
	}
	base, err := gitutil.FirstParent(ctx, repo, head)
	if err != nil {
		return Result{}, err
	}
	result, err := AnalyzeGitRange(ctx, repo, base, head, nil)
	if err != nil {
		return Result{}, err
	}
	result.Checkpoint = checkpointID
	return result, nil
}

func Compare(before, after []Entity) []EntityChange {
	beforeByKey := map[string]Entity{}
	afterByKey := map[string]Entity{}
	for _, entity := range before {
		beforeByKey[key(entity)] = entity
	}
	for _, entity := range after {
		afterByKey[key(entity)] = entity
	}

	var changes []EntityChange
	deleted := map[string]Entity{}
	added := map[string]Entity{}

	for key, oldEntity := range beforeByKey {
		newEntity, ok := afterByKey[key]
		if !ok {
			deleted[key] = oldEntity
			continue
		}
		switch {
		case oldEntity.Signature != newEntity.Signature:
			changes = append(changes, EntityChange{
				Type:            "signature_changed",
				Kind:            oldEntity.Kind,
				Name:            oldEntity.Name,
				OldSignature:    oldEntity.Signature,
				NewSignature:    newEntity.Signature,
				BeforeStartLine: oldEntity.StartLine,
				AfterStartLine:  newEntity.StartLine,
			})
		case oldEntity.BodyHash != newEntity.BodyHash:
			changes = append(changes, EntityChange{
				Type:            "body_changed",
				Kind:            oldEntity.Kind,
				Name:            oldEntity.Name,
				BeforeStartLine: oldEntity.StartLine,
				AfterStartLine:  newEntity.StartLine,
			})
		}
	}
	for key, newEntity := range afterByKey {
		if _, ok := beforeByKey[key]; !ok {
			added[key] = newEntity
		}
	}

	for oldKey, oldEntity := range deleted {
		bestKey, bestEntity, score := bestRename(oldEntity, added)
		if score >= 0.92 {
			delete(deleted, oldKey)
			delete(added, bestKey)
			changes = append(changes, EntityChange{
				Type:            "renamed",
				Kind:            oldEntity.Kind,
				Name:            bestEntity.Name,
				OldName:         oldEntity.Name,
				NewName:         bestEntity.Name,
				OldSignature:    oldEntity.Signature,
				NewSignature:    bestEntity.Signature,
				BeforeStartLine: oldEntity.StartLine,
				AfterStartLine:  bestEntity.StartLine,
				Similarity:      score,
			})
		}
	}

	for _, oldEntity := range deleted {
		changes = append(changes, EntityChange{
			Type:            "removed",
			Kind:            oldEntity.Kind,
			Name:            oldEntity.Name,
			OldSignature:    oldEntity.Signature,
			BeforeStartLine: oldEntity.StartLine,
		})
	}
	for _, newEntity := range added {
		changes = append(changes, EntityChange{
			Type:           "added",
			Kind:           newEntity.Kind,
			Name:           newEntity.Name,
			NewSignature:   newEntity.Signature,
			AfterStartLine: newEntity.StartLine,
		})
	}

	sort.Slice(changes, func(i, j int) bool {
		left := lineForSort(changes[i])
		right := lineForSort(changes[j])
		if left == right {
			return fmt.Sprintf("%s:%s", changes[i].Kind, changes[i].Name) < fmt.Sprintf("%s:%s", changes[j].Kind, changes[j].Name)
		}
		return left < right
	})
	return changes
}

func key(entity Entity) string {
	return entity.Kind + ":" + entity.Name
}

func lineForSort(change EntityChange) int {
	if change.AfterStartLine > 0 {
		return change.AfterStartLine
	}
	return change.BeforeStartLine
}

func bestRename(old Entity, added map[string]Entity) (string, Entity, float64) {
	var bestKey string
	var best Entity
	var bestScore float64
	for key, candidate := range added {
		if candidate.Kind != old.Kind {
			continue
		}
		score := similarity(old, candidate)
		if score > bestScore {
			bestKey = key
			best = candidate
			bestScore = score
		}
	}
	return bestKey, best, bestScore
}

func similarity(a, b Entity) float64 {
	if a.Fingerprint != "" && a.Fingerprint == b.Fingerprint {
		return 1
	}
	if a.BodyHash != "" && a.BodyHash == b.BodyHash {
		return 0.97
	}
	return jaccard(a.Signature, b.Signature)
}

func jaccard(a, b string) float64 {
	left := tokenSet(a)
	right := tokenSet(b)
	if len(left) == 0 && len(right) == 0 {
		return 1
	}
	var intersection int
	for token := range left {
		if right[token] {
			intersection++
		}
	}
	union := len(left) + len(right) - intersection
	if union == 0 {
		return 0
	}
	return math.Round((float64(intersection)/float64(union))*100) / 100
}

func tokenSet(value string) map[string]bool {
	out := map[string]bool{}
	token := ""
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			token += string(r)
			continue
		}
		if token != "" {
			out[token] = true
			token = ""
		}
	}
	if token != "" {
		out[token] = true
	}
	return out
}
