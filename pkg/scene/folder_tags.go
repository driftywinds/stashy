package scene

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/stashapp/stash/pkg/logger"
	"github.com/stashapp/stash/pkg/models"
)

// FolderTagWriterReader is the subset of the tag and scene repositories
// needed for folder-based tag auto-creation and assignment.
type FolderTagWriterReader interface {
	models.TagFinder
	models.TagCreator
	models.TagIDLoader
	models.SceneUpdater
}

// TagsFromFolderPath derives the ordered list of tag names that should be
// applied to a scene at filePath, given the library roots.
//
// Each tag name is the slash-joined relative path from the library root down
// to the scene's immediate parent folder, built up incrementally:
//
//   root:  /media
//   file:  /media/Movies/Action/Die Hard.mkv
//   tags:  ["Movies", "Movies/Action"]
//
// Files sitting directly in the root (no sub-folder) are skipped.
func TagsFromFolderPath(filePath string, libraryRoots []string) []string {
	for _, root := range libraryRoots {
		rel, err := filepath.Rel(root, filepath.Dir(filePath))
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			continue
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		tags := make([]string, 0, len(parts))
		for i := range parts {
			tags = append(tags, strings.Join(parts[:i+1], "/"))
		}
		return tags
	}
	return nil
}

// EnsureFolderTags creates (if absent) one tag per element of tagNames,
// wiring each tag's parent to the tag for the previous element so the tag
// tree mirrors the folder hierarchy. Returns IDs in order (shallowest first).
func EnsureFolderTags(ctx context.Context, rw FolderTagWriterReader, tagNames []string) ([]int, error) {
	ids := make([]int, 0, len(tagNames))
	var parentID *int

	for _, name := range tagNames {
		existing, err := rw.FindByName(ctx, name, false)
		if err != nil {
			return nil, fmt.Errorf("finding tag %q: %w", name, err)
		}

		if existing != nil {
			ids = append(ids, existing.ID)
			pid := existing.ID
			parentID = &pid
			continue
		}

		// Build the leaf alias (last path segment) for cleaner display.
		leaf := name
		if idx := strings.LastIndex(name, "/"); idx >= 0 {
			leaf = name[idx+1:]
		}

		now := time.Now()
		newTag := &models.Tag{
			Name:      name,
			CreatedAt: now,
			UpdatedAt: now,
		}
		if parentID != nil {
			newTag.ParentIDs = models.NewRelatedIDs([]int{*parentID})
		}
		if leaf != name {
			newTag.Aliases = models.NewRelatedStrings([]string{leaf})
		}

		input := &models.CreateTagInput{Tag: newTag}
		if err := rw.Create(ctx, input); err != nil {
			return nil, fmt.Errorf("creating folder tag %q: %w", name, err)
		}
		logger.Infof("[folder-tags] created tag %q (id=%d)", name, newTag.ID)

		ids = append(ids, newTag.ID)
		pid := newTag.ID
		parentID = &pid
	}
	return ids, nil
}

// AssignFolderTags ensures the folder-derived tags exist and adds any missing
// ones to the scene's tag list without removing manually-added tags.
func AssignFolderTags(ctx context.Context, s *models.Scene, rw FolderTagWriterReader, libraryRoots []string) error {
	if s.Path == "" {
		return nil
	}

	tagNames := TagsFromFolderPath(s.Path, libraryRoots)
	if len(tagNames) == 0 {
		return nil
	}

	tagIDs, err := EnsureFolderTags(ctx, rw, tagNames)
	if err != nil {
		return err
	}

	if err := s.LoadTagIDs(ctx, rw); err != nil {
		return fmt.Errorf("loading tag IDs for scene %d: %w", s.ID, err)
	}
	existing := s.TagIDs.List()

	existingSet := make(map[int]struct{}, len(existing))
	for _, id := range existing {
		existingSet[id] = struct{}{}
	}

	var toAdd []int
	for _, id := range tagIDs {
		if _, found := existingSet[id]; !found {
			toAdd = append(toAdd, id)
		}
	}
	if len(toAdd) == 0 {
		return nil
	}

	partial := models.ScenePartial{
		TagIDs: &models.UpdateIDs{
			IDs:  append(existing, toAdd...),
			Mode: models.RelationshipUpdateModeSet,
		},
	}
	if _, err := rw.UpdatePartial(ctx, s.ID, partial); err != nil {
		return fmt.Errorf("updating scene %d tags: %w", s.ID, err)
	}
	logger.Debugf("[folder-tags] assigned %v to scene %d", tagNames, s.ID)
	return nil
}
