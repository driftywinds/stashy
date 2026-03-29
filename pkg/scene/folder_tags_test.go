package scene

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTagsFromFolderPath(t *testing.T) {
	tests := []struct {
		name         string
		filePath     string
		roots        []string
		expectedTags []string
	}{
		{
			name:         "single level deep",
			filePath:     "/media/Movies/Inception.mkv",
			roots:        []string{"/media"},
			expectedTags: []string{"Movies"},
		},
		{
			name:         "two levels deep",
			filePath:     "/media/Movies/Action/Die Hard.mkv",
			roots:        []string{"/media"},
			expectedTags: []string{"Movies", "Movies/Action"},
		},
		{
			name:         "three levels deep",
			filePath:     "/media/TV/Drama/Breaking Bad/S01E01.mkv",
			roots:        []string{"/media"},
			expectedTags: []string{"TV", "TV/Drama", "TV/Drama/Breaking Bad"},
		},
		{
			name:         "file directly in root produces no tags",
			filePath:     "/media/standalone.mkv",
			roots:        []string{"/media"},
			expectedTags: nil,
		},
		{
			name:         "no matching root produces no tags",
			filePath:     "/other/Movies/film.mkv",
			roots:        []string{"/media"},
			expectedTags: nil,
		},
		{
			name:         "multiple roots picks the correct one",
			filePath:     "/nas/TV/Sitcoms/Seinfeld/ep.mkv",
			roots:        []string{"/media", "/nas"},
			expectedTags: []string{"TV", "TV/Sitcoms", "TV/Sitcoms/Seinfeld"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TagsFromFolderPath(tt.filePath, tt.roots)
			assert.Equal(t, tt.expectedTags, got)
		})
	}
}
