//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/andreabedini/minecraft-operator/api/v1alpha1"
)

const (
	readyTimeout = 3 * time.Minute
	poll         = 2 * time.Second
)

// instanceSpec is a vanilla instance on the fake publisher and fake server.
func instanceSpec(name string) *v1alpha1.MinecraftInstance {
	return &v1alpha1.MinecraftInstance{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: v1alpha1.MinecraftInstanceSpec{
			Version: "26.3",
			Flavour: v1alpha1.FlavourSpec{Vanilla: &v1alpha1.VanillaFlavour{}},
			Storage: v1alpha1.StorageSpec{Size: ptr.To(resource.MustParse("1Gi"))},
			Service: v1alpha1.ServiceSpec{Game: v1alpha1.GameServiceSpec{Type: corev1.ServiceTypeClusterIP}},
			JVM:     v1alpha1.JVMSpec{MinMemoryMiB: 64, MaxMemoryMiB: 128},
		},
	}
}

type harness struct {
	t   *testing.T
	g   gomega.Gomega
	c   client.Client
	ctx context.Context
}

func newHarness(ctx context.Context, t *testing.T, cfg *envconf.Config) *harness {
	t.Helper()
	c, err := newClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, g: gomega.NewWithT(t), c: c, ctx: ctx}
}

func (h *harness) instance(name string) *v1alpha1.MinecraftInstance {
	inst := &v1alpha1.MinecraftInstance{}
	h.g.Expect(h.c.Get(h.ctx, types.NamespacedName{Namespace: testNamespace, Name: name}, inst)).To(gomega.Succeed())
	return inst
}

func (h *harness) waitReady(name string) *v1alpha1.MinecraftInstance {
	h.t.Helper()
	var inst *v1alpha1.MinecraftInstance
	h.g.Eventually(func() string {
		inst = h.instance(name)
		return inst.Status.Phase
	}).WithTimeout(readyTimeout).WithPolling(poll).Should(gomega.Equal("Ready"), func() string {
		return describeInstance(inst)
	}())
	return inst
}

func describeInstance(inst *v1alpha1.MinecraftInstance) string {
	if inst == nil {
		return "no instance"
	}
	out := "phase " + inst.Status.Phase
	for _, c := range inst.Status.Conditions {
		out += "\n  " + c.Type + "=" + string(c.Status) + " " + c.Reason + ": " + c.Message
	}
	return out
}

func (h *harness) serverPod(name string) *corev1.Pod {
	h.t.Helper()
	var pods corev1.PodList
	h.g.Expect(h.c.List(h.ctx, &pods, client.InNamespace(testNamespace), client.MatchingLabels{"minecraft.bedini.au/instance": name})).To(gomega.Succeed())
	var running *corev1.Pod
	for i := range pods.Items {
		if pods.Items[i].Status.Phase == corev1.PodRunning && pods.Items[i].DeletionTimestamp.IsZero() {
			running = &pods.Items[i]
		}
	}
	h.g.Expect(running).NotTo(gomega.BeNil(), "no running server pod for %s", name)
	return running
}

func (h *harness) events(name, reason string) int {
	var events corev1.EventList
	h.g.Expect(h.c.List(h.ctx, &events, client.InNamespace(testNamespace))).To(gomega.Succeed())
	n := 0
	for _, e := range events.Items {
		if e.InvolvedObject.Name == name && e.InvolvedObject.Kind == "MinecraftInstance" && e.Reason == reason {
			n += int(max(e.Count, 1))
		}
	}
	return n
}

func (h *harness) scaleOperator(replicas int32) {
	h.t.Helper()
	var dep appsv1.Deployment
	h.g.Expect(h.c.Get(h.ctx, types.NamespacedName{Namespace: operatorNamespace, Name: operatorName}, &dep)).To(gomega.Succeed())
	dep.Spec.Replicas = ptr.To(replicas)
	h.g.Expect(h.c.Update(h.ctx, &dep)).To(gomega.Succeed())
	h.g.Eventually(func() int32 {
		var d appsv1.Deployment
		_ = h.c.Get(h.ctx, types.NamespacedName{Namespace: operatorNamespace, Name: operatorName}, &d)
		if replicas == 0 {
			return d.Status.Replicas
		}
		return d.Status.AvailableReplicas
	}).WithTimeout(2 * time.Minute).WithPolling(poll).Should(gomega.Equal(replicas))
}

func TestInstanceLifecycle(t *testing.T) {
	const name = "craft"

	create := features.New("instance becomes ready").
		WithLabel("tier", "e2e").
		Assess("the operator creates the resources and the server starts", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			h := newHarness(ctx, t, cfg)
			h.g.Expect(h.c.Create(ctx, instanceSpec(name))).To(gomega.Succeed())
			inst := h.waitReady(name)

			h.g.Expect(inst.Status.Resolved).NotTo(gomega.BeNil())
			h.g.Expect(inst.Status.Resolved.JavaMajor).To(gomega.Equal(int32(25)))
			h.g.Expect(inst.Status.ManagementProtocolVersion).To(gomega.Equal("3.1.0"))
			h.g.Expect(meta.IsStatusConditionTrue(inst.Status.Conditions, v1alpha1.ConditionInstalled)).To(gomega.BeTrue())

			for _, obj := range []client.Object{
				&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: name + "-data"}},
				&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: name + "-supervisor"}},
				&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: name + "-game"}},
				&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: name + "-supervisor"}},
				&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: name}},
			} {
				h.g.Expect(h.c.Get(ctx, client.ObjectKeyFromObject(obj), obj)).To(gomega.Succeed(), "%T %s", obj, obj.GetName())
			}
			return ctx
		}).
		Assess("the fake player join shows up in status and events", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			h := newHarness(ctx, t, cfg)
			h.g.Eventually(func() int32 {
				inst := h.instance(name)
				if inst.Status.Players == nil {
					return -1
				}
				return inst.Status.Players.Online
			}).WithTimeout(time.Minute).WithPolling(poll).Should(gomega.Equal(int32(1)))
			h.g.Eventually(func() int { return h.events(name, "PlayerJoined") }).WithTimeout(time.Minute).WithPolling(poll).Should(gomega.BeNumerically(">=", 1))
			return ctx
		}).Feature()

	survives := features.New("server survives an operator restart").
		WithLabel("tier", "e2e").
		Assess("deleting the operator pod leaves the server pod untouched", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			h := newHarness(ctx, t, cfg)
			before := h.serverPod(name)
			startedBefore := h.events(name, "Started")

			var pods corev1.PodList
			h.g.Expect(h.c.List(ctx, &pods, client.InNamespace(operatorNamespace), client.MatchingLabels{"app.kubernetes.io/name": operatorName})).To(gomega.Succeed())
			h.g.Expect(pods.Items).NotTo(gomega.BeEmpty())
			for i := range pods.Items {
				h.g.Expect(h.c.Delete(ctx, &pods.Items[i])).To(gomega.Succeed())
			}
			h.g.Eventually(func() int32 {
				var d appsv1.Deployment
				_ = h.c.Get(ctx, types.NamespacedName{Namespace: operatorNamespace, Name: operatorName}, &d)
				return d.Status.AvailableReplicas
			}).WithTimeout(2 * time.Minute).WithPolling(poll).Should(gomega.Equal(int32(1)))

			// Give the new operator a full reconcile cycle to do something wrong.
			time.Sleep(20 * time.Second)
			after := h.serverPod(name)
			h.g.Expect(after.UID).To(gomega.Equal(before.UID), "server pod was replaced")
			for _, cs := range after.Status.ContainerStatuses {
				h.g.Expect(cs.RestartCount).To(gomega.BeZero(), "container %s restarted", cs.Name)
			}
			h.g.Expect(h.events(name, "Started")).To(gomega.Equal(startedBefore), "operator started the server again")
			h.g.Expect(h.waitReady(name).Status.Phase).To(gomega.Equal("Ready"))
			return ctx
		}).Feature()

	autostart := features.New("supervisor autostarts without the operator").
		WithLabel("tier", "e2e").
		Assess("a replaced server pod comes back on its own while the operator is down", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			h := newHarness(ctx, t, cfg)
			h.scaleOperator(0)
			defer h.scaleOperator(1)

			old := h.serverPod(name)
			h.g.Expect(h.c.Delete(ctx, old)).To(gomega.Succeed())

			// Pod readiness follows the game port, so a Ready pod means the
			// fake server is listening, started by the supervisor alone.
			h.g.Eventually(func() bool {
				var pods corev1.PodList
				if err := h.c.List(ctx, &pods, client.InNamespace(testNamespace), client.MatchingLabels{"minecraft.bedini.au/instance": name}); err != nil {
					return false
				}
				for _, p := range pods.Items {
					if p.UID == old.UID || !p.DeletionTimestamp.IsZero() {
						continue
					}
					for _, c := range p.Status.Conditions {
						if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
							return true
						}
					}
				}
				return false
			}).WithTimeout(readyTimeout).WithPolling(poll).Should(gomega.BeTrue(), "new server pod never became ready with the operator scaled to zero")
			return ctx
		}).
		Assess("the operator reattaches when it returns", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			h := newHarness(ctx, t, cfg)
			inst := h.waitReady(name)
			h.g.Expect(inst.Status.Players).NotTo(gomega.BeNil())
			return ctx
		}).Feature()

	deletion := features.New("deleting the instance keeps the claim").
		WithLabel("tier", "e2e").
		Assess("the deployment goes away and the PVC stays", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			h := newHarness(ctx, t, cfg)
			inst := h.instance(name)
			h.g.Expect(h.c.Delete(ctx, inst)).To(gomega.Succeed())
			h.g.Eventually(func() bool {
				var dep appsv1.Deployment
				err := h.c.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: name}, &dep)
				return apierrors.IsNotFound(err)
			}).WithTimeout(2 * time.Minute).WithPolling(poll).Should(gomega.BeTrue())
			var pvc corev1.PersistentVolumeClaim
			h.g.Expect(h.c.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: name + "-data"}, &pvc)).To(gomega.Succeed(), "PVC was deleted with the instance")
			h.g.Expect(pvc.DeletionTimestamp.IsZero()).To(gomega.BeTrue())
			return ctx
		}).Feature()

	testenv.Test(t, create, survives, autostart, deletion)
}
