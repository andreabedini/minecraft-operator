package upstream

import (
	"context"
	"fmt"
	"strings"
)

type forgePromotions struct {
	Promos map[string]string `json:"promos"`
}

// ForgeServer describes a Forge installer.
type ForgeServer struct {
	Version   string
	Build     string // e.g. "1.20.1-47.2.0"
	Installer Artifact
}

// Forge resolves the installer for a version. An empty build picks the
// recommended promotion, falling back to latest.
func (r *Resolver) Forge(ctx context.Context, version, build string) (*ForgeServer, error) {
	if build == "" {
		var promos forgePromotions
		if err := r.getJSON(ctx, r.ForgeFilesURL+"/promotions_slim.json", &promos); err != nil {
			return nil, err
		}
		forgeVersion, ok := promos.Promos[version+"-recommended"]
		if !ok {
			forgeVersion, ok = promos.Promos[version+"-latest"]
		}
		if !ok {
			return nil, &NotFoundError{What: "forge promotion for " + version}
		}
		build = version + "-" + forgeVersion
	} else if !strings.HasPrefix(build, version+"-") {
		return nil, fmt.Errorf("forge build %q does not belong to minecraft %s", build, version)
	}
	name := fmt.Sprintf("forge-%s-installer.jar", build)
	return &ForgeServer{
		Version: version,
		Build:   build,
		Installer: Artifact{
			URL:      fmt.Sprintf("%s/%s/%s", r.ForgeMavenURL, build, name),
			Filename: name,
		},
	}, nil
}

// ForgeArgsFile is the argument file the installer writes for 1.17+.
func ForgeArgsFile(build string) string {
	return "libraries/net/minecraftforge/forge/" + build + "/unix_args.txt"
}
