package upstream

import (
	"context"
	"fmt"
	"strings"
)

// Fill v3 (https://fill.papermc.io/v3) replaced the v2 downloads API, which
// was sunset in 2026.

type paperBuild struct {
	ID        int64  `json:"id"`
	Channel   string `json:"channel"` // ALPHA, BETA, STABLE
	Downloads map[string]struct {
		Name      string `json:"name"`
		Checksums struct {
			SHA256 string `json:"sha256"`
		} `json:"checksums"`
		Size int64  `json:"size"`
		URL  string `json:"url"`
	} `json:"downloads"`
}

// PaperServer describes a Paper build.
type PaperServer struct {
	Version string
	Build   int64
	Channel string
	Server  Artifact
}

var paperChannelRank = map[string]int{"STABLE": 3, "BETA": 2, "ALPHA": 1}

// Paper resolves a build for a version. A zero build picks the newest build
// whose channel is at least minChannel (stable, beta or alpha; empty means
// stable). A pinned build is used regardless of channel.
func (r *Resolver) Paper(ctx context.Context, version string, build int64, minChannel string) (*PaperServer, error) {
	var builds []paperBuild
	if err := r.getJSON(ctx, fmt.Sprintf("%s/projects/paper/versions/%s/builds", r.PaperAPIURL, version), &builds); err != nil {
		return nil, err
	}
	if minChannel == "" {
		minChannel = "stable"
	}
	minRank, ok := paperChannelRank[strings.ToUpper(minChannel)]
	if !ok {
		return nil, fmt.Errorf("unknown paper channel %q", minChannel)
	}
	idx := -1
	for i, b := range builds {
		if build != 0 {
			if b.ID == build {
				idx = i
				break
			}
			continue
		}
		if paperChannelRank[strings.ToUpper(b.Channel)] >= minRank && (idx < 0 || b.ID > builds[idx].ID) {
			idx = i
		}
	}
	if idx < 0 {
		if build != 0 {
			return nil, &NotFoundError{What: fmt.Sprintf("paper %s build %d", version, build)}
		}
		return nil, &NotFoundError{What: fmt.Sprintf("paper %s build in channel %s or better", version, minChannel)}
	}
	b := builds[idx]
	dl, ok := b.Downloads["server:default"]
	if !ok {
		return nil, fmt.Errorf("paper %s build %d has no server:default download", version, b.ID)
	}
	return &PaperServer{
		Version: version,
		Build:   b.ID,
		Channel: strings.ToLower(b.Channel),
		Server: Artifact{
			URL:      dl.URL,
			Digest:   Digest{Algorithm: "sha256", Hex: dl.Checksums.SHA256},
			Filename: dl.Name,
		},
	}, nil
}
