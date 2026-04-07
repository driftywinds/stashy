package scene

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
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

// folderGroupCache tracks group IDs and parent→child relationships that have
// already been written to the DB within a single scan pass, so we never attempt
// to INSERT a duplicate groups_relations row.
//
// The scan runs many files concurrently but all DB writes happen inside a
// transaction on one goroutine, so a plain (non-concurrent) map is fine here.
// We expose it as a value type so callers can allocate it on the stack and it
// is automatically discarded when the scan finishes.
type folderGroupCache struct {
	mu sync.Mutex
	// nameToID maps bare directory name → group ID
	nameToID map[string]int
	// parentLinks tracks "child-id:parent-id" pairs already inserted
	parentLinks map[string]struct{}
}

func newFolderGroupCache() *folderGroupCache {
	return &folderGroupCache{
		nameToID:    make(map[string]int),
		parentLinks: make(map[string]struct{}),
	}
}

func (c *folderGroupCache) getID(name string) (int, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id, ok := c.nameToID[name]
	return id, ok
}

func (c *folderGroupCache) setID(name string, id int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nameToID[name] = id
}

func (c *folderGroupCache) hasLink(childID, parentID int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := fmt.Sprintf("%d:%d", childID, parentID)
	_, ok := c.parentLinks[key]
	return ok
}

func (c *folderGroupCache) markLink(childID, parentID int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.parentLinks[fmt.Sprintf("%d:%d", childID, parentID)] = struct{}{}
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
// cache must be a *folderGroupCache shared across all calls within a single
// scan; it prevents duplicate INSERT attempts for already-established
// containing-group relationships, which would otherwise cause a UNIQUE
// constraint error in the DB.
func EnsureFolderGroups(ctx context.Context, groupRW FolderGroupManager, groupNames []string, cache *folderGroupCache) ([]int, error) {
	ids := make([]int, 0, len(groupNames))
	var parentID *int

	for _, name := range groupNames {
		// Check cache first to avoid a DB round-trip on every episode.
		var groupID int
		if cachedID, ok := cache.getID(name); ok {
			groupID = cachedID
		} else {
			existing, err := groupRW.FindByName(ctx, name, false)
			if err != nil {
				return nil, fmt.Errorf("finding group %q: %w", name, err)
			}

			if existing != nil {
				groupID = existing.ID
				cache.setID(name, groupID)
			} else {
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
				groupID = newGroup.ID
				cache.setID(name, groupID)

				// The containing link was set during Create; record it so we
				// never try to add it again.
				if parentID != nil {
					cache.markLink(groupID, *parentID)
				}
			}
		}

		// Wire the containing-group relationship for existing groups that may
		// have been created in a previous scan without this parent, but only
		// if we haven't already done so in this scan pass.
		if parentID != nil && !cache.hasLink(groupID, *parentID) {
			if err := addContainingGroup(ctx, groupRW, groupID, *parentID); err != nil {
				return nil, err
			}
			cache.markLink(groupID, *parentID)
		}

		ids = append(ids, groupID)
		pid := groupID
		parentID = &pid
	}
	return ids, nil
}

// addContainingGroup adds parentID as a containing group of childID.
// This must only be called when the relationship is known not to exist yet
// (enforced by the cache in EnsureFolderGroups).
func addContainingGroup(ctx context.Context, groupRW FolderGroupManager, childID, parentID int) error {
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
// cache must be shared across all AssignFolderGroups calls within a single scan.
func AssignFolderGroups(ctx context.Context, s *models.Scene, groupRW FolderGroupManager, sceneRW FolderSceneGroupUpdater, libraryRoots []string, cache *folderGroupCache) error {
	if s.Path == "" {
		return nil
	}

	groupNames := GroupsFromFolderPath(s.Path, libraryRoots)
	if len(groupNames) == 0 {
		return nil
	}

	groupIDs, err := EnsureFolderGroups(ctx, groupRW, groupNames, cache)
	if err != nil {
		return err
	}

	// Assign only the leaf (deepest/most-specific) group to the scene.
	// The hierarchy is expressed via containing-groups on the groups themselves.
	leafID := groupIDs[len(groupIDs)-1]

	existing, err := sceneRW.GetGroups(ctx, s.ID)
	if err != nil {
		return fmt.Errorf("loading groups for scene %d: %w", s.ID, err)
	}

	for _, gs := range existing {
		if gs.GroupID == leafID {
			return nil // already assigned
		}
	}

	partial := models.ScenePartial{
		GroupIDs: &models.UpdateGroupIDs{
			Groups: []models.GroupsScenes{{GroupID: leafID}},
			Mode:   models.RelationshipUpdateModeAdd,
		},
	}
	if _, err := sceneRW.UpdatePartial(ctx, s.ID, partial); err != nil {
		return fmt.Errorf("updating scene %d groups: %w", s.ID, err)
	}
	logger.Debugf("[folder-groups] assigned leaf group %q (id=%d) to scene %d", groupNames[len(groupNames)-1], leafID, s.ID)
	return nil
}