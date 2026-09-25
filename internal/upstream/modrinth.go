package upstream

import (
	"context"
	"fmt"
	"net/url"
)

type modrinthVersion struct {
	ID            string   `json:"id"`
	ProjectID     string   `json:"project_id"`
	VersionNumber string   `json:"version_number"`
	GameVersions  []string `json:"game_versions"`
	Loaders       []string `json:"loaders"`
	Files         []struct {
		Hashes struct {
			SHA1   string `json:"sha1"`
			SHA512 string `json:"sha512"`
		} `json:"hashes"`
		URL      string `json:"url"`
		Filename string `json:"filename"`
		Primary  bool   `json:"primary"`
		Size     int64  `json:"size"`
	} `json:"files"`
}

// ModrinthFile describes a mod jar from Modrinth.
type ModrinthFile struct {
	ProjectID     string
	VersionID     string
	VersionNumber string
	GameVersions  []string
	Loaders       []string
	File          Artifact
}

// Modrinth resolves a project version. version may be a version id or a
// version number.
func (r *Resolver) Modrinth(ctx context.Context, project, version string) (*ModrinthFile, error) {
	var v modrinthVersion
	u := fmt.Sprintf("%s/project/%s/version/%s", r.ModrinthAPIURL, url.PathEscape(project), url.PathEscape(version))
	if err := r.getJSON(ctx, u, &v); err != nil {
		return nil, err
	}
	if len(v.Files) == 0 {
		return nil, fmt.Errorf("modrinth %s %s has no files", project, version)
	}
	idx := 0
	for i, f := range v.Files {
		if f.Primary {
			idx = i
			break
		}
	}
	f := v.Files[idx]
	return &ModrinthFile{
		ProjectID:     v.ProjectID,
		VersionID:     v.ID,
		VersionNumber: v.VersionNumber,
		GameVersions:  v.GameVersions,
		Loaders:       v.Loaders,
		File: Artifact{
			URL:      f.URL,
			Digest:   Digest{Algorithm: "sha512", Hex: f.Hashes.SHA512},
			Filename: f.Filename,
		},
	}, nil
}

// Supports reports whether the version lists the game version and loader.
func (m *ModrinthFile) Supports(gameVersion, loader string) bool {
	game, ld := false, false
	for _, g := range m.GameVersions {
		if g == gameVersion {
			game = true
			break
		}
	}
	for _, l := range m.Loaders {
		if l == loader {
			ld = true
			break
		}
	}
	return game && ld
}
