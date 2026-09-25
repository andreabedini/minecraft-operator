package controller

import (
	"context"
	"fmt"
	"path"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/andreabedini/minecraft-operator/api/v1alpha1"
	supervisorv1 "github.com/andreabedini/minecraft-operator/gen/supervisor/v1"
	"github.com/andreabedini/minecraft-operator/internal/plan"
	"github.com/andreabedini/minecraft-operator/internal/supervisorclient"
)

const (
	eulaFile       = "eula.txt"
	eulaContent    = "# Accepted by creating the MinecraftInstance; see https://aka.ms/MinecraftEULA\neula=true\n"
	propertiesFile = "server.properties"
	installTimeout = 30 * time.Minute
)

// installState is what the install pass found and did.
type installState struct {
	// Installed is true when every artifact is in place.
	Installed bool
	// NeedsRestart is true when changes are staged behind a running process.
	NeedsRestart bool
	// Pending describes why Installed is false while the process runs.
	Pending []string
	// Unmanaged lists jars in the mods directory the operator did not install.
	Unmanaged []string
	// LaunchHash is the launch spec hash now in effect on the supervisor.
	LaunchHash string
	// Downloaded lists files fetched during this pass.
	Downloaded []string
}

// installer drives one supervisor toward a plan.
type installer struct {
	client   client.Client
	sup      *supervisorclient.Client
	inst     *v1alpha1.MinecraftInstance
	plan     *plan.Plan
	running  bool
	observed *v1alpha1.ResolvedStatus // digests observed on earlier passes
}

// run performs one idempotent pass. Files are compared by digest; only
// missing or mismatching ones are fetched. While the process runs, nothing
// is downloaded and config goes to the staged tree.
func (in *installer) run(ctx context.Context) (*installState, error) {
	logger := log.FromContext(ctx)
	state := &installState{}
	present, err := in.sup.Digests(ctx, ".", supervisorv1.DigestAlgorithm_DIGEST_ALGORITHM_SHA256)
	if err != nil {
		return nil, fmt.Errorf("list files: %w", err)
	}
	sha1s := map[string]string{}
	sha512s := map[string]string{}
	need := func(algo string) (map[string]string, error) {
		switch algo {
		case "sha256":
			return present, nil
		case "sha1":
			if len(sha1s) == 0 {
				m, err := in.sup.Digests(ctx, ".", supervisorv1.DigestAlgorithm_DIGEST_ALGORITHM_SHA1)
				if err != nil {
					return nil, err
				}
				sha1s = m
			}
			return sha1s, nil
		case "sha512":
			if len(sha512s) == 0 {
				m, err := in.sup.Digests(ctx, ".", supervisorv1.DigestAlgorithm_DIGEST_ALGORITHM_SHA512)
				if err != nil {
					return nil, err
				}
				sha512s = m
			}
			return sha512s, nil
		}
		return nil, fmt.Errorf("unsupported digest algorithm %q", algo)
	}

	// Downloads.
	for _, d := range in.plan.Downloads {
		want := strings.ToLower(d.Digest.Hex)
		algo := d.Digest.Algorithm
		if want == "" {
			// No publisher digest: use the sha256 observed when we first
			// fetched it, so re-resolution does not re-download.
			algo = "sha256"
			want = in.observedDigest(d.Path)
		}
		have := ""
		if _, ok := present[d.Path]; ok {
			m, err := need(algo)
			if err != nil {
				return nil, err
			}
			have = m[d.Path]
		}
		if have != "" && (want == "" || have == want) {
			if want == "" {
				// Adopted without a publisher digest: remember what is
				// there so later passes compare against it.
				in.recordObserved(d.Path, []*supervisorv1.Digest{{Algorithm: supervisorv1.DigestAlgorithm_DIGEST_ALGORITHM_SHA256, Hex: present[d.Path]}})
			}
			continue
		}
		if in.running {
			state.Pending = append(state.Pending, "download "+d.Path)
			continue
		}
		logger.Info("downloading", "path", d.Path, "url", d.URL)
		res, err := in.sup.Download(ctx, d.URL, d.Path, plan.DigestProto(d.Digest))
		if err != nil {
			return nil, fmt.Errorf("download %s: %w", d.Path, err)
		}
		state.Downloaded = append(state.Downloaded, d.Path)
		in.recordObserved(d.Path, res.GetDigests())
	}

	// Installers (Forge) run when their output is missing.
	for _, r := range in.plan.Runs {
		target := in.plan.Resolved.ServerJar
		if target != nil {
			if _, ok := present[target.Path]; ok {
				continue
			}
			if len(state.Downloaded) == 0 {
				// The output may exist under a path we did not list (fresh
				// listing needed after downloads); re-check cheaply.
				if _, err := in.sup.ReadFile(ctx, target.Path); err == nil {
					continue
				}
			}
		}
		if in.running {
			state.Pending = append(state.Pending, "run "+r.Command)
			continue
		}
		logger.Info("running installer", "command", r.Command, "args", r.Args)
		res, err := in.sup.Run(ctx, r.Command, r.Args, "", in.plan.Launch.GetEnv(), installTimeout)
		if err != nil {
			return nil, fmt.Errorf("run %s: %w", r.Command, err)
		}
		if res.ExitCode != 0 {
			tail := res.Output
			if len(tail) > 20 {
				tail = tail[len(tail)-20:]
			}
			return nil, fmt.Errorf("installer %s exited with %d: %s", r.Command, res.ExitCode, strings.Join(tail, " | "))
		}
	}

	// Stale mods: only jars the operator installed and that are no longer
	// in the spec. Anything else in the directory is reported, not deleted.
	wantMods := map[string]bool{}
	for _, f := range in.plan.ModFiles {
		wantMods[f] = true
	}
	managed := map[string]bool{}
	if in.observed != nil {
		for _, m := range in.observed.Mods {
			managed[path.Base(m.File)] = true
		}
	}
	for p := range present {
		dir, file := path.Split(p)
		if strings.TrimSuffix(dir, "/") != in.plan.ModsDir || !strings.HasSuffix(file, ".jar") {
			continue
		}
		if wantMods[file] {
			continue
		}
		if !managed[file] {
			state.Unmanaged = append(state.Unmanaged, file)
			continue
		}
		if err := in.sup.DeleteFile(ctx, p, in.running, false); err != nil {
			return nil, fmt.Errorf("delete stale mod %s: %w", p, err)
		}
		if in.running {
			state.NeedsRestart = true
		}
		logger.Info("removed stale mod", "path", p, "staged", in.running)
	}

	// EULA.
	if _, ok := present[eulaFile]; !ok {
		if err := in.write(ctx, eulaFile, []byte(eulaContent), state); err != nil {
			return nil, err
		}
	}

	// Config files, including server.properties with its reserved keys.
	if err := in.syncConfigFiles(ctx, state); err != nil {
		return nil, err
	}

	// Launch spec.
	hash, err := in.sup.SetLaunch(ctx, in.plan.Launch)
	if err != nil {
		return nil, fmt.Errorf("set launch: %w", err)
	}
	state.LaunchHash = hash
	if in.running && in.observed != nil && in.observed.LaunchSpecHash != "" && in.observed.LaunchSpecHash != hash {
		state.NeedsRestart = true
	}

	// Apply staged changes now if nothing runs.
	if !in.running {
		status, err := in.sup.Status(ctx)
		if err != nil {
			return nil, err
		}
		if status.GetStagedCount() > 0 {
			if _, err := in.sup.ApplyStaged(ctx); err != nil {
				return nil, fmt.Errorf("apply staged: %w", err)
			}
		}
	}
	state.Installed = len(state.Pending) == 0
	return state, nil
}

// write puts content in place, staged when the process runs. It records a
// pending restart when the staged content differs from the live file.
func (in *installer) write(ctx context.Context, rel string, content []byte, state *installState) error {
	if in.running {
		current, err := in.sup.ReadFile(ctx, rel)
		if err != nil && !supervisorclient.IsNotFound(err) {
			return fmt.Errorf("read %s: %w", rel, err)
		}
		if string(current) == string(content) {
			return nil
		}
		if _, err := in.sup.WriteFile(ctx, rel, content, true); err != nil {
			return fmt.Errorf("stage %s: %w", rel, err)
		}
		state.NeedsRestart = true
		return nil
	}
	if _, err := in.sup.WriteFile(ctx, rel, content, false); err != nil {
		return fmt.Errorf("write %s: %w", rel, err)
	}
	return nil
}

// syncConfigFiles renders every config file and the properties file.
func (in *installer) syncConfigFiles(ctx context.Context, state *installState) error {
	propertiesHandled := false
	for _, cf := range in.inst.Spec.ConfigFiles {
		content, err := in.loadConfigSource(ctx, cf)
		if err != nil {
			return err
		}
		rel := path.Clean(cf.Path)
		if cf.Merge == v1alpha1.MergeProperties || rel == propertiesFile {
			existing, err := in.sup.ReadFile(ctx, rel)
			if err != nil && !supervisorclient.IsNotFound(err) {
				return fmt.Errorf("read %s: %w", rel, err)
			}
			merged := plan.MergeProperties(string(existing), plan.ParseProperties(string(content)))
			if rel == propertiesFile {
				merged = plan.MergeProperties(merged, in.plan.ReservedProperties)
				propertiesHandled = true
			}
			if err := in.writeIfChanged(ctx, rel, string(existing), merged, state); err != nil {
				return err
			}
			continue
		}
		existing, err := in.sup.ReadFile(ctx, rel)
		if err != nil && !supervisorclient.IsNotFound(err) {
			return fmt.Errorf("read %s: %w", rel, err)
		}
		if err := in.writeIfChanged(ctx, rel, string(existing), string(content), state); err != nil {
			return err
		}
	}
	if !propertiesHandled {
		existing, err := in.sup.ReadFile(ctx, propertiesFile)
		if err != nil && !supervisorclient.IsNotFound(err) {
			return fmt.Errorf("read %s: %w", propertiesFile, err)
		}
		merged := plan.MergeProperties(string(existing), in.plan.ReservedProperties)
		if err := in.writeIfChanged(ctx, propertiesFile, string(existing), merged, state); err != nil {
			return err
		}
	}
	return nil
}

func (in *installer) writeIfChanged(ctx context.Context, rel, existing, desired string, state *installState) error {
	if existing == desired {
		return nil
	}
	return in.write(ctx, rel, []byte(desired), state)
}

func (in *installer) loadConfigSource(ctx context.Context, cf v1alpha1.ConfigFileSpec) ([]byte, error) {
	switch {
	case cf.ConfigMapKeyRef != nil:
		var cm corev1.ConfigMap
		if err := in.client.Get(ctx, types.NamespacedName{Namespace: in.inst.Namespace, Name: cf.ConfigMapKeyRef.Name}, &cm); err != nil {
			return nil, fmt.Errorf("config file %s: %w", cf.Path, err)
		}
		if v, ok := cm.Data[cf.ConfigMapKeyRef.Key]; ok {
			return []byte(v), nil
		}
		if v, ok := cm.BinaryData[cf.ConfigMapKeyRef.Key]; ok {
			return v, nil
		}
		return nil, fmt.Errorf("config file %s: key %q not in ConfigMap %s", cf.Path, cf.ConfigMapKeyRef.Key, cf.ConfigMapKeyRef.Name)
	case cf.SecretKeyRef != nil:
		var sec corev1.Secret
		if err := in.client.Get(ctx, types.NamespacedName{Namespace: in.inst.Namespace, Name: cf.SecretKeyRef.Name}, &sec); err != nil {
			return nil, fmt.Errorf("config file %s: %w", cf.Path, err)
		}
		v, ok := sec.Data[cf.SecretKeyRef.Key]
		if !ok {
			return nil, fmt.Errorf("config file %s: key %q not in Secret %s", cf.Path, cf.SecretKeyRef.Key, cf.SecretKeyRef.Name)
		}
		return v, nil
	}
	return nil, fmt.Errorf("config file %s: no source", cf.Path)
}

// observedDigest returns the sha256 recorded for a file on an earlier pass.
func (in *installer) observedDigest(rel string) string {
	if in.observed == nil {
		return ""
	}
	if in.observed.ServerJar != nil && in.observed.ServerJar.Path == rel {
		return strings.TrimPrefix(in.observed.ServerJar.Digest, "sha256:")
	}
	for _, m := range in.observed.Mods {
		if m.File == rel && strings.HasPrefix(m.Digest, "sha256:") {
			return strings.TrimPrefix(m.Digest, "sha256:")
		}
	}
	return ""
}

// recordObserved stores the sha256 of a freshly downloaded file into the
// plan's resolved status when the publisher gave no digest.
func (in *installer) recordObserved(rel string, digests []*supervisorv1.Digest) {
	var sha256 string
	for _, d := range digests {
		if d.GetAlgorithm() == supervisorv1.DigestAlgorithm_DIGEST_ALGORITHM_SHA256 {
			sha256 = d.GetHex()
		}
	}
	if sha256 == "" {
		return
	}
	if in.plan.Resolved.ServerJar != nil && in.plan.Resolved.ServerJar.Path == rel && in.plan.Resolved.ServerJar.Digest == "" {
		in.plan.Resolved.ServerJar.Digest = "sha256:" + sha256
	}
	for i := range in.plan.Resolved.Mods {
		if in.plan.Resolved.Mods[i].File == rel && in.plan.Resolved.Mods[i].Digest == "" {
			in.plan.Resolved.Mods[i].Digest = "sha256:" + sha256
		}
	}
}
