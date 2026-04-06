package scene

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGroupsFromFolderPath(t *testing.T) {
	tests := []struct {
		name           string
		filePath       string
		roots          []string
		expectedGroups []string
	}{
		{
			name:           "single level deep",
			filePath:       "/media/Movies/Inception.mkv",
			roots:          []string{"/media"},
			expectedGroups: []string{"Movies"},
		},
		{
			name:           "two levels deep",
			filePath:       "/media/Movies/Action/Die Hard.mkv",
			roots:          []string{"/media"},
			expectedGroups: []string{"Movies", "Action"},
		},
		{
			name:           "three levels deep",
			filePath:       "/media/TV/Drama/Breaking Bad/S01E01.mkv",
			roots:          []string{"/media"},
			expectedGroups: []string{"TV", "Drama", "Breaking Bad"},
		},
		{
			name:           "file directly in root produces no groups",
			filePath:       "/media/standalone.mkv",
			roots:          []string{"/media"},
			expectedGroups: nil,
		},
		{
			name:           "no matching root produces no groups",
			filePath:       "/other/Movies/film.mkv",
			roots:          []string{"/media"},
			expectedGroups: nil,
		},
		{
			name:           "multiple roots picks the correct one",
			filePath:       "/nas/TV/Sitcoms/Seinfeld/ep.mkv",
			roots:          []string{"/media", "/nas"},
			expectedGroups: []string{"TV", "Sitcoms", "Seinfeld"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GroupsFromFolderPath(tt.filePath, tt.roots)
			assert.Equal(t, tt.expectedGroups, got)
		})
	}
}
