package upstream

import (
	"context"
	"fmt"
)

// MojangVersion is what the Mojang manifest knows about one version.
type MojangVersion struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	URL         string `json:"url"`
	SHA1        string `json:"sha1"`
	ReleaseTime string `json:"releaseTime"`
}

type mojangManifest struct {
	Latest struct {
		Release  string `json:"release"`
		Snapshot string `json:"snapshot"`
	} `json:"latest"`
	Versions []MojangVersion `json:"versions"`
}

type mojangVersionInfo struct {
	ID          string `json:"id"`
	JavaVersion struct {
		Component    string `json:"component"`
		MajorVersion int    `json:"majorVersion"`
	} `json:"javaVersion"`
	Downloads struct {
		Server *struct {
			URL  string `json:"url"`
			SHA1 string `json:"sha1"`
			Size int64  `json:"size"`
		} `json:"server"`
	} `json:"downloads"`
}

// VanillaServer describes the Mojang server jar for a version.
type VanillaServer struct {
	Version   string
	JavaMajor int
	Server    Artifact
}

// MojangVersions lists every version in the manifest, newest first.
func (r *Resolver) MojangVersions(ctx context.Context) ([]MojangVersion, error) {
	var m mojangManifest
	if err := r.getJSON(ctx, r.MojangManifestURL, &m); err != nil {
		return nil, err
	}
	return m.Versions, nil
}

// Vanilla resolves the server jar and Java major for a version.
func (r *Resolver) Vanilla(ctx context.Context, version string) (*VanillaServer, error) {
	versions, err := r.MojangVersions(ctx)
	if err != nil {
		return nil, err
	}
	var entry *MojangVersion
	for i := range versions {
		if versions[i].ID == version {
			entry = &versions[i]
			break
		}
	}
	if entry == nil {
		return nil, &NotFoundError{What: "minecraft version " + version}
	}
	var info mojangVersionInfo
	if err := r.getJSON(ctx, entry.URL, &info); err != nil {
		return nil, err
	}
	if info.Downloads.Server == nil || info.Downloads.Server.URL == "" {
		return nil, fmt.Errorf("minecraft version %s has no server download", version)
	}
	return &VanillaServer{
		Version:   version,
		JavaMajor: JavaMajorFor(info.JavaVersion.MajorVersion),
		Server: Artifact{
			URL:      info.Downloads.Server.URL,
			Digest:   Digest{Algorithm: "sha1", Hex: info.Downloads.Server.SHA1},
			Filename: "server.jar",
		},
	}, nil
}

// JavaMajorFor maps the manifest's Java major to one Temurin ships. Old
// versions without the field (0) need Java 8; 16 has no Temurin build, so
// 17 is used.
func JavaMajorFor(manifestMajor int) int {
	switch {
	case manifestMajor <= 0:
		return 8
	case manifestMajor == 16:
		return 17
	default:
		return manifestMajor
	}
}
