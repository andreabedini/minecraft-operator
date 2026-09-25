package plan

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/andreabedini/minecraft-operator/api/v1alpha1"
	supervisorv1 "github.com/andreabedini/minecraft-operator/gen/supervisor/v1"
	"github.com/andreabedini/minecraft-operator/internal/upstream"
)

func fakeResolver(t *testing.T) *upstream.Resolver {
	t.Helper()
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/mojang/manifest.json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"versions":[{"id":"26.3","url":"` + srv.URL + `/mojang/26.3.json"},{"id":"1.20.1","url":"` + srv.URL + `/mojang/1.20.1.json"}]}`))
	})
	mux.HandleFunc("/mojang/26.3.json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"javaVersion":{"majorVersion":25},"downloads":{"server":{"url":"https://piston.example/26.3/server.jar","sha1":"abc"}}}`))
	})
	mux.HandleFunc("/mojang/1.20.1.json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"javaVersion":{"majorVersion":17},"downloads":{"server":{"url":"https://piston.example/1.20.1/server.jar","sha1":"old"}}}`))
	})
	mux.HandleFunc("/fabric/versions/loader/26.3", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"loader":{"version":"0.19.5","stable":true},"intermediary":{"version":"26.3","stable":true}}]`))
	})
	mux.HandleFunc("/fabric/versions/installer", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"version":"1.1.2","stable":true}]`))
	})
	mux.HandleFunc("/paper/projects/paper/versions/26.3/builds", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"id":41,"channel":"STABLE","downloads":{"server:default":{"name":"paper-26.3-41.jar","checksums":{"sha256":"p256"},"url":"https://fill.example/paper-26.3-41.jar"}}}]`))
	})
	mux.HandleFunc("/forgefiles/promotions_slim.json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"promos":{"1.20.1-recommended":"47.2.0"}}`))
	})
	mux.HandleFunc("/modrinth/project/fabric-api/version/0.161.0+26.3", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"v1","project_id":"P7dR8mSH","version_number":"0.161.0+26.3","game_versions":["26.3"],"loaders":["fabric"],
		 "files":[{"hashes":{"sha512":"f512"},"url":"https://cdn.example/fabric-api-0.161.0+26.3.jar","filename":"fabric-api-0.161.0+26.3.jar","primary":true}]}`))
	})
	mux.HandleFunc("/modrinth/project/oldmod/version/1.0", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"v2","project_id":"old","version_number":"1.0","game_versions":["26.2"],"loaders":["fabric"],
		 "files":[{"hashes":{"sha512":"o512"},"url":"https://cdn.example/oldmod-1.0.jar","filename":"oldmod-1.0.jar","primary":true}]}`))
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &upstream.Resolver{
		Client:            srv.Client(),
		MojangManifestURL: srv.URL + "/mojang/manifest.json",
		FabricMetaURL:     srv.URL + "/fabric",
		PaperAPIURL:       srv.URL + "/paper",
		ForgeFilesURL:     srv.URL + "/forgefiles",
		ForgeMavenURL:     "https://maven.example/forge",
		ModrinthAPIURL:    srv.URL + "/modrinth",
	}
}

func TestResolveFabricWithMods(t *testing.T) {
	r := fakeResolver(t)
	spec := &v1alpha1.MinecraftInstanceSpec{
		Version: "26.3",
		Flavour: v1alpha1.FlavourSpec{Fabric: &v1alpha1.FabricFlavour{}},
		JVM:     v1alpha1.JVMSpec{MinMemoryMiB: 2048, MaxMemoryMiB: 6144, ExtraArgs: []string{"-XX:+UseZGC", ""}, Env: map[string]string{"JAVA_TOOL_OPTIONS": "-Djava.net.preferIPv6Addresses=true"}},
		Mods: []v1alpha1.ModSpec{
			{Name: "fabric-api", Modrinth: &v1alpha1.ModrinthSource{Project: "fabric-api", Version: "0.161.0+26.3"}},
			{Name: "geyser", URL: "https://download.example/geyser/Geyser-Fabric.jar", Digest: &v1alpha1.DigestSpec{Algorithm: "sha256", Value: "g256"}},
		},
	}
	p, err := Resolve(context.Background(), r, spec, Options{ManagementSecret: "s3cret"})
	if err != nil {
		t.Fatal(err)
	}
	if p.JavaMajor != 25 || p.JavaImage != "eclipse-temurin:25-jre" {
		t.Errorf("java = %d %s", p.JavaMajor, p.JavaImage)
	}
	if p.Resolved.LoaderVersion != "0.19.5" || p.Resolved.InstallerVersion != "1.1.2" {
		t.Errorf("resolved = %+v", p.Resolved)
	}
	var paths []string
	for _, d := range p.Downloads {
		paths = append(paths, d.Path)
	}
	if strings.Join(paths, ",") != "server.jar,mods/fabric-api-0.161.0+26.3.jar,mods/Geyser-Fabric.jar" {
		t.Errorf("downloads = %v", paths)
	}
	if p.Downloads[0].URL != r.FabricMetaURL+"/versions/loader/26.3/0.19.5/1.1.2/server/jar" || p.Downloads[0].Digest.Hex != "" {
		t.Errorf("launcher download = %+v", p.Downloads[0])
	}
	if p.Downloads[1].Digest.Algorithm != "sha512" || p.Downloads[2].Digest.Hex != "g256" {
		t.Errorf("mod digests = %+v %+v", p.Downloads[1].Digest, p.Downloads[2].Digest)
	}
	if strings.Join(p.ModFiles, ",") != "fabric-api-0.161.0+26.3.jar,Geyser-Fabric.jar" {
		t.Errorf("mod files = %v", p.ModFiles)
	}
	if len(p.Runs) != 0 {
		t.Errorf("runs = %v", p.Runs)
	}
	wantArgs := "-Xms2048M -Xmx6144M -XX:+UseZGC -jar server.jar nogui"
	if got := strings.Join(p.Launch.GetArgs(), " "); got != wantArgs {
		t.Errorf("args = %q, want %q", got, wantArgs)
	}
	if p.Launch.GetCommand() != "java" || !p.Launch.GetAutostart() || p.Launch.GetStopCommand() != "stop" {
		t.Errorf("launch = %v", p.Launch)
	}
	if p.Launch.GetEnv()["JAVA_TOOL_OPTIONS"] == "" {
		t.Error("env not carried")
	}
	if p.Launch.GetTunnelTargets()[ManagementTunnelTarget].GetAddress() != "127.0.0.1:25585" {
		t.Errorf("tunnel targets = %v", p.Launch.GetTunnelTargets())
	}
	if p.ReservedProperties["management-server-secret"] != "s3cret" || p.ReservedProperties["enable-rcon"] != "false" || p.ReservedProperties["server-port"] != "25565" {
		t.Errorf("reserved = %v", p.ReservedProperties)
	}
}

func TestResolveRejectsIncompatibleModUnlessForced(t *testing.T) {
	r := fakeResolver(t)
	spec := &v1alpha1.MinecraftInstanceSpec{
		Version: "26.3",
		Flavour: v1alpha1.FlavourSpec{Fabric: &v1alpha1.FabricFlavour{}},
		Mods:    []v1alpha1.ModSpec{{Name: "oldmod", Modrinth: &v1alpha1.ModrinthSource{Project: "oldmod", Version: "1.0"}}},
	}
	_, err := Resolve(context.Background(), r, spec, Options{})
	var inc *IncompatibleModError
	if !errors.As(err, &inc) || inc.Mod != "oldmod" {
		t.Fatalf("expected IncompatibleModError, got %v", err)
	}
	spec.Upgrade.Force = true
	if _, err := Resolve(context.Background(), r, spec, Options{}); err != nil {
		t.Errorf("forced: %v", err)
	}
}

func TestResolveVanillaPaperForge(t *testing.T) {
	r := fakeResolver(t)
	ctx := context.Background()

	p, err := Resolve(ctx, r, &v1alpha1.MinecraftInstanceSpec{Version: "26.3", Flavour: v1alpha1.FlavourSpec{Vanilla: &v1alpha1.VanillaFlavour{}}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Downloads[0].URL != "https://piston.example/26.3/server.jar" || p.Downloads[0].Digest.Algorithm != "sha1" || p.Resolved.ServerJar.Digest != "sha1:abc" {
		t.Errorf("vanilla = %+v", p.Downloads[0])
	}
	if strings.Join(p.Launch.GetArgs(), " ") != "-Xms1024M -Xmx2048M -jar server.jar nogui" {
		t.Errorf("vanilla args = %v", p.Launch.GetArgs())
	}
	_, err = Resolve(ctx, r, &v1alpha1.MinecraftInstanceSpec{Version: "26.3", Flavour: v1alpha1.FlavourSpec{Vanilla: &v1alpha1.VanillaFlavour{}},
		Mods: []v1alpha1.ModSpec{{Name: "x", URL: "https://x/x.jar", Digest: &v1alpha1.DigestSpec{Algorithm: "sha1", Value: "1"}}}}, Options{})
	if err == nil {
		t.Error("vanilla with mods accepted")
	}

	p, err = Resolve(ctx, r, &v1alpha1.MinecraftInstanceSpec{Version: "26.3", Flavour: v1alpha1.FlavourSpec{Paper: &v1alpha1.PaperFlavour{}}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if p.ModsDir != "plugins" || *p.Resolved.PaperBuild != 41 || p.Downloads[0].Digest.Hex != "p256" {
		t.Errorf("paper = %+v", p)
	}

	p, err = Resolve(ctx, r, &v1alpha1.MinecraftInstanceSpec{Version: "1.20.1", Flavour: v1alpha1.FlavourSpec{Forge: &v1alpha1.ForgeFlavour{}}, Java: v1alpha1.JavaSpec{Image: "my/jre:17"}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if p.JavaImage != "my/jre:17" || p.Resolved.ForgeBuild != "1.20.1-47.2.0" {
		t.Errorf("forge = %+v", p.Resolved)
	}
	if p.Downloads[0].Path != ForgeInstallerJar || len(p.Runs) != 1 || strings.Join(p.Runs[0].Args, " ") != "-jar forge-installer.jar --installServer ." {
		t.Errorf("forge steps = %+v %+v", p.Downloads, p.Runs)
	}
	if got := strings.Join(p.Launch.GetArgs(), " "); got != "-Xms1024M -Xmx2048M @libraries/net/minecraftforge/forge/1.20.1-47.2.0/unix_args.txt nogui" {
		t.Errorf("forge args = %q", got)
	}
}

func TestDigestProto(t *testing.T) {
	if DigestProto(upstream.Digest{}) != nil {
		t.Error("empty digest should be nil")
	}
	d := DigestProto(upstream.Digest{Algorithm: "SHA256", Hex: "ABC"})
	if d.GetAlgorithm() != supervisorv1.DigestAlgorithm_DIGEST_ALGORITHM_SHA256 || d.GetHex() != "abc" {
		t.Errorf("digest = %v", d)
	}
	if DigestProto(upstream.Digest{Algorithm: "md5", Hex: "x"}) != nil {
		t.Error("md5 should be nil")
	}
}

func TestMergeProperties(t *testing.T) {
	existing := "#Minecraft server properties\n#Thu Sep 25 2026\nmotd=Hello\nserver-port=25565\nenable-rcon=true\nview-distance=10\n"
	got := MergeProperties(existing, map[string]string{
		"server-port":               "25566",
		"enable-rcon":               "false",
		"management-server-enabled": "true",
		"motd":                      "Hello",
	})
	want := "#Minecraft server properties\n#Thu Sep 25 2026\nmotd=Hello\nserver-port=25566\nenable-rcon=false\nview-distance=10\nmanagement-server-enabled=true\n"
	if got != want {
		t.Errorf("merged =\n%s\nwant\n%s", got, want)
	}
	small := MergeProperties("", map[string]string{"b": "2", "a": "1"})
	if small != "a=1\nb=2\n" {
		t.Errorf("empty existing = %q", small)
	}
	props := ParseProperties(small)
	if props["a"] != "1" || props["b"] != "2" || len(props) != 2 {
		t.Errorf("parsed = %v", props)
	}
}
