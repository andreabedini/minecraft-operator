// Package upstream resolves Minecraft server artifacts from their publishers:
// Mojang, FabricMC, PaperMC, MinecraftForge and Modrinth. It turns a version
// and flavour into download URLs and expected digests. It never downloads
// the artifacts themselves; the supervisor does that.
package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Default endpoints. Tests override them.
const (
	DefaultMojangManifestURL = "https://piston-meta.mojang.com/mc/game/version_manifest_v2.json"
	DefaultFabricMetaURL     = "https://meta.fabricmc.net/v2"
	DefaultPaperAPIURL       = "https://fill.papermc.io/v3"
	DefaultForgeFilesURL     = "https://files.minecraftforge.net/net/minecraftforge/forge"
	DefaultForgeMavenURL     = "https://maven.minecraftforge.net/net/minecraftforge/forge"
	DefaultModrinthAPIURL    = "https://api.modrinth.com/v2"
)

// Digest is an expected artifact digest.
type Digest struct {
	// Algorithm is sha1, sha256 or sha512.
	Algorithm string
	Hex       string
}

// Artifact is something to download.
type Artifact struct {
	URL string
	// Digest is empty when the publisher gives none.
	Digest Digest
	// Filename is the publisher's file name, when known.
	Filename string
}

// Resolver queries the publishers.
type Resolver struct {
	Client            *http.Client
	UserAgent         string
	MojangManifestURL string
	FabricMetaURL     string
	PaperAPIURL       string
	ForgeFilesURL     string
	ForgeMavenURL     string
	ModrinthAPIURL    string
}

// NewResolver returns a resolver with the default endpoints.
func NewResolver(userAgent string) *Resolver {
	return &Resolver{
		Client:            &http.Client{Timeout: 30 * time.Second},
		UserAgent:         userAgent,
		MojangManifestURL: DefaultMojangManifestURL,
		FabricMetaURL:     DefaultFabricMetaURL,
		PaperAPIURL:       DefaultPaperAPIURL,
		ForgeFilesURL:     DefaultForgeFilesURL,
		ForgeMavenURL:     DefaultForgeMavenURL,
		ModrinthAPIURL:    DefaultModrinthAPIURL,
	}
}

// NotFoundError reports a version, build or project the publisher does not
// have.
type NotFoundError struct {
	What string
}

func (e *NotFoundError) Error() string { return e.What + " not found" }

func (r *Resolver) getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if r.UserAgent != "" {
		req.Header.Set("User-Agent", r.UserAgent)
	}
	resp, err := r.Client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return &NotFoundError{What: url}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("GET %s: %s: %s", url, resp.Status, string(body))
	}
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("GET %s: decode: %w", url, err)
	}
	return nil
}
