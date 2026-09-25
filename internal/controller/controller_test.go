package controller

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/andreabedini/minecraft-operator/api/v1alpha1"
	"github.com/andreabedini/minecraft-operator/gen/supervisor/v1/supervisorv1connect"
	"github.com/andreabedini/minecraft-operator/internal/supervisor"
	"github.com/andreabedini/minecraft-operator/internal/supervisorclient"
	"github.com/andreabedini/minecraft-operator/internal/upstream"
)

// harness wires a fake API server, a real supervisor over a temp directory,
// fake publishers and a fake "java" on PATH.
type harness struct {
	t        *testing.T
	client   client.Client
	rec      *MinecraftInstanceReconciler
	recorder *record.FakeRecorder
	dataDir  string
	sup      *supervisor.Server
	artifact *httptest.Server
	jarBody  string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	s := runtime.NewScheme()
	if err := scheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}

	// Fake java: prints the ready line, exits on "stop".
	binDir := t.TempDir()
	script := "#!/bin/sh\necho \"Done (1.0s)! For help, type help\"\nwhile read -r l; do [ \"$l\" = stop ] && exit 0; done\n"
	if err := os.WriteFile(filepath.Join(binDir, "java"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// Artifacts.
	jarBody := "fake jar bytes"
	artifact := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, jarBody)
	}))
	t.Cleanup(artifact.Close)

	// Publishers.
	mux := http.NewServeMux()
	var pub *httptest.Server
	mux.HandleFunc("/mojang/manifest.json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"versions":[{"id":"26.3","url":"` + pub.URL + `/mojang/26.3.json"}]}`))
	})
	mux.HandleFunc("/mojang/26.3.json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"javaVersion":{"majorVersion":25},"downloads":{"server":{"url":"` + artifact.URL + `/server.jar","sha1":"` + sha1hex(jarBody) + `"}}}`))
	})
	mux.HandleFunc("/fabric/versions/loader/26.3", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"loader":{"version":"0.19.5","stable":true},"intermediary":{"version":"26.3","stable":true}}]`))
	})
	mux.HandleFunc("/fabric/versions/installer", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"version":"1.1.2","stable":true}]`))
	})
	mux.HandleFunc("/fabric/versions/loader/26.3/0.19.5/1.1.2/server/jar", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, jarBody)
	})
	mux.HandleFunc("/modrinth/project/fabric-api/version/0.161.0+26.3", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"v1","project_id":"P7dR8mSH","version_number":"0.161.0+26.3","game_versions":["26.3"],"loaders":["fabric"],
		 "files":[{"hashes":{"sha512":"` + sha512hex(jarBody) + `"},"url":"` + artifact.URL + `/fabric-api.jar","filename":"fabric-api-0.161.0+26.3.jar","primary":true}]}`))
	})
	pub = httptest.NewServer(mux)
	t.Cleanup(pub.Close)
	resolver := &upstream.Resolver{
		Client:            pub.Client(),
		MojangManifestURL: pub.URL + "/mojang/manifest.json",
		FabricMetaURL:     pub.URL + "/fabric",
		PaperAPIURL:       pub.URL + "/paper",
		ForgeFilesURL:     pub.URL + "/forge",
		ForgeMavenURL:     pub.URL + "/forgemaven",
		ModrinthAPIURL:    pub.URL + "/modrinth",
	}

	// Real supervisor over a temp dir, no auth (the reconciler still sends
	// the token; the server ignores it without the interceptor).
	dataDir := t.TempDir()
	root, err := supervisor.NewRoot(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	state, err := supervisor.NewState(root)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	console := supervisor.NewConsole(100)
	mgr, err := supervisor.NewManager(root, state, console, logger)
	if err != nil {
		t.Fatal(err)
	}
	mgr.Passthrough = nil
	sup := supervisor.New(supervisor.Config{Version: "test", AllowInsecureDownloads: true}, root, state, console, mgr, logger)
	supMux := http.NewServeMux()
	supMux.Handle(supervisorv1connect.NewSupervisorServiceHandler(sup))
	supSrv := httptest.NewServer(h2c.NewHandler(supMux, &http2.Server{}))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = mgr.Stop(ctx, time.Second, true)
		supSrv.Close()
	})
	httpClient := supervisorclient.NewHTTPClient()

	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&v1alpha1.MinecraftInstance{}).Build()
	recorder := record.NewFakeRecorder(100)
	rec := &MinecraftInstanceReconciler{
		Client:            c,
		Scheme:            s,
		Recorder:          recorder,
		Resolver:          resolver,
		SupervisorImage:   "supervisor:test",
		JavaImageTemplate: "temurin:%d-jre",
		NewSupervisorClient: func(_ string, token string) *supervisorclient.Client {
			return supervisorclient.New(httpClient, supSrv.URL, token)
		},
	}
	return &harness{t: t, client: c, rec: rec, recorder: recorder, dataDir: dataDir, sup: sup, artifact: artifact, jarBody: jarBody}
}

func (h *harness) reconcile(name string) (ctrl.Result, *v1alpha1.MinecraftInstance) {
	h.t.Helper()
	res, err := h.rec.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "minecraft", Name: name}})
	if err != nil {
		h.t.Fatalf("reconcile: %v", err)
	}
	inst := &v1alpha1.MinecraftInstance{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: "minecraft", Name: name}, inst); err != nil {
		h.t.Fatal(err)
	}
	return res, inst
}

// addRunningPod simulates the kubelet: a running pod with an IP.
func (h *harness) addRunningPod(inst *v1alpha1.MinecraftInstance) {
	h.t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: inst.Name + "-abc", Namespace: inst.Namespace, Labels: selectorLabels(inst)},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "fd00::1"},
	}
	if err := h.client.Create(context.Background(), pod); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) drainEvents() []string {
	var out []string
	for {
		select {
		case e := <-h.recorder.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func condition(inst *v1alpha1.MinecraftInstance, t string) *metav1.Condition {
	return meta.FindStatusCondition(inst.Status.Conditions, t)
}

func TestReconcileCreatesResourcesAndInstalls(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	inst := &v1alpha1.MinecraftInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "craft", Namespace: "minecraft", UID: "uid-1", Generation: 1},
		Spec: v1alpha1.MinecraftInstanceSpec{
			Version: "26.3",
			Flavour: v1alpha1.FlavourSpec{Fabric: &v1alpha1.FabricFlavour{}},
			Mods:    []v1alpha1.ModSpec{{Name: "fabric-api", Modrinth: &v1alpha1.ModrinthSource{Project: "fabric-api", Version: "0.161.0+26.3"}}},
			ConfigFiles: []v1alpha1.ConfigFileSpec{
				{Path: "server.properties", ConfigMapKeyRef: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "props"}, Key: "server.properties"}, Merge: v1alpha1.MergeProperties},
				{Path: "config/geyser.yml", ConfigMapKeyRef: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "props"}, Key: "geyser.yml"}},
			},
			Service: v1alpha1.ServiceSpec{
				Game:       v1alpha1.GameServiceSpec{Annotations: map[string]string{"lbipam.cilium.io/sharing-key": "lodestone"}},
				ExtraPorts: []v1alpha1.ExtraPort{{Name: "bedrock", Port: 19132, Protocol: corev1.ProtocolUDP}},
			},
		},
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "props", Namespace: "minecraft"},
		Data: map[string]string{"server.properties": "motd=Hello\nmax-players=20\n", "geyser.yml": "bedrock:\n  port: 19132\n"}}
	for _, o := range []client.Object{inst, cm} {
		if err := h.client.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}

	// Pass 1: no pod yet. Resources must exist.
	res, inst := h.reconcile("craft")
	if res.RequeueAfter == 0 {
		t.Error("expected a requeue while the pod is pending")
	}
	var sec corev1.Secret
	if err := h.client.Get(ctx, types.NamespacedName{Namespace: "minecraft", Name: "craft-supervisor"}, &sec); err != nil {
		t.Fatal(err)
	}
	if len(sec.Data[secretKeyManagement]) != 40 || len(sec.Data[secretKeyToken]) != 64 {
		t.Errorf("secret keys: management %d, token %d", len(sec.Data[secretKeyManagement]), len(sec.Data[secretKeyToken]))
	}
	var pvc corev1.PersistentVolumeClaim
	if err := h.client.Get(ctx, types.NamespacedName{Namespace: "minecraft", Name: "craft-data"}, &pvc); err != nil {
		t.Fatal(err)
	}
	if len(pvc.OwnerReferences) != 0 {
		t.Error("retained PVC must not have an owner reference")
	}
	var dep appsv1.Deployment
	if err := h.client.Get(ctx, types.NamespacedName{Namespace: "minecraft", Name: "craft"}, &dep); err != nil {
		t.Fatal(err)
	}
	server := dep.Spec.Template.Spec.Containers[0]
	if server.Image != "temurin:25-jre" || dep.Spec.Template.Spec.InitContainers[0].Image != "supervisor:test" {
		t.Errorf("images = %s / %s", server.Image, dep.Spec.Template.Spec.InitContainers[0].Image)
	}
	if server.Command[0] != supervisorBinPath || *dep.Spec.Template.Spec.SecurityContext.RunAsUser != 1000 {
		t.Errorf("container = %+v", server)
	}
	var gameSvc corev1.Service
	if err := h.client.Get(ctx, types.NamespacedName{Namespace: "minecraft", Name: "craft-game"}, &gameSvc); err != nil {
		t.Fatal(err)
	}
	if gameSvc.Spec.Type != corev1.ServiceTypeLoadBalancer || len(gameSvc.Spec.Ports) != 2 || gameSvc.Annotations["lbipam.cilium.io/sharing-key"] != "lodestone" {
		t.Errorf("game service = %+v", gameSvc.Spec)
	}
	if inst.Status.Phase != "Pending" || condition(inst, v1alpha1.ConditionSupervisorReady).Status != metav1.ConditionFalse {
		t.Errorf("status = %+v", inst.Status)
	}

	// Pass 2: pod running → install, then start.
	h.addRunningPod(inst)
	_, inst = h.reconcile("craft")
	if c := condition(inst, v1alpha1.ConditionInstalled); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("Installed = %+v; events %v", c, h.drainEvents())
	}
	for _, f := range []string{"server.jar", "mods/fabric-api-0.161.0+26.3.jar", "eula.txt", "server.properties", "config/geyser.yml"} {
		data, err := os.ReadFile(filepath.Join(h.dataDir, f))
		if err != nil {
			t.Errorf("%s missing: %v", f, err)
			continue
		}
		if strings.HasSuffix(f, ".jar") && string(data) != h.jarBody {
			t.Errorf("%s content = %q", f, data)
		}
	}
	props, _ := os.ReadFile(filepath.Join(h.dataDir, "server.properties"))
	for _, want := range []string{"motd=Hello", "max-players=20", "enable-rcon=false", "management-server-enabled=true", "management-server-host=127.0.0.1", "management-server-secret=" + string(sec.Data[secretKeyManagement])} {
		if !strings.Contains(string(props), want) {
			t.Errorf("server.properties lacks %q:\n%s", want, props)
		}
	}
	if inst.Status.Resolved == nil || inst.Status.Resolved.JavaMajor != 25 || inst.Status.Resolved.LoaderVersion != "0.19.5" || inst.Status.Resolved.LaunchSpecHash == "" {
		t.Errorf("resolved = %+v", inst.Status.Resolved)
	}
	if inst.Status.Resolved.ServerJar.Digest == "" {
		t.Error("observed digest of the unhashed launcher jar not recorded")
	}
	if c := condition(inst, v1alpha1.ConditionRunning); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("Running = %+v; events %v", c, h.drainEvents())
	}
	if inst.Status.Phase != "Ready" {
		t.Errorf("phase = %s", inst.Status.Phase)
	}
	events := strings.Join(h.drainEvents(), "\n")
	if !strings.Contains(events, "Downloaded") || !strings.Contains(events, "Started") {
		t.Errorf("events = %s", events)
	}

	// Pass 3: nothing changed → no downloads, still running, no drift.
	_, inst = h.reconcile("craft")
	events = strings.Join(h.drainEvents(), "\n")
	if strings.Contains(events, "Downloaded") {
		t.Errorf("re-downloaded on a no-op pass: %s", events)
	}
	if c := condition(inst, v1alpha1.ConditionConfigDrift); c == nil || c.Status != metav1.ConditionFalse {
		t.Errorf("ConfigDrift = %+v", c)
	}

	// Pass 4: config change while running → staged, drift reported, not applied.
	cm.Data["server.properties"] = "motd=Changed\nmax-players=20\n"
	if err := h.client.Update(ctx, cm); err != nil {
		t.Fatal(err)
	}
	_, inst = h.reconcile("craft")
	if c := condition(inst, v1alpha1.ConditionConfigDrift); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("ConfigDrift after change = %+v", c)
	}
	props, _ = os.ReadFile(filepath.Join(h.dataDir, "server.properties"))
	if strings.Contains(string(props), "motd=Changed") {
		t.Error("live properties changed while running")
	}
	staged, err := os.ReadFile(filepath.Join(h.dataDir, ".supervisor", "staged", "server.properties"))
	if err != nil || !strings.Contains(string(staged), "motd=Changed") {
		t.Errorf("staged properties = %q, %v", staged, err)
	}

	// Pass 5: stopped=true → server stops, staged changes applied, phase Stopped.
	if err := h.client.Get(ctx, types.NamespacedName{Namespace: "minecraft", Name: "craft"}, inst); err != nil {
		t.Fatal(err)
	}
	inst.Spec.Stopped = true
	if err := h.client.Update(ctx, inst); err != nil {
		t.Fatal(err)
	}
	_, inst = h.reconcile("craft")
	if c := condition(inst, v1alpha1.ConditionRunning); c == nil || c.Status != metav1.ConditionFalse || c.Reason != "Stopped" {
		t.Errorf("Running after stop = %+v", c)
	}
	if inst.Status.Phase != "Stopped" {
		t.Errorf("phase = %s", inst.Status.Phase)
	}
	props, _ = os.ReadFile(filepath.Join(h.dataDir, "server.properties"))
	if !strings.Contains(string(props), "motd=Changed") {
		t.Errorf("staged change not applied after stop:\n%s", props)
	}
	if c := condition(inst, v1alpha1.ConditionConfigDrift); c == nil || c.Status != metav1.ConditionFalse {
		t.Errorf("ConfigDrift after apply = %+v", c)
	}

	// Pass 6: stopped=false → starts again.
	if err := h.client.Get(ctx, types.NamespacedName{Namespace: "minecraft", Name: "craft"}, inst); err != nil {
		t.Fatal(err)
	}
	inst.Spec.Stopped = false
	if err := h.client.Update(ctx, inst); err != nil {
		t.Fatal(err)
	}
	_, inst = h.reconcile("craft")
	if inst.Status.Phase != "Ready" {
		t.Errorf("phase after restart = %s, events %v", inst.Status.Phase, h.drainEvents())
	}
}

func TestReconcileAdoptsExistingDirectoryAndReportsUnmanagedMods(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	// Pre-existing install: jar, a mod we know, a mod we do not, properties with user keys.
	for f, content := range map[string]string{
		"server.jar":                       h.jarBody,
		"mods/journeymap.jar":              "jm",
		"server.properties":                "motd=Old world\nview-distance=12\nenable-rcon=true\nrcon.password=hunter2\n",
		"world/level.dat":                  "nbt",
		"mods/fabric-api-0.161.0+26.3.jar": h.jarBody,
	} {
		p := filepath.Join(h.dataDir, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	inst := &v1alpha1.MinecraftInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "adopt", Namespace: "minecraft", UID: "uid-2", Generation: 1},
		Spec: v1alpha1.MinecraftInstanceSpec{
			Version: "26.3",
			Flavour: v1alpha1.FlavourSpec{Fabric: &v1alpha1.FabricFlavour{}},
			Mods:    []v1alpha1.ModSpec{{Name: "fabric-api", Modrinth: &v1alpha1.ModrinthSource{Project: "fabric-api", Version: "0.161.0+26.3"}}},
			Storage: v1alpha1.StorageSpec{ExistingClaim: "lodestone-data", SubPath: "instances/x"},
			Stopped: true,
		},
	}
	if err := h.client.Create(ctx, inst); err != nil {
		t.Fatal(err)
	}
	_, inst = h.reconcile("adopt")
	h.addRunningPod(inst)
	_, inst = h.reconcile("adopt")

	var pvc corev1.PersistentVolumeClaim
	if err := h.client.Get(ctx, types.NamespacedName{Namespace: "minecraft", Name: "adopt-data"}, &pvc); err == nil {
		t.Error("operator created a PVC despite existingClaim")
	}
	var dep appsv1.Deployment
	if err := h.client.Get(ctx, types.NamespacedName{Namespace: "minecraft", Name: "adopt"}, &dep); err != nil {
		t.Fatal(err)
	}
	vol := dep.Spec.Template.Spec.Volumes[0]
	mount := dep.Spec.Template.Spec.Containers[0].VolumeMounts[0]
	if vol.PersistentVolumeClaim.ClaimName != "lodestone-data" || mount.SubPath != "instances/x" {
		t.Errorf("volume = %+v mount = %+v", vol, mount)
	}
	if c := condition(inst, v1alpha1.ConditionInstalled); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("Installed = %+v events %v", c, h.drainEvents())
	}
	events := strings.Join(h.drainEvents(), "\n")
	// The launcher jar has no publisher digest, so the existing file is
	// adopted as is; the mod matched its sha512. Nothing is downloaded.
	if strings.Contains(events, "Downloaded") {
		t.Errorf("adoption downloaded something: %s", events)
	}
	if inst.Status.Resolved == nil || inst.Status.Resolved.ServerJar == nil || !strings.HasPrefix(inst.Status.Resolved.ServerJar.Digest, "sha256:") {
		t.Errorf("adopted jar digest not recorded: %+v", inst.Status.Resolved)
	}
	if !strings.Contains(events, "UnmanagedMods") || !strings.Contains(events, "journeymap.jar") {
		t.Errorf("unmanaged mod not reported: %s", events)
	}
	if _, err := os.Stat(filepath.Join(h.dataDir, "mods", "journeymap.jar")); err != nil {
		t.Error("unmanaged mod was deleted")
	}
	props, _ := os.ReadFile(filepath.Join(h.dataDir, "server.properties"))
	for _, want := range []string{"motd=Old world", "view-distance=12", "enable-rcon=false", "rcon.password=hunter2", "management-server-enabled=true"} {
		if !strings.Contains(string(props), want) {
			t.Errorf("properties lack %q:\n%s", want, props)
		}
	}
	if inst.Status.Phase != "Stopped" {
		t.Errorf("phase = %s", inst.Status.Phase)
	}
}

func TestPodOverridesApply(t *testing.T) {
	inst := &v1alpha1.MinecraftInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "o", Namespace: "n"},
		Spec: v1alpha1.MinecraftInstanceSpec{
			PodOverrides: &runtime.RawExtension{Raw: []byte(`{"spec":{"initContainers":[{"name":"clat","image":"clat:1","restartPolicy":"Always"}],
			 "securityContext":{"sysctls":[{"name":"net.ipv6.conf.all.forwarding","value":"1"}]},
			 "containers":[{"name":"server","env":[{"name":"EXTRA","value":"1"}]}]}}`)},
		},
	}
	dep, err := buildDeployment(inst, DeploymentInput{JavaImage: "j", SupervisorImage: "s"})
	if err != nil {
		t.Fatal(err)
	}
	spec := dep.Spec.Template.Spec
	if len(spec.InitContainers) != 2 || spec.InitContainers[1].Name != "clat" && spec.InitContainers[0].Name != "clat" {
		t.Errorf("init containers = %+v", spec.InitContainers)
	}
	if len(spec.SecurityContext.Sysctls) != 1 || *spec.SecurityContext.RunAsUser != 1000 {
		t.Errorf("security context = %+v", spec.SecurityContext)
	}
	var server *corev1.Container
	for i := range spec.Containers {
		if spec.Containers[i].Name == "server" {
			server = &spec.Containers[i]
		}
	}
	if server == nil || server.Image != "j" || len(server.Ports) < 2 {
		t.Fatalf("server container lost fields: %+v", server)
	}
	found := false
	for _, e := range server.Env {
		if e.Name == "EXTRA" {
			found = true
		}
	}
	if !found {
		t.Errorf("env not merged: %+v", server.Env)
	}
	if dep.Spec.Template.Labels[labelInstance] != "o" {
		t.Error("selector label lost")
	}
}
