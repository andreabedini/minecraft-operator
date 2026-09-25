package upstream

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

type fabricLoaderEntry struct {
	Loader struct {
		Version string `json:"version"`
		Stable  bool   `json:"stable"`
	} `json:"loader"`
	Intermediary struct {
		Version string `json:"version"`
		Stable  bool   `json:"stable"`
	} `json:"intermediary"`
}

type fabricInstallerEntry struct {
	Version string `json:"version"`
	Stable  bool   `json:"stable"`
	URL     string `json:"url"`
}

// FabricServer describes a Fabric launcher jar.
type FabricServer struct {
	Version          string
	LoaderVersion    string
	InstallerVersion string
	Launcher         Artifact
}

// Fabric resolves the launcher jar for a game version. Empty loader and
// installer versions pick the newest stable ones.
func (r *Resolver) Fabric(ctx context.Context, version, loader, installer string) (*FabricServer, error) {
	if loader == "" {
		var entries []fabricLoaderEntry
		if err := r.getJSON(ctx, r.FabricMetaURL+"/versions/loader/"+version, &entries); err != nil {
			return nil, err
		}
		if len(entries) == 0 {
			return nil, &NotFoundError{What: "fabric loader for " + version}
		}
		for _, e := range entries {
			if e.Loader.Stable && e.Intermediary.Stable {
				if loader == "" || compareVersions(e.Loader.Version, loader) > 0 {
					loader = e.Loader.Version
				}
			}
		}
		if loader == "" {
			loader = entries[0].Loader.Version
		}
	}
	if installer == "" {
		var entries []fabricInstallerEntry
		if err := r.getJSON(ctx, r.FabricMetaURL+"/versions/installer", &entries); err != nil {
			return nil, err
		}
		for _, e := range entries {
			if e.Stable {
				installer = e.Version
				break
			}
		}
		if installer == "" && len(entries) > 0 {
			installer = entries[0].Version
		}
		if installer == "" {
			return nil, &NotFoundError{What: "fabric installer"}
		}
	}
	return &FabricServer{
		Version:          version,
		LoaderVersion:    loader,
		InstallerVersion: installer,
		Launcher: Artifact{
			URL:      fmt.Sprintf("%s/versions/loader/%s/%s/%s/server/jar", r.FabricMetaURL, version, loader, installer),
			Filename: "server.jar",
		},
	}, nil
}

// compareVersions compares dotted versions part by part. Each part is a
// numeric prefix plus an optional suffix; a part with a suffix (a
// pre-release such as "0-beta") ranks below the same number without one.
func compareVersions(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y string
		if i < len(as) {
			x = as[i]
		}
		if i < len(bs) {
			y = bs[i]
		}
		xn, xs := splitNumeric(x)
		yn, ys := splitNumeric(y)
		if xn != yn {
			if xn < yn {
				return -1
			}
			return 1
		}
		if xs != ys {
			switch {
			case xs == "":
				return 1
			case ys == "":
				return -1
			default:
				return strings.Compare(xs, ys)
			}
		}
	}
	return 0
}

// splitNumeric returns the leading integer of s and the rest. A missing or
// non-numeric prefix counts as -1 so "" sorts before "0".
func splitNumeric(s string) (int, string) {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 {
		return -1, s
	}
	n, err := strconv.Atoi(s[:i])
	if err != nil {
		return -1, s
	}
	return n, s[i:]
}
