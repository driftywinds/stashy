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

// FolderGroupManager is the group-side repository needed for folder group creation.
type FolderGroupManager interface {
	models.GroupFinder
	models.GroupCreator
	models.GroupUpdater
}

// FolderSceneGroupUpdater is the scene-side repository needed for folder group assignment.
type FolderSceneGroupUpdater interface {
	GetGroups(ctx context.Context, sceneID int) ([]models.GroupsScenes, error)
	UpdatePartial(ctx context.Context, id int, updatedScene models.ScenePartial) (*models.Scene, error)
}

// GroupsFromFolderPath derives the ordered list of directory names that should
// map to groups for a scene at filePath, given the library roots.
//
// Each entry is just the bare directory name (not a full path), in order from
// the library root down to the scene's immediate parent folder:
//
//	root:  /media
//	file:  /media/Movies/Action/Die Hard.mkv
//	names: ["Movies", "Action"]
//
// Files sitting directly in the root (no sub-folder) produce no groups.
func GroupsFromFolderPath(filePath string, libraryRoots []string) []string {
	for _, root := range libraryRoots {
		rel, err := filepath.Rel(root, filepath.Dir(filePath))
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			continue
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		// Return a copy so callers cannot mutate the slice.
		result := make([]string, len(parts))
		copy(result, parts)
		return result
	}
	return nil
}

// EnsureFolderGroups creates (if absent) one group per element of groupNames,
// wiring each group's containing group to the group for the previous element so
// the group hierarchy mirrors the folder hierarchy.
//
// Group names are the bare directory names. Returns group IDs in order
// (shallowest first).
func EnsureFolderGroups(ctx context.Context, groupRW FolderGroupManager, groupNames []string) ([]int, error) {
	ids := make([]int, 0, len(groupNames))
	var parentID *int

	for _, name := range groupNames {
		existing, err := groupRW.FindByName(ctx, name, false)
		if err != nil {
			return nil, fmt.Errorf("finding group %q: %w", name, err)
		}

		if existing != nil {
			ids = append(ids, existing.ID)
			pid := existing.ID
			// If this group exists but is missing the containing-group link,
			// add it now so the hierarchy stays correct.
			if parentID != nil {
				if err := ensureContainingGroup(ctx, groupRW, existing.ID, *parentID); err != nil {
					return nil, err
				}
			}
			parentID = &pid
			continue
		}

		now := time.Now()
		newGroup := &models.Group{
			Name:      name,
			CreatedAt: now,
			UpdatedAt: now,
		}
		if parentID != nil {
			newGroup.ContainingGroups = models.NewRelatedGroupDescriptions([]models.GroupIDDescription{
				{GroupID: *parentID},
			})
		}

		if err := groupRW.Create(ctx, newGroup); err != nil {
			return nil, fmt.Errorf("creating folder group %q: %w", name, err)
		}
		logger.Infof("[folder-groups] created group %q (id=%d)", name, newGroup.ID)

		ids = append(ids, newGroup.ID)
		pid := newGroup.ID
		parentID = &pid
	}
	return ids, nil
}

// ensureContainingGroup adds parentID as a containing group of childID if the
// relationship does not already exist.
func ensureContainingGroup(ctx context.Context, groupRW FolderGroupManager, childID, parentID int) error {
	child, err := groupRW.Find(ctx, childID)
	if err != nil {
		return fmt.Errorf("finding group %d: %w", childID, err)
	}
	if child == nil {
		return fmt.Errorf("group %d not found", childID)
	}

	// Check if the containing relationship already exists.
	if child.ContainingGroups.Loaded() {
		for _, desc := range child.ContainingGroups.List() {
			if desc.GroupID == parentID {
				return nil // already present
			}
		}
	}

	partial := models.GroupPartial{
		ContainingGroups: &models.UpdateGroupDescriptions{
			Groups: []models.GroupIDDescription{{GroupID: parentID}},
			Mode:   models.RelationshipUpdateModeAdd,
		},
	}
	if _, err := groupRW.UpdatePartial(ctx, childID, partial); err != nil {
		return fmt.Errorf("adding containing group %d to group %d: %w", parentID, childID, err)
	}
	return nil
}

// AssignFolderGroups ensures the folder-derived groups exist and adds any
// missing ones to the scene's group list without removing manually-added groups.
//
// groupRW is typically r.Group (the group store).
// sceneRW is typically r.Scene (the scene store).
func AssignFolderGroups(ctx context.Context, s *models.Scene, groupRW FolderGroupManager, sceneRW FolderSceneGroupUpdater, libraryRoots []string) error {
	if s.Path == "" {
		return nil
	}

	groupNames := GroupsFromFolderPath(s.Path, libraryRoots)
	if len(groupNames) == 0 {
		return nil
	}

	groupIDs, err := EnsureFolderGroups(ctx, groupRW, groupNames)
	if err != nil {
		return err
	}

	// Load existing scene groups.
	existing, err := sceneRW.GetGroups(ctx, s.ID)
	if err != nil {
		return fmt.Errorf("loading groups for scene %d: %w", s.ID, err)
	}

	existingSet := make(map[int]struct{}, len(existing))
	for _, gs := range existing {
		existingSet[gs.GroupID] = struct{}{}
	}

	var toAdd []models.GroupsScenes
	for _, id := range groupIDs {
		if _, found := existingSet[id]; !found {
			toAdd = append(toAdd, models.GroupsScenes{GroupID: id})
		}
	}
	if len(toAdd) == 0 {
		return nil
	}

	partial := models.ScenePartial{
		GroupIDs: &models.UpdateGroupIDs{
			Groups: toAdd,
			Mode:   models.RelationshipUpdateModeAdd,
		},
	}
	if _, err := sceneRW.UpdatePartial(ctx, s.ID, partial); err != nil {
		return fmt.Errorf("updating scene %d groups: %w", s.ID, err)
	}
	logger.Debugf("[folder-groups] assigned groups %v to scene %d", groupNames, s.ID)
	return nil
}
