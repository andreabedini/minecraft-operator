// Package plan turns a MinecraftInstance spec into what the supervisor has
// to do: files to download, installers to run, and the launch spec. It is
// the operator's Minecraft knowledge in one place, with no Kubernetes or
// network side effects beyond the upstream resolver.
package plan

import (
	"context"
	"fmt"
	"net/url"
	"path"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/andreabedini/minecraft-operator/api/v1alpha1"
	supervisorv1 "github.com/andreabedini/minecraft-operator/gen/supervisor/v1"
	"github.com/andreabedini/minecraft-operator/internal/upstream"
)

// Well-known values the operator and supervisor agree on.
const (
	// ManagementPort is where the server binds the management protocol on
	// loopback. The operator reaches it through the supervisor tunnel.
	ManagementPort = 25585
	// ManagementTunnelTarget is the tunnel target name.
	ManagementTunnelTarget = "management"
	// DefaultJavaImageTemplate builds the JRE image from the Java major.
	DefaultJavaImageTemplate = "eclipse-temurin:%d-jre"
	// ServerJar is where the server jar lives in the instance directory.
	ServerJar = "server.jar"
	// ForgeInstallerJar is where the Forge installer is downloaded to.
	ForgeInstallerJar = "forge-installer.jar"

	defaultMinMemoryMiB = 1024
	defaultMaxMemoryMiB = 2048
	stopTimeout         = 120 * time.Second
	quiesceTimeout      = 120 * time.Second
)

// Options tune resolution.
type Options struct {
	// JavaImageTemplate receives the Java major with %d.
	JavaImageTemplate string
	// ManagementSecret is written into server.properties.
	ManagementSecret string
	// GamePort is the server-port property.
	GamePort int32
	// ManagementPort overrides the loopback port of the management protocol
	// (tests). Zero means ManagementPort.
	ManagementPort int
}

// Download is a file the supervisor must fetch.
type Download struct {
	Path   string
	URL    string
	Digest upstream.Digest
}

// Run is a one-shot command the supervisor must execute after downloads.
type Run struct {
	Command string
	Args    []string
}

// Plan is the result of resolving a spec.
type Plan struct {
	Flavour   string
	JavaMajor int
	JavaImage string
	Resolved  v1alpha1.ResolvedStatus
	Downloads []Download
	Runs      []Run
	// ModsDir is "mods" or "plugins" depending on the flavour.
	ModsDir string
	// ModFiles are the jar file names expected in ModsDir; anything else
	// there was not installed by the operator.
	ModFiles []string
	// ReservedProperties are the server.properties keys the operator owns.
	ReservedProperties map[string]string
	Launch             *supervisorv1.LaunchSpec
}

// IncompatibleModError reports a mod that does not list the game version or
// loader.
type IncompatibleModError struct {
	Mod          string
	GameVersion  string
	Loader       string
	GameVersions []string
	Loaders      []string
}

func (e *IncompatibleModError) Error() string {
	return fmt.Sprintf("mod %s does not support %s/%s (supports versions %v, loaders %v)", e.Mod, e.GameVersion, e.Loader, e.GameVersions, e.Loaders)
}

// Resolve builds the plan. It talks to the publishers through r.
func Resolve(ctx context.Context, r *upstream.Resolver, spec *v1alpha1.MinecraftInstanceSpec, opts Options) (*Plan, error) {
	if opts.JavaImageTemplate == "" {
		opts.JavaImageTemplate = DefaultJavaImageTemplate
	}
	if opts.GamePort == 0 {
		opts.GamePort = 25565
	}
	if opts.ManagementPort == 0 {
		opts.ManagementPort = ManagementPort
	}
	vanilla, err := r.Vanilla(ctx, spec.Version)
	if err != nil {
		return nil, err
	}
	p := &Plan{
		Flavour:   spec.Flavour.FlavourName(),
		JavaMajor: vanilla.JavaMajor,
		ModsDir:   "mods",
		Resolved: v1alpha1.ResolvedStatus{
			Version:   spec.Version,
			JavaMajor: int32(vanilla.JavaMajor),
		},
	}
	if spec.Java.Image != "" {
		p.JavaImage = spec.Java.Image
	} else {
		p.JavaImage = fmt.Sprintf(opts.JavaImageTemplate, vanilla.JavaMajor)
	}
	p.Resolved.JavaImage = p.JavaImage

	var target []string
	switch {
	case spec.Flavour.Fabric != nil:
		f, err := r.Fabric(ctx, spec.Version, spec.Flavour.Fabric.LoaderVersion, spec.Flavour.Fabric.InstallerVersion)
		if err != nil {
			return nil, err
		}
		p.Resolved.LoaderVersion = f.LoaderVersion
		p.Resolved.InstallerVersion = f.InstallerVersion
		p.Downloads = append(p.Downloads, Download{Path: ServerJar, URL: f.Launcher.URL})
		p.Resolved.ServerJar = &v1alpha1.ResolvedFile{Path: ServerJar}
		target = []string{"-jar", ServerJar}
	case spec.Flavour.Paper != nil:
		var build int64
		if spec.Flavour.Paper.Build != nil {
			build = *spec.Flavour.Paper.Build
		}
		pp, err := r.Paper(ctx, spec.Version, build, spec.Flavour.Paper.Channel)
		if err != nil {
			return nil, err
		}
		p.Resolved.PaperBuild = &pp.Build
		p.Downloads = append(p.Downloads, Download{Path: ServerJar, URL: pp.Server.URL, Digest: pp.Server.Digest})
		p.Resolved.ServerJar = &v1alpha1.ResolvedFile{Path: ServerJar, Digest: digestString(pp.Server.Digest)}
		p.ModsDir = "plugins"
		target = []string{"-jar", ServerJar}
	case spec.Flavour.Forge != nil:
		f, err := r.Forge(ctx, spec.Version, spec.Flavour.Forge.Build)
		if err != nil {
			return nil, err
		}
		p.Resolved.ForgeBuild = f.Build
		p.Downloads = append(p.Downloads, Download{Path: ForgeInstallerJar, URL: f.Installer.URL})
		p.Runs = append(p.Runs, Run{Command: "java", Args: []string{"-jar", ForgeInstallerJar, "--installServer", "."}})
		argsFile := upstream.ForgeArgsFile(f.Build)
		p.Resolved.ServerJar = &v1alpha1.ResolvedFile{Path: argsFile}
		target = []string{"@" + argsFile}
	default:
		p.Downloads = append(p.Downloads, Download{Path: ServerJar, URL: vanilla.Server.URL, Digest: vanilla.Server.Digest})
		p.Resolved.ServerJar = &v1alpha1.ResolvedFile{Path: ServerJar, Digest: digestString(vanilla.Server.Digest)}
		target = []string{"-jar", ServerJar}
	}

	if len(spec.Mods) > 0 && p.Flavour == "vanilla" {
		return nil, fmt.Errorf("vanilla does not load mods; use fabric, paper or forge")
	}
	for _, m := range spec.Mods {
		var (
			file   string
			artURL string
			digest upstream.Digest
		)
		switch {
		case m.Modrinth != nil:
			mf, err := r.Modrinth(ctx, m.Modrinth.Project, m.Modrinth.Version)
			if err != nil {
				return nil, fmt.Errorf("mod %s: %w", m.Name, err)
			}
			if !mf.Supports(spec.Version, p.Flavour) && !spec.Upgrade.Force {
				return nil, &IncompatibleModError{Mod: m.Name, GameVersion: spec.Version, Loader: p.Flavour, GameVersions: mf.GameVersions, Loaders: mf.Loaders}
			}
			file, artURL, digest = mf.File.Filename, mf.File.URL, mf.File.Digest
		case m.URL != "":
			if m.Digest == nil {
				return nil, fmt.Errorf("mod %s: digest is required with url", m.Name)
			}
			u, err := url.Parse(m.URL)
			if err != nil {
				return nil, fmt.Errorf("mod %s: %w", m.Name, err)
			}
			file = path.Base(u.Path)
			if file == "" || file == "." || file == "/" {
				return nil, fmt.Errorf("mod %s: cannot derive a file name from %s", m.Name, m.URL)
			}
			artURL = m.URL
			digest = upstream.Digest{Algorithm: m.Digest.Algorithm, Hex: m.Digest.Value}
		default:
			return nil, fmt.Errorf("mod %s: no source", m.Name)
		}
		rel := p.ModsDir + "/" + file
		p.Downloads = append(p.Downloads, Download{Path: rel, URL: artURL, Digest: digest})
		p.ModFiles = append(p.ModFiles, file)
		p.Resolved.Mods = append(p.Resolved.Mods, v1alpha1.ResolvedMod{Name: m.Name, File: rel, Digest: digestString(digest)})
	}

	p.ReservedProperties = map[string]string{
		"server-port":                   fmt.Sprint(opts.GamePort),
		"enable-rcon":                   "false",
		"management-server-enabled":     "true",
		"management-server-host":        "127.0.0.1",
		"management-server-port":        fmt.Sprint(opts.ManagementPort),
		"management-server-tls-enabled": "false",
		"management-server-secret":      opts.ManagementSecret,
		"status-heartbeat-interval":     "15",
	}
	p.Launch = buildLaunch(spec, target, opts.ManagementPort)
	return p, nil
}

func buildLaunch(spec *v1alpha1.MinecraftInstanceSpec, target []string, managementPort int) *supervisorv1.LaunchSpec {
	minMiB, maxMiB := spec.JVM.MinMemoryMiB, spec.JVM.MaxMemoryMiB
	if minMiB <= 0 {
		minMiB = defaultMinMemoryMiB
	}
	if maxMiB <= 0 {
		maxMiB = defaultMaxMemoryMiB
	}
	if minMiB > maxMiB {
		minMiB = maxMiB
	}
	args := []string{fmt.Sprintf("-Xms%dM", minMiB), fmt.Sprintf("-Xmx%dM", maxMiB)}
	for _, a := range spec.JVM.ExtraArgs {
		if strings.TrimSpace(a) != "" {
			args = append(args, a)
		}
	}
	args = append(args, target...)
	args = append(args, "nogui")

	autostart := true
	if spec.Autostart != nil {
		autostart = *spec.Autostart
	}
	env := map[string]string{}
	for k, v := range spec.JVM.Env {
		env[k] = v
	}
	return &supervisorv1.LaunchSpec{
		Command:       "java",
		Args:          args,
		Env:           env,
		Autostart:     autostart,
		RestartPolicy: supervisorv1.RestartPolicy_RESTART_POLICY_ON_FAILURE,
		StopCommand:   "stop",
		StopTimeout:   durationpb.New(stopTimeout),
		Quiesce: &supervisorv1.ConsoleHook{
			Commands: []string{"save-off", "save-all flush"},
			WaitFor:  `Saved the game`,
			Timeout:  durationpb.New(quiesceTimeout),
		},
		Resume: &supervisorv1.ConsoleHook{Commands: []string{"save-on"}},
		TunnelTargets: map[string]*supervisorv1.TunnelTarget{
			ManagementTunnelTarget: {Address: fmt.Sprintf("127.0.0.1:%d", managementPort)},
		},
	}
}

func digestString(d upstream.Digest) string {
	if d.Hex == "" {
		return ""
	}
	return d.Algorithm + ":" + d.Hex
}

// DigestProto converts a digest for the supervisor API. Nil for none.
func DigestProto(d upstream.Digest) *supervisorv1.Digest {
	if d.Hex == "" {
		return nil
	}
	var algo supervisorv1.DigestAlgorithm
	switch strings.ToLower(d.Algorithm) {
	case "sha1":
		algo = supervisorv1.DigestAlgorithm_DIGEST_ALGORITHM_SHA1
	case "sha256":
		algo = supervisorv1.DigestAlgorithm_DIGEST_ALGORITHM_SHA256
	case "sha512":
		algo = supervisorv1.DigestAlgorithm_DIGEST_ALGORITHM_SHA512
	default:
		return nil
	}
	return &supervisorv1.Digest{Algorithm: algo, Hex: strings.ToLower(d.Hex)}
}
