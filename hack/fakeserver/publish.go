package main

import (
	"crypto/sha1" //nolint:gosec // Mojang publishes sha1
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// publish serves the subset of the Mojang and Fabric publisher APIs that the
// operator's resolver needs, pointing every download at itself. It lets the
// end-to-end tests run without internet access.
//
//	java publish -listen :8080 -public-url http://fake-upstream.minecraft-operator.svc:8080
func publish(args []string) {
	fs := flag.NewFlagSet("publish", flag.ExitOnError)
	listen := fs.String("listen", ":8080", "address to serve on")
	publicURL := fs.String("public-url", "http://127.0.0.1:8080", "URL under which clients reach this server")
	version := fs.String("version", "26.3", "the one Minecraft version to publish")
	javaMajor := fs.Int("java-major", 25, "Java major to report for the version")
	_ = fs.Parse(args)
	base := strings.TrimRight(*publicURL, "/")

	jar := []byte("fake minecraft server jar " + *version + "\n")
	sum := sha1.Sum(jar) //nolint:gosec
	jarSHA1 := hex.EncodeToString(sum[:])

	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("/mojang/version_manifest_v2.json", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"latest":   map[string]string{"release": *version, "snapshot": *version},
			"versions": []map[string]any{{"id": *version, "type": "release", "url": base + "/mojang/" + *version + ".json", "sha1": "0", "releaseTime": time.Now().UTC().Format(time.RFC3339)}},
		})
	})
	mux.HandleFunc("/mojang/"+*version+".json", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"id":          *version,
			"javaVersion": map[string]any{"component": "fake", "majorVersion": *javaMajor},
			"downloads":   map[string]any{"server": map[string]any{"url": base + "/server.jar", "sha1": jarSHA1, "size": len(jar)}},
		})
	})
	mux.HandleFunc("/server.jar", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/java-archive")
		_, _ = w.Write(jar)
	})
	mux.HandleFunc("/fabric/versions/loader/"+*version, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []map[string]any{{
			"loader":       map[string]any{"version": "0.19.5", "stable": true},
			"intermediary": map[string]any{"version": *version, "stable": true},
		}})
	})
	mux.HandleFunc("/fabric/versions/installer", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []map[string]any{{"version": "1.1.2", "stable": true}})
	})
	mux.HandleFunc("/fabric/versions/loader/"+*version+"/0.19.5/1.1.2/server/jar", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/java-archive")
		_, _ = w.Write(jar)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })

	fmt.Fprintf(os.Stderr, "fake publisher serving %s on %s (java %d)\n", *version, *listen, *javaMajor)
	srv := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
