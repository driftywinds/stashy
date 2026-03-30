package video

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/stashapp/stash/pkg/ffmpeg"
	"github.com/stashapp/stash/pkg/file"
	"github.com/stashapp/stash/pkg/fsutil"
	"github.com/stashapp/stash/pkg/logger"
	"github.com/stashapp/stash/pkg/models"
)

// Decorator adds video specific fields to a File.
type Decorator struct {
	FFProbe *ffmpeg.FFProbe
	FFMpeg  *ffmpeg.FFMpeg

	// CaptionRepo, if set, is used to immediately register extracted VTT
	// sidecar files in the database right after writing them.  Without this
	// the files are written to disk but the scanner's directory walk has
	// already finished, so AssociateCaptions is never called for them.
	CaptionRepo CaptionUpdater
	FileFinder  models.FileFinder
	TxnManager  models.TxnManager
}

func (d *Decorator) Decorate(ctx context.Context, fs models.FS, f models.File) (models.File, error) {
	if d.FFProbe == nil {
		return f, errors.New("ffprobe not configured")
	}

	base := f.Base()
	// TODO - copy to temp file if not an OsFS
	if _, isOs := fs.(*file.OsFS); !isOs {
		return f, fmt.Errorf("video.constructFile: only OsFS is supported")
	}

	probe := d.FFProbe
	videoFile, err := probe.NewVideoFile(base.Path)
	if err != nil {
		return f, fmt.Errorf("running ffprobe on %q: %w", base.Path, err)
	}

	container, err := ffmpeg.MatchContainer(videoFile.Container, base.Path)
	if err != nil {
		return f, fmt.Errorf("matching container for %q: %w", base.Path, err)
	}

	// check if there is a funscript file
	interactive := false
	if _, err := fs.Lstat(GetFunscriptPath(base.Path)); err == nil {
		interactive = true
	}

	// Extract embedded subtitle streams to VTT sidecar files.
	// After writing each file, immediately call AssociateCaptions so that
	// the captions are registered in the DB even though the directory walker
	// has already finished by the time extraction runs.
	if d.FFMpeg != nil && len(videoFile.SubtitleStreams) > 0 {
		extracted := extractEmbeddedSubtitles(ctx, d.FFMpeg, base.Path, videoFile.SubtitleStreams)
		if d.CaptionRepo != nil && d.FileFinder != nil && d.TxnManager != nil {
			for _, vttPath := range extracted {
				AssociateCaptions(ctx, vttPath, d.TxnManager, d.FileFinder, d.CaptionRepo)
			}
		}
	}

	return &models.VideoFile{
		BaseFile:    base,
		Format:      string(container),
		VideoCodec:  videoFile.VideoCodec,
		AudioCodec:  videoFile.AudioCodec,
		Width:       videoFile.Width,
		Height:      videoFile.Height,
		Duration:    videoFile.FileDuration,
		FrameRate:   videoFile.FrameRate,
		BitRate:     videoFile.Bitrate,
		Interactive: interactive,
	}, nil
}

func (d *Decorator) IsMissingMetadata(ctx context.Context, fs models.FS, f models.File) bool {
	const (
		unsetString = "unset"
		unsetNumber = -1
	)

	vf, ok := f.(*models.VideoFile)
	if !ok {
		return true
	}

	interactive := false
	if _, err := fs.Lstat(GetFunscriptPath(vf.Base().Path)); err == nil {
		interactive = true
	}

	return vf.VideoCodec == unsetString || vf.AudioCodec == unsetString ||
		vf.Format == unsetString || vf.Width == unsetNumber ||
		vf.Height == unsetNumber || vf.FrameRate == unsetNumber ||
		vf.Duration == unsetNumber ||
		vf.BitRate == unsetNumber || interactive != vf.Interactive
}

// extractEmbeddedSubtitles extracts each embedded subtitle stream from the
// video file as a VTT sidecar file placed alongside the video. Returns the
// list of VTT paths that were successfully written (skips already-existing
// files and streams that can't be converted).
func extractEmbeddedSubtitles(ctx context.Context, enc *ffmpeg.FFMpeg, videoPath string, streams []ffmpeg.FFProbeStream) []string {
	ext := filepath.Ext(videoPath)
	base := strings.TrimSuffix(videoPath, ext)

	// track which language codes we've already written so that when multiple
	// streams share the same language we append a numeric disambiguator.
	langCount := make(map[string]int)

	var written []string

	for _, s := range streams {
		lang := strings.ToLower(strings.TrimSpace(s.Tags.Language))

		// Normalise missing / invalid language codes.
		if lang == "" || lang == "und" || !IsValidLanguage(lang) {
			lang = LangUnknown
		}

		// Build the output path.
		var outPath string
		count := langCount[lang]
		langCount[lang]++

		if count == 0 {
			if lang == LangUnknown {
				outPath = base + ".vtt"
			} else {
				outPath = base + "." + lang + ".vtt"
			}
		} else {
			// Second (or later) stream for the same language – disambiguate.
			if lang == LangUnknown {
				outPath = fmt.Sprintf("%s.%d.vtt", base, count)
			} else {
				outPath = fmt.Sprintf("%s.%s.%d.vtt", base, lang, count)
			}
		}

		// Skip if already extracted (avoids re-running ffmpeg on every rescan).
		if exists, _ := fsutil.FileExists(outPath); exists {
			logger.Debugf("[subtitles] skipping already-extracted %s", outPath)
			// Still register it — DB entry may be missing from a previous scan.
			written = append(written, outPath)
			continue
		}

		logger.Infof("[subtitles] extracting stream #%d (%s) from %s → %s", s.Index, lang, videoPath, outPath)

		// ffmpeg -i <input> -map 0:<stream-index> -f webvtt <o>
		args := []string{
			"-hide_banner",
			"-loglevel", "error",
			"-i", videoPath,
			"-map", fmt.Sprintf("0:%d", s.Index),
			"-f", "webvtt",
			outPath,
		}

		cmd := enc.Command(ctx, args)
		out, err := cmd.CombinedOutput()
		if err != nil {
			// Some subtitle codecs (PGS/VOBSUB bitmap subs) cannot be
			// converted to WebVTT — log and move on rather than failing the
			// whole scan.
			logger.Warnf("[subtitles] ffmpeg could not extract stream #%d from %s: %v — %s", s.Index, videoPath, err, string(out))
			// Remove a partial output file if ffmpeg created one.
			_ = os.Remove(outPath)
			continue
		}

		written = append(written, outPath)
	}

	return written
