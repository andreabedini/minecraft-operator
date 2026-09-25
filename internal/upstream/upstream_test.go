package upstream

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeUpstreams serves canned responses for every publisher.
func fakeUpstreams(t *testing.T) *Resolver {
	t.Helper()
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/mojang/manifest.json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"latest":{"release":"26.3","snapshot":"26.4-snapshot-1"},"versions":[
		 {"id":"26.4-snapshot-1","type":"snapshot","url":"` + srv.URL + `/mojang/26.4s1.json","sha1":"x"},
		 {"id":"26.3","type":"release","url":"` + srv.URL + `/mojang/26.3.json","sha1":"y"},
		 {"id":"1.16.5","type":"release","url":"` + srv.URL + `/mojang/1.16.5.json","sha1":"z"},
		 {"id":"1.7.10","type":"release","url":"` + srv.URL + `/mojang/1.7.10.json","sha1":"w"}]}`))
	})
	mux.HandleFunc("/mojang/26.3.json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"26.3","javaVersion":{"component":"java-runtime-epsilon","majorVersion":25},
		 "downloads":{"server":{"url":"https://piston-data.example/26.3/server.jar","sha1":"abc123","size":10}}}`))
	})
	mux.HandleFunc("/mojang/1.16.5.json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"1.16.5","javaVersion":{"majorVersion":16},"downloads":{"server":{"url":"u","sha1":"s"}}}`))
	})
	mux.HandleFunc("/mojang/1.7.10.json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"1.7.10","downloads":{"server":{"url":"u","sha1":"s"}}}`))
	})
	mux.HandleFunc("/mojang/26.4s1.json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"26.4-snapshot-1","javaVersion":{"majorVersion":25},"downloads":{}}`))
	})
	mux.HandleFunc("/fabric/versions/loader/26.3", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[
		 {"loader":{"version":"0.19.6-beta.1","stable":false},"intermediary":{"version":"26.3","stable":true}},
		 {"loader":{"version":"0.19.5","stable":true},"intermediary":{"version":"26.3","stable":true}},
		 {"loader":{"version":"0.19.10","stable":true},"intermediary":{"version":"26.3","stable":true}},
		 {"loader":{"version":"0.19.4","stable":true},"intermediary":{"version":"26.3","stable":true}}]`))
	})
	mux.HandleFunc("/fabric/versions/loader/9.9", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	})
	mux.HandleFunc("/fabric/versions/installer", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"version":"1.2.0-rc1","stable":false},{"version":"1.1.0","stable":true},{"version":"1.0.1","stable":true}]`))
	})
	mux.HandleFunc("/paper/projects/paper/versions/26.3/builds", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[
		 {"id":13,"channel":"ALPHA","downloads":{"server:default":{"name":"paper-26.3-13.jar","checksums":{"sha256":"cc"},"url":"https://fill-data.example/cc/paper-26.3-13.jar"}}},
		 {"id":12,"channel":"STABLE","downloads":{"server:default":{"name":"paper-26.3-12.jar","checksums":{"sha256":"bb"},"url":"https://fill-data.example/bb/paper-26.3-12.jar"}}},
		 {"id":11,"channel":"BETA","downloads":{"server:default":{"name":"paper-26.3-11.jar","checksums":{"sha256":"b1"},"url":"https://fill-data.example/b1/paper-26.3-11.jar"}}},
		 {"id":10,"channel":"STABLE","downloads":{"server:default":{"name":"paper-26.3-10.jar","checksums":{"sha256":"aa"},"url":"https://fill-data.example/aa/paper-26.3-10.jar"}}}]`))
	})
	mux.HandleFunc("/paper/projects/paper/versions/26.4/builds", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"id":1,"channel":"ALPHA","downloads":{"server:default":{"name":"paper-26.4-1.jar","checksums":{"sha256":"a1"},"url":"https://fill-data.example/a1/paper-26.4-1.jar"}}}]`))
	})
	mux.HandleFunc("/forgefiles/promotions_slim.json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"promos":{"1.20.1-latest":"47.3.0","1.20.1-recommended":"47.2.0","1.21-latest":"51.0.1"}}`))
	})
	mux.HandleFunc("/modrinth/project/fabric-api/version/0.158.0+26.3", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"abc","project_id":"P7dR8mSH","version_number":"0.158.0+26.3","game_versions":["26.3"],"loaders":["fabric"],
		 "files":[{"hashes":{"sha1":"1","sha512":"deadbeef"},"url":"https://cdn.example/fabric-api-sources.jar","filename":"fabric-api-sources.jar","primary":false},
		          {"hashes":{"sha1":"2","sha512":"cafebabe"},"url":"https://cdn.example/fabric-api.jar","filename":"fabric-api-0.158.0+26.3.jar","primary":true}]}`))
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &Resolver{
		Client:            srv.Client(),
		MojangManifestURL: srv.URL + "/mojang/manifest.json",
		FabricMetaURL:     srv.URL + "/fabric",
		PaperAPIURL:       srv.URL + "/paper",
		ForgeFilesURL:     srv.URL + "/forgefiles",
		ForgeMavenURL:     "https://maven.example/forge",
		ModrinthAPIURL:    srv.URL + "/modrinth",
	}
}

func TestVanilla(t *testing.T) {
	r := fakeUpstreams(t)
	ctx := context.Background()
	v, err := r.Vanilla(ctx, "26.3")
	if err != nil {
		t.Fatal(err)
	}
	if v.JavaMajor != 25 || v.Server.URL != "https://piston-data.example/26.3/server.jar" || v.Server.Digest.Hex != "abc123" || v.Server.Digest.Algorithm != "sha1" {
		t.Errorf("vanilla 26.3 = %+v", v)
	}
	v, err = r.Vanilla(ctx, "1.16.5")
	if err != nil || v.JavaMajor != 17 {
		t.Errorf("1.16.5 java = %d, %v (want 17)", v.JavaMajor, err)
	}
	v, err = r.Vanilla(ctx, "1.7.10")
	if err != nil || v.JavaMajor != 8 {
		t.Errorf("1.7.10 java = %d, %v (want 8)", v.JavaMajor, err)
	}
	var nf *NotFoundError
	if _, err := r.Vanilla(ctx, "0.0.0"); !errors.As(err, &nf) {
		t.Errorf("unknown version: %v", err)
	}
	if _, err := r.Vanilla(ctx, "26.4-snapshot-1"); err == nil || !strings.Contains(err.Error(), "no server download") {
		t.Errorf("no server download: %v", err)
	}
}

func TestFabric(t *testing.T) {
	r := fakeUpstreams(t)
	ctx := context.Background()
	f, err := r.Fabric(ctx, "26.3", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if f.LoaderVersion != "0.19.10" {
		t.Errorf("loader = %s, want 0.19.10 (highest stable, numeric compare)", f.LoaderVersion)
	}
	if f.InstallerVersion != "1.1.0" {
		t.Errorf("installer = %s, want 1.1.0 (first stable)", f.InstallerVersion)
	}
	if want := r.FabricMetaURL + "/versions/loader/26.3/0.19.10/1.1.0/server/jar"; f.Launcher.URL != want {
		t.Errorf("url = %s, want %s", f.Launcher.URL, want)
	}
	f, err = r.Fabric(ctx, "26.3", "0.19.5", "1.0.1")
	if err != nil || f.LoaderVersion != "0.19.5" || f.InstallerVersion != "1.0.1" {
		t.Errorf("pinned = %+v, %v", f, err)
	}
	var nf *NotFoundError
	if _, err := r.Fabric(ctx, "9.9", "", ""); !errors.As(err, &nf) {
		t.Errorf("no loaders: %v", err)
	}
}

func TestPaper(t *testing.T) {
	r := fakeUpstreams(t)
	ctx := context.Background()
	p, err := r.Paper(ctx, "26.3", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Build != 12 || p.Channel != "stable" || p.Server.Digest.Hex != "bb" || p.Server.URL != "https://fill-data.example/bb/paper-26.3-12.jar" || p.Server.Filename != "paper-26.3-12.jar" {
		t.Errorf("paper = %+v", p)
	}
	p, err = r.Paper(ctx, "26.3", 0, "beta")
	if err != nil || p.Build != 12 {
		t.Errorf("beta-or-better = %+v, %v (stable 12 beats beta 11)", p, err)
	}
	p, err = r.Paper(ctx, "26.3", 0, "alpha")
	if err != nil || p.Build != 13 {
		t.Errorf("alpha-or-better = %+v, %v", p, err)
	}
	p, err = r.Paper(ctx, "26.3", 13, "")
	if err != nil || p.Build != 13 || p.Channel != "alpha" {
		t.Errorf("pinned alpha = %+v, %v", p, err)
	}
	var nf *NotFoundError
	if _, err := r.Paper(ctx, "26.4", 0, ""); !errors.As(err, &nf) {
		t.Errorf("no stable build: %v", err)
	}
	if _, err := r.Paper(ctx, "26.3", 99, ""); !errors.As(err, &nf) {
		t.Errorf("missing build: %v", err)
	}
	if _, err := r.Paper(ctx, "0.1", 0, ""); !errors.As(err, &nf) {
		t.Errorf("missing version: %v", err)
	}
	if _, err := r.Paper(ctx, "26.3", 0, "nightly"); err == nil {
		t.Error("unknown channel accepted")
	}
}

func TestForge(t *testing.T) {
	r := fakeUpstreams(t)
	ctx := context.Background()
	f, err := r.Forge(ctx, "1.20.1", "")
	if err != nil {
		t.Fatal(err)
	}
	if f.Build != "1.20.1-47.2.0" || f.Installer.URL != "https://maven.example/forge/1.20.1-47.2.0/forge-1.20.1-47.2.0-installer.jar" {
		t.Errorf("forge = %+v", f)
	}
	f, err = r.Forge(ctx, "1.21", "")
	if err != nil || f.Build != "1.21-51.0.1" {
		t.Errorf("latest fallback = %+v, %v", f, err)
	}
	if _, err := r.Forge(ctx, "1.20.1", "1.21-51.0.1"); err == nil {
		t.Error("mismatched build accepted")
	}
	var nf *NotFoundError
	if _, err := r.Forge(ctx, "1.12.2", ""); !errors.As(err, &nf) {
		t.Errorf("no promo: %v", err)
	}
	if got := ForgeArgsFile("1.20.1-47.2.0"); got != "libraries/net/minecraftforge/forge/1.20.1-47.2.0/unix_args.txt" {
		t.Errorf("args file = %s", got)
	}
}

func TestModrinth(t *testing.T) {
	r := fakeUpstreams(t)
	m, err := r.Modrinth(context.Background(), "fabric-api", "0.158.0+26.3")
	if err != nil {
		t.Fatal(err)
	}
	if m.File.Filename != "fabric-api-0.158.0+26.3.jar" || m.File.Digest.Hex != "cafebabe" || m.File.Digest.Algorithm != "sha512" {
		t.Errorf("primary file = %+v", m.File)
	}
	if !m.Supports("26.3", "fabric") || m.Supports("26.2", "fabric") || m.Supports("26.3", "forge") {
		t.Error("Supports wrong")
	}
	var nf *NotFoundError
	if _, err := r.Modrinth(context.Background(), "fabric-api", "nope"); !errors.As(err, &nf) {
		t.Errorf("missing version: %v", err)
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.19.10", "0.19.5", 1},
		{"0.19.5", "0.19.10", -1},
		{"1.0", "1.0.0", -1},
		{"1.0.0", "1.0.0", 0},
		{"0.16.0-beta.1", "0.16.0", -1},
	}
	for _, c := range cases {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compare(%s, %s) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
