package file

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/stashapp/stash/pkg/logger"
	"github.com/stashapp/stash/pkg/models"
	"github.com/stashapp/stash/pkg/txn"
)

// Scanner scans files into the database.
type Scanner struct {
	FS                    models.FS
	Repository            Repository
	FingerprintCalculator FingerprintCalculator

	// ZipFileExtensions is a list of file extensions that are considered zip files.
	// Extension does not include the . character.
	ZipFileExtensions []string

	// ScanFilters are used to determine if a file should be scanned.
	ScanFilters []PathFilter

	// HandlerRequiredFilters are used to determine if an unchanged file needs to be handled
	HandlerRequiredFilters []Filter

	// FileDecorators are applied to files as they are scanned.
	FileDecorators []Decorator

	// handlers are called after a file has been scanned.
	FileHandlers []Handler

	// RootPaths form the top-level paths for the library.
	// Used to determine the root of the folder hierarchy when creating folders.
	RootPaths []string

	// Rescan indicates whether files should be rescanned even if they haven't changed.
	Rescan bool

	folderPathToID sync.Map
}

// FingerprintCalculator calculates a fingerprint for the provided file.
type FingerprintCalculator interface {
	CalculateFingerprints(f *models.BaseFile, o Opener, useExisting bool) ([]models.Fingerprint, error)
}

// Decorator wraps the Decorate method to add additional functionality while scanning files.
type Decorator interface {
	Decorate(ctx context.Context, fs models.FS, f models.File) (models.File, error)
	IsMissingMetadata(ctx context.Context, fs models.FS, f models.File) bool
}

type FilteredDecorator struct {
	Decorator
	Filter
}

// Decorate runs the decorator if the filter accepts the file.
func (d *FilteredDecorator) Decorate(ctx context.Context, fs models.FS, f models.File) (models.File, error) {
	if d.Accept(ctx, f) {
		return d.Decorator.Decorate(ctx, fs, f)
	}
	return f, nil
}

func (d *FilteredDecorator) IsMissingMetadata(ctx context.Context, fs models.FS, f models.File) bool {
	if d.Accept(ctx, f) {
		return d.Decorator.IsMissingMetadata(ctx, fs, f)
	}

	return false
}

// ScannedFile represents a file being scanned.
type ScannedFile struct {
	*models.BaseFile
	FS   models.FS
	Info fs.FileInfo
}

// AcceptEntry determines if the file entry should be accepted for scanning
func (s *Scanner) AcceptEntry(ctx context.Context, path string, info fs.FileInfo, zipFilePath string) bool {
	// always accept if there's no filters
	accept := len(s.ScanFilters) == 0
	for _, filter := range s.ScanFilters {
		// accept if any filter accepts the file
		if filter.Accept(ctx, path, info, zipFilePath) {
			accept = true
			break
		}
	}

	return accept
}

func (s *Scanner) getFolderID(ctx context.Context, path string) (*models.FolderID, error) {
	// check the folder cache first
	if f, ok := s.folderPathToID.Load(path); ok {
		v := f.(models.FolderID)
		return &v, nil
	}

	// assume case sensitive when searching for the folder
	const caseSensitive = true

	ret, err := s.Repository.Folder.FindByPath(ctx, path, caseSensitive)
	if err != nil {
		return nil, err
	}

	if ret == nil {
		return nil, nil
	}

	s.folderPathToID.Store(path, ret.ID)
	return &ret.ID, nil
}

// ScanFolder scans the provided folder into the database, returning the folder entry.
func (s *Scanner) ScanFolder(ctx context.Context, file ScannedFile) (*models.Folder, error) {
	var f *models.Folder
	var err error
	path := file.Path

	err = s.Repository.WithTxn(ctx, func(ctx context.Context) error {
		f, err = s.Repository.Folder.FindByPath(ctx, path, true)
		if err != nil {
			return fmt.Errorf("checking for existing folder %q: %w", path, err)
		}

		if f == nil && file.ZipFileID == nil {
			caseSensitive, _ := file.FS.IsPathCaseSensitive(file.Path)

			if !caseSensitive {
				f, err = s.Repository.Folder.FindByPath(ctx, path, false)
				if err != nil {
					return fmt.Errorf("checking for existing folder %q: %w", path, err)
				}
			}
		}

		if f == nil {
			f, err = s.onNewFolder(ctx, file)
		} else {
			f, err = s.onExistingFolder(ctx, file, f)
		}

		if err != nil {
			return err
		}

		if f != nil {
			s.folderPathToID.Store(f.Path, f.ID)
		}

		return nil
	})

	return f, err
}

func (s *Scanner) isRootPath(path string) bool {
	return path == "." || slices.Contains(s.RootPaths, path)
}

func (s *Scanner) onNewFolder(ctx context.Context, file ScannedFile) (*models.Folder, error) {
	renamed, err := s.handleFolderRename(ctx, file)
	if err != nil {
		return nil, err
	}

	if renamed != nil {
		return renamed, nil
	}

	now := time.Now()

	toCreate := &models.Folder{
		DirEntry:  file.DirEntry,
		Path:      file.Path,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if !s.isRootPath(file.Path) {
		dir := filepath.Dir(file.Path)
		parentFolder, err := GetOrCreateFolderHierarchy(ctx, s.Repository.Folder, dir, s.RootPaths)
		if err != nil {
			return nil, fmt.Errorf("getting parent folder %q: %w", dir, err)
		}
		toCreate.ParentFolderID = &parentFolder.ID
	}

	txn.AddPostCommitHook(ctx, func(ctx context.Context) {
		logger.Infof("%s doesn't exist. Creating new folder entry...", file.Path)
	})

	if err := s.Repository.Folder.Create(ctx, toCreate); err != nil {
		return nil, fmt.Errorf("creating folder %q: %w", file.Path, err)
	}

	return toCreate, nil
}

func (s *Scanner) handleFolderRename(ctx context.Context, file ScannedFile) (*models.Folder, error) {
	if file.ZipFileID != nil {
		return nil, nil
	}

	renamedFrom, err := s.detectFolderMove(ctx, file)
	if err != nil {
		return nil, fmt.Errorf("detecting folder move: %w", err)
	}

	if renamedFrom == nil {
		return nil, nil
	}

	logger.Infof("%s moved to %s. Updating path...", renamedFrom.Path, file.Path)
	renamedFrom.Path = file.Path

	parentFolderID, err := s.getFolderID(ctx, filepath.Dir(file.Path))
	if err != nil {
		return nil, fmt.Errorf("getting parent folder for %q: %w", file.Path, err)
	}

	renamedFrom.ParentFolderID = parentFolderID

	if err := s.Repository.Folder.Update(ctx, renamedFrom); err != nil {
		return nil, fmt.Errorf("updating folder for rename %q: %w", renamedFrom.Path, err)
	}

	if err := correctSubFolderHierarchy(ctx, s.Repository.Folder, renamedFrom); err != nil {
		return nil, fmt.Errorf("correcting sub folder hierarchy for %q: %w", renamedFrom.Path, err)
	}

	return renamedFrom, nil
}

func (s *Scanner) onExistingFolder(ctx context.Context, f ScannedFile, existing *models.Folder) (*models.Folder, error) {
	update := false
	entryModTime := f.ModTime
	if !entryModTime.Equal(existing.ModTime) {
		existing.Path = f.Path
		existing.ModTime = entryModTime
		update = true
	}

	if existing.Path != f.Path {
		existing.Path = f.Path
		update = true
	}

	fZfID := f.ZipFileID
	existingZfID := existing.ZipFileID
	if fZfID != existingZfID {
		if fZfID == nil {
			existing.ZipFileID = nil
			update = true
		} else if existingZfID == nil || *fZfID != *existingZfID {
			existing.ZipFileID = fZfID
			update = true
		}
	}

	if existing.ParentFolderID == nil && !s.isRootPath(existing.Path) {
		logger.Infof("Existing folder entry %q has no parent folder. Creating folder hierarchy and setting parent ID...", existing.Path)
		parentFolder, err := GetOrCreateFolderHierarchy(ctx, s.Repository.Folder, filepath.Dir(f.Path), s.RootPaths)
		if err != nil {
			return nil, fmt.Errorf("getting parent folder for %q: %w", f.Path, err)
		}
		existing.ParentFolderID = &parentFolder.ID
		update = true
	}

	if update {
		if err := s.Repository.Folder.Update(ctx, existing); err != nil {
			return nil, fmt.Errorf("updating folder %q: %w", f.Path, err)
		}
	}

	return existing, nil
}

type ScanFileResult struct {
	File               models.File
	New                bool
	Renamed            bool
	Updated            bool
	FingerprintChanged bool
}

func (r ScanFileResult) IsUnchanged() bool {
	return !r.New && !r.Renamed && !r.Updated
}

func (s *Scanner) ScanFile(ctx context.Context, f ScannedFile) (*ScanFileResult, error) {
	var r *ScanFileResult

	if err := s.Repository.WithDB(ctx, func(ctx context.Context) error {
		ff, err := s.Repository.File.FindByPath(ctx, f.Path, true)
		if err != nil {
			return fmt.Errorf("checking for existing file %q: %w", f.Path, err)
		}

		if ff == nil && f.ZipFileID != nil {
			caseSensitive, _ := f.FS.IsPathCaseSensitive(f.Path)
			if !caseSensitive {
				ff, err = s.Repository.File.FindByPath(ctx, f.Path, false)
				if err != nil {
					return fmt.Errorf("checking for existing file %q: %w", f.Path, err)
				}
			}
		}

		if ff == nil {
			r, err = s.onNewFile(ctx, f)
			return err
		}

		r, err = s.onExistingFile(ctx, f, ff)
		return err
	}); err != nil {
		return nil, err
	}

	return r, nil
}

func (s *Scanner) IsZipFile(path string) bool {
	fExt := filepath.Ext(path)
	for _, ext := range s.ZipFileExtensions {
		if strings.EqualFold(fExt, "."+ext) {
			return true
		}
	}
	return false
}

func (s *Scanner) onNewFile(ctx context.Context, f ScannedFile) (*ScanFileResult, error) {
	now := time.Now()
	baseFile := f.BaseFile
	path := baseFile.Path

	baseFile.CreatedAt = now
	baseFile.UpdatedAt = now

	folderPath := filepath.Dir(path)
	parentFolderID, err := s.getFolderID(ctx, folderPath)
	if err != nil {
		return nil, fmt.Errorf("getting parent folder for %q: %w", path, err)
	}

	if parentFolderID == nil {
		if err := s.Repository.WithTxn(ctx, func(ctx context.Context) error {
			parentFolder, err := GetOrCreateFolderHierarchy(ctx, s.Repository.Folder, folderPath, s.RootPaths)
			if err != nil {
				return fmt.Errorf("getting parent folder for %q: %w", f.Path, err)
			}
			parentFolderID = &parentFolder.ID
			return nil
		}); err != nil {
			return nil, err
		}
	}
	if parentFolderID == nil {
		return nil, fmt.Errorf("parent folder ID is nil for %q", path)
	}

	baseFile.ParentFolderID = *parentFolderID

	const useExisting = false
	fp, err := s.calculateFingerprints(f.FS, baseFile, path, useExisting)
	if err != nil {
		return nil, err
	}

	baseFile.SetFingerprints(fp)

	file, err := s.fireDecorators(ctx, f.FS, baseFile)
	if err != nil {
		return nil, err
	}

	zipFilePath := ""
	if f.ZipFile != nil {
		zipFilePath = f.ZipFile.Base().Path
	}
	renamed, err := s.handleRename(ctx, file, fp, zipFilePath)
	if err != nil {
		return nil, err
	}

	if renamed != nil {
		return &ScanFileResult{
			File:    renamed,
			Renamed: true,
		}, nil
	}

	if err := s.Repository.WithTxn(ctx, func(ctx context.Context) error {
		if err := s.Repository.File.Create(ctx, file); err != nil {
			return fmt.Errorf("creating file %q: %w", path, err)
		}
		return s.fireHandlers(ctx, file, nil)
	}); err != nil {
		return nil, err
	}

	return &ScanFileResult{
		File: file,
		New:  true,
	}, nil
}

func (s *Scanner) fireDecorators(ctx context.Context, fs models.FS, f models.File) (models.File, error) {
	for _, h := range s.FileDecorators {
		var err error
		f, err = h.Decorate(ctx, fs, f)
		if err != nil {
			return f, err
		}
	}
	return f, nil
}

func (s *Scanner) fireHandlers(ctx context.Context, f models.File, oldFile models.File) error {
	for _, h := range s.FileHandlers {
		if err := h.Handle(ctx, f, oldFile); err != nil {
			return err
		}
	}
	return nil
}

func (s *Scanner) calculateFingerprints(fs models.FS, f *models.BaseFile, path string, useExisting bool) (models.Fingerprints, error) {
	if !useExisting {
		logger.Infof("Calculating fingerprints for %s ...", path)
	}

	fp, err := s.FingerprintCalculator.CalculateFingerprints(f, &fsOpener{
		fs:   fs,
		name: path,
	}, useExisting)
	if err != nil {
		return nil, fmt.Errorf("calculating fingerprint for file %q: %w", path, err)
	}

	return fp, nil
}

func appendFileUnique(v []models.File, toAdd []models.File) []models.File {
	for _, f := range toAdd {
		found := false
		id := f.Base().ID
		for _, vv := range v {
			if vv.Base().ID == id {
				found = true
				break
			}
		}
		if !found {
			v = append(v, f)
		}
	}
	return v
}

func (s *Scanner) getFileFS(f *models.BaseFile) (models.FS, error) {
	if f.ZipFile == nil {
		return s.FS, nil
	}

	fs, err := s.getFileFS(f.ZipFile.Base())
	if err != nil {
		return nil, err
	}

	zipPath := f.ZipFile.Base().Path
	zipSize := f.ZipFile.Base().Size
	return fs.OpenZip(zipPath, zipSize)
}

func (s *Scanner) handleRename(ctx context.Context, f models.File, fp []models.Fingerprint, zipFilePath string) (models.File, error) {
	var others []models.File
	for _, tfp := range fp {
		thisOthers, err := s.Repository.File.FindByFingerprint(ctx, tfp)
		if err != nil {
			return nil, fmt.Errorf("getting files by fingerprint %v: %w", tfp, err)
		}
		others = appendFileUnique(others, thisOthers)
	}

	var missing []models.File
	fZipID := f.Base().ZipFileID
	for _, other := range others {
		otherZipID := other.Base().ZipFileID
		if otherZipID != nil && (fZipID == nil || *otherZipID != *fZipID) {
			continue
		}

		fs, err := s.getFileFS(other.Base())
		if err != nil {
			missing = append(missing, other)
			continue
		}

		info, err := fs.Lstat(other.Base().Path)
		switch {
		case err != nil:
			missing = append(missing, other)
		case !s.AcceptEntry(ctx, other.Base().Path, info, zipFilePath):
			logger.Debugf("File %q no longer in library paths. Treating as a move.", other.Base().Path)
			missing = append(missing, other)
		}
	}

	if len(missing) == 0 {
		return nil, nil
	}

	other := missing[0]
	updated := other.Clone()
	updatedBase := updated.Base()
	fBaseCopy := *(f.Base())

	oldPath := updatedBase.Path
	newPath := fBaseCopy.Path

	logger.Infof("%s moved to %s. Updating path...", oldPath, newPath)
	fBaseCopy.ID = updatedBase.ID
	fBaseCopy.CreatedAt = updatedBase.CreatedAt
	fBaseCopy.Fingerprints = updatedBase.Fingerprints
	*updatedBase = fBaseCopy

	zipMover := zipHierarchyMover{
		folderStore: s.Repository.Folder,
		files:       s.Repository.File,
		rootPaths:   s.RootPaths,
	}

	if err := s.Repository.WithTxn(ctx, func(ctx context.Context) error {
		if err := s.Repository.File.Update(ctx, updated); err != nil {
			return fmt.Errorf("updating file for rename %q: %w", newPath, err)
		}

		if s.IsZipFile(updatedBase.Basename) {
			if err := zipMover.transferZipHierarchy(ctx, updatedBase.ID, oldPath, newPath); err != nil {
				return fmt.Errorf("moving zip hierarchy for renamed zip file %q: %w", newPath, err)
			}
		}
		return s.fireHandlers(ctx, updated, other)
	}); err != nil {
		return nil, err
	}

	return updated, nil
}

func (s *Scanner) isHandlerRequired(ctx context.Context, f models.File) bool {
	accept := len(s.HandlerRequiredFilters) == 0
	for _, filter := range s.HandlerRequiredFilters {
		if filter.Accept(ctx, f) {
			accept = true
			break
		}
	}
	return accept
}

func (s *Scanner) isMissingMetadata(ctx context.Context, f ScannedFile, existing models.File) bool {
	for _, h := range s.FileDecorators {
		if h.IsMissingMetadata(ctx, f.FS, existing) {
			return true
		}
	}
	return false
}

func (s *Scanner) setMissingMetadata(ctx context.Context, f ScannedFile, existing models.File) (models.File, error) {
	path := existing.Base().Path
	logger.Infof("Updating metadata for %s", path)
	existing.Base().Size = f.Size

	var err error
	existing, err = s.fireDecorators(ctx, f.FS, existing)
	if err != nil {
		return nil, err
	}

	if err := s.Repository.WithTxn(ctx, func(ctx context.Context) error {
		if err := s.Repository.File.Update(ctx, existing); err != nil {
			return fmt.Errorf("updating file %q: %w", path, err)
		}
		return nil
	}); err != nil {
		return nil, err
	}

	return existing, nil
}

func (s *Scanner) setMissingFingerprints(ctx context.Context, f ScannedFile, existing models.File) (models.File, error) {
	const useExisting = true
	fp, err := s.calculateFingerprints(f.FS, existing.Base(), f.Path, useExisting)
	if err != nil {
		return nil, err
	}

	if fp.ContentsChanged(existing.Base().Fingerprints) {
		existing.SetFingerprints(fp)
		if err := s.Repository.WithTxn(ctx, func(ctx context.Context) error {
			if err := s.Repository.File.Update(ctx, existing); err != nil {
				return fmt.Errorf("updating file %q: %w", f.Path, err)
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}

	return existing, nil
}

func (s *Scanner) onExistingFile(ctx context.Context, f ScannedFile, existing models.File) (*ScanFileResult, error) {
	base := existing.Base()
	path := base.Path

	fileModTime := f.ModTime
	updated := !fileModTime.Equal(base.ModTime) || base.Basename != f.Basename
	forceRescan := s.Rescan

	if !updated && !forceRescan {
		return s.onUnchangedFile(ctx, f, existing)
	}

	oldBase := *base
	if !updated && forceRescan {
		logger.Infof("rescanning %s", path)
	} else {
		logger.Infof("%s has been updated: rescanning", path)
	}

	base.Basename = f.Basename
	base.ModTime = fileModTime
	base.Size = f.Size
	base.UpdatedAt = time.Now()

	const useExisting = false
	fp, err := s.calculateFingerprints(f.FS, base, path, useExisting)
	if err != nil {
		return nil, err
	}

	fingerprintChanged := fp.ContentsChanged(existing.Base().Fingerprints)
	s.removeOutdatedFingerprints(existing, fp)
	existing.SetFingerprints(fp)

	existing, err = s.fireDecorators(ctx, f.FS, existing)
	if err != nil {
		return nil, err
	}

	if err := s.Repository.WithTxn(ctx, func(ctx context.Context) error {
		if err := s.Repository.File.Update(ctx, existing); err != nil {
			return fmt.Errorf("updating file %q: %w", path, err)
		}
		return s.fireHandlers(ctx, existing, &oldBase)
	}); err != nil {
		return nil, err
	}

	return &ScanFileResult{
		File:               existing,
		Updated:            true,
		FingerprintChanged: fingerprintChanged,
	}, nil
}

func (s *Scanner) removeOutdatedFingerprints(existing models.File, fp models.Fingerprints) {
	oshash := fp.For(models.FingerprintTypeOshash)
	if oshash == nil {
		return
	}

	existingOshash := existing.Base().Fingerprints.For(models.FingerprintTypeOshash)
	if existingOshash == nil || *existingOshash == *oshash {
		return
	}

	if fp.For(models.FingerprintTypeMD5) != nil {
		return
	}

	logger.Infof("Removing outdated checksum from %s", existing.Base().Path)
	b := existing.Base()
	b.Fingerprints = b.Fingerprints.Remove(models.FingerprintTypeMD5)
}

func (s *Scanner) onUnchangedFile(ctx context.Context, f ScannedFile, existing models.File) (*ScanFileResult, error) {
	var err error
	isMissingMetdata := s.isMissingMetadata(ctx, f, existing)
	if isMissingMetdata {
		existing, err = s.setMissingMetadata(ctx, f, existing)
		if err != nil {
			return nil, err
		}
	}

	existing, err = s.setMissingFingerprints(ctx, f, existing)
	if err != nil {
		return nil, err
	}

	handlerRequired := false
	if err := s.Repository.WithDB(ctx, func(ctx context.Context) error {
		handlerRequired = s.isHandlerRequired(ctx, existing)
		return nil
	}); err != nil {
		return nil, err
	}

	if !handlerRequired {
		if isMissingMetdata {
			return &ScanFileResult{File: existing, Updated: true}, nil
		}
		return &ScanFileResult{File: existing}, nil
	}

	if err := s.Repository.WithTxn(ctx, func(ctx context.Context) error {
		return s.fireHandlers(ctx, existing, nil)
	}); err != nil {
		return nil, err
	}

	return &ScanFileResult{File: existing, Updated: true}, nil
}