// Package controller reconciles MinecraftInstance resources.
package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/andreabedini/minecraft-operator/api/v1alpha1"
	supervisorv1 "github.com/andreabedini/minecraft-operator/gen/supervisor/v1"
	"github.com/andreabedini/minecraft-operator/internal/plan"
	"github.com/andreabedini/minecraft-operator/internal/supervisorclient"
	"github.com/andreabedini/minecraft-operator/internal/upstream"
)

const (
	requeueSoon    = 10 * time.Second
	requeueRunning = 60 * time.Second
	requeueResolve = 5 * time.Minute
)

// MinecraftInstanceReconciler reconciles a MinecraftInstance.
type MinecraftInstanceReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	Resolver *upstream.Resolver

	// SupervisorImage is the image whose init container copies the
	// supervisor binary into the pod.
	SupervisorImage string
	// JavaImageTemplate receives the Java major with %d.
	JavaImageTemplate string
	// NewSupervisorClient builds a client for a pod. Tests override it.
	NewSupervisorClient func(baseURL, token string) *supervisorclient.Client

	plans sync.Map // string(uid)/generation → *plan.Plan
}

// +kubebuilder:rbac:groups=minecraft.bedini.au,resources=minecraftinstances,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=minecraft.bedini.au,resources=minecraftinstances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=minecraft.bedini.au,resources=minecraftinstances/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services;persistentvolumeclaims;secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// SetupWithManager registers the controller.
func (r *MinecraftInstanceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.NewSupervisorClient == nil {
		httpClient := supervisorclient.NewHTTPClient()
		r.NewSupervisorClient = func(baseURL, token string) *supervisorclient.Client {
			return supervisorclient.New(httpClient, baseURL, token)
		}
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.MinecraftInstance{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.Secret{}).
		Complete(r)
}

// Reconcile drives one instance toward its spec.
func (r *MinecraftInstanceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	inst := &v1alpha1.MinecraftInstance{}
	if err := r.Get(ctx, req.NamespacedName, inst); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !inst.DeletionTimestamp.IsZero() {
		// Owned resources are garbage collected; the PVC follows
		// retainOnDelete through its owner reference.
		return ctrl.Result{}, nil
	}

	orig := inst.DeepCopy()
	result, err := r.reconcile(ctx, inst)
	inst.Status.ObservedGeneration = inst.Generation
	inst.Status.Phase = phaseOf(inst)
	if statusErr := r.Status().Patch(ctx, inst, client.MergeFrom(orig)); statusErr != nil && !apierrors.IsNotFound(statusErr) {
		if err == nil {
			err = statusErr
		} else {
			logger.Error(statusErr, "status patch failed")
		}
	}
	return result, err
}

func (r *MinecraftInstanceReconciler) reconcile(ctx context.Context, inst *v1alpha1.MinecraftInstance) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	secret, err := r.ensureSecret(ctx, inst)
	if err != nil {
		return ctrl.Result{}, err
	}

	p, err := r.resolvePlan(ctx, inst, string(secret.Data[secretKeyManagement]))
	if err != nil {
		var inc *plan.IncompatibleModError
		if errors.As(err, &inc) {
			setCondition(inst, v1alpha1.ConditionUpgradeBlocked, metav1.ConditionTrue, "IncompatibleMod", err.Error())
			r.Recorder.Event(inst, corev1.EventTypeWarning, "UpgradeBlocked", err.Error())
			return ctrl.Result{}, nil
		}
		setCondition(inst, v1alpha1.ConditionInstalled, metav1.ConditionFalse, "ResolveFailed", err.Error())
		r.Recorder.Event(inst, corev1.EventTypeWarning, "ResolveFailed", err.Error())
		return ctrl.Result{RequeueAfter: requeueResolve}, nil
	}
	setCondition(inst, v1alpha1.ConditionUpgradeBlocked, metav1.ConditionFalse, "Resolved", "")

	if inst.Spec.Storage.ExistingClaim == "" {
		if err := r.ensurePVC(ctx, inst); err != nil {
			return ctrl.Result{}, err
		}
	}
	if err := r.ensureServices(ctx, inst); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureDeployment(ctx, inst, p); err != nil {
		return ctrl.Result{}, err
	}

	pod, err := r.findPod(ctx, inst)
	if err != nil {
		return ctrl.Result{}, err
	}
	if pod == nil || pod.Status.PodIP == "" {
		setCondition(inst, v1alpha1.ConditionSupervisorReady, metav1.ConditionFalse, "PodPending", "no running pod yet")
		setCondition(inst, v1alpha1.ConditionRunning, metav1.ConditionFalse, "PodPending", "")
		setCondition(inst, v1alpha1.ConditionReady, metav1.ConditionFalse, "PodPending", "")
		return ctrl.Result{RequeueAfter: requeueSoon}, nil
	}
	sup := r.NewSupervisorClient(supervisorURL(pod.Status.PodIP), string(secret.Data[secretKeyToken]))
	status, err := sup.Status(ctx)
	if err != nil {
		setCondition(inst, v1alpha1.ConditionSupervisorReady, metav1.ConditionFalse, "Unreachable", trimErr(err))
		return ctrl.Result{RequeueAfter: requeueSoon}, nil
	}
	setCondition(inst, v1alpha1.ConditionSupervisorReady, metav1.ConditionTrue, "Reachable", "")
	inst.Status.SupervisorVersion = status.GetSupervisorVersion()
	running := status.GetState() != supervisorv1.ProcessState_PROCESS_STATE_STOPPED

	// Stop when asked to.
	if inst.Spec.Stopped && running {
		logger.Info("stopping server as requested")
		if _, err := sup.Stop(ctx, 0, false); err != nil {
			return ctrl.Result{}, fmt.Errorf("stop: %w", err)
		}
		r.Recorder.Event(inst, corev1.EventTypeNormal, "Stopped", "server stopped because spec.stopped is true")
		running = false
	}

	in := &installer{client: r.Client, sup: sup, inst: inst, plan: p, running: running, observed: inst.Status.Resolved}
	state, err := in.run(ctx)
	if err != nil {
		setCondition(inst, v1alpha1.ConditionInstalled, metav1.ConditionFalse, "InstallFailed", trimErr(err))
		r.Recorder.Event(inst, corev1.EventTypeWarning, "InstallFailed", trimErr(err))
		return ctrl.Result{RequeueAfter: requeueSoon}, nil
	}
	for _, f := range state.Downloaded {
		r.Recorder.Eventf(inst, corev1.EventTypeNormal, "Downloaded", "installed %s", f)
	}
	resolved := p.Resolved
	resolved.LaunchSpecHash = state.LaunchHash
	inst.Status.Resolved = &resolved
	if len(state.Unmanaged) > 0 {
		sort.Strings(state.Unmanaged)
		r.Recorder.Eventf(inst, corev1.EventTypeWarning, "UnmanagedMods", "jars in %s not listed in spec.mods: %s", p.ModsDir, strings.Join(state.Unmanaged, ", "))
	}
	if state.Installed {
		setCondition(inst, v1alpha1.ConditionInstalled, metav1.ConditionTrue, "Installed", "")
	} else {
		setCondition(inst, v1alpha1.ConditionInstalled, metav1.ConditionFalse, "RestartRequired", "pending while running: "+strings.Join(state.Pending, ", "))
	}
	if state.NeedsRestart {
		setCondition(inst, v1alpha1.ConditionConfigDrift, metav1.ConditionTrue, "StagedChanges", "staged changes await a restart")
	} else {
		setCondition(inst, v1alpha1.ConditionConfigDrift, metav1.ConditionFalse, "InSync", "")
	}

	// Restart automatically on drift when allowed; start when stopped.
	if running && (state.NeedsRestart || !state.Installed) && inst.Spec.RestartPolicy == v1alpha1.RestartAutomatic {
		logger.Info("restarting server to apply changes")
		if _, err := sup.Stop(ctx, 0, false); err != nil {
			return ctrl.Result{}, fmt.Errorf("stop for restart: %w", err)
		}
		r.Recorder.Event(inst, corev1.EventTypeNormal, "Restarting", "stopped to apply staged changes")
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	if !running && !inst.Spec.Stopped && state.Installed {
		pid, err := sup.Start(ctx)
		if err != nil {
			setCondition(inst, v1alpha1.ConditionRunning, metav1.ConditionFalse, "StartFailed", trimErr(err))
			r.Recorder.Event(inst, corev1.EventTypeWarning, "StartFailed", trimErr(err))
			return ctrl.Result{RequeueAfter: requeueSoon}, nil
		}
		r.Recorder.Eventf(inst, corev1.EventTypeNormal, "Started", "server started, pid %d", pid)
		running = true
	}
	if running {
		setCondition(inst, v1alpha1.ConditionRunning, metav1.ConditionTrue, "ProcessAlive", "")
	} else if inst.Spec.Stopped {
		setCondition(inst, v1alpha1.ConditionRunning, metav1.ConditionFalse, "Stopped", "spec.stopped is true")
	} else {
		setCondition(inst, v1alpha1.ConditionRunning, metav1.ConditionFalse, "NotStarted", "")
	}
	// Ready follows the management protocol from phase 3; until then it
	// mirrors Running.
	if running {
		setCondition(inst, v1alpha1.ConditionReady, metav1.ConditionTrue, "Running", "")
	} else {
		setCondition(inst, v1alpha1.ConditionReady, metav1.ConditionFalse, "NotRunning", "")
	}
	return ctrl.Result{RequeueAfter: requeueRunning}, nil
}

// resolvePlan returns the cached plan for this generation or resolves it.
func (r *MinecraftInstanceReconciler) resolvePlan(ctx context.Context, inst *v1alpha1.MinecraftInstance, managementSecret string) (*plan.Plan, error) {
	key := fmt.Sprintf("%s/%d/%s", inst.UID, inst.Generation, managementSecret)
	if cached, ok := r.plans.Load(key); ok {
		p := cached.(*plan.Plan)
		// Carry observed digests forward.
		if inst.Status.Resolved != nil {
			mergeObservedDigests(&p.Resolved, inst.Status.Resolved)
		}
		return p, nil
	}
	p, err := plan.Resolve(ctx, r.Resolver, &inst.Spec, plan.Options{
		JavaImageTemplate: r.JavaImageTemplate,
		ManagementSecret:  managementSecret,
		GamePort:          gamePort(inst),
	})
	if err != nil {
		return nil, err
	}
	if inst.Status.Resolved != nil {
		mergeObservedDigests(&p.Resolved, inst.Status.Resolved)
	}
	r.plans.Store(key, p)
	return p, nil
}

// mergeObservedDigests copies digests recorded in status into a freshly
// resolved status for files the publisher does not hash.
func mergeObservedDigests(dst, src *v1alpha1.ResolvedStatus) {
	if dst.ServerJar != nil && src.ServerJar != nil && dst.ServerJar.Digest == "" && dst.ServerJar.Path == src.ServerJar.Path &&
		dst.LoaderVersion == src.LoaderVersion && dst.InstallerVersion == src.InstallerVersion && dst.Version == src.Version {
		dst.ServerJar.Digest = src.ServerJar.Digest
	}
	for i := range dst.Mods {
		if dst.Mods[i].Digest != "" {
			continue
		}
		for _, m := range src.Mods {
			if m.File == dst.Mods[i].File {
				dst.Mods[i].Digest = m.Digest
			}
		}
	}
}

func (r *MinecraftInstanceReconciler) ensureSecret(ctx context.Context, inst *v1alpha1.MinecraftInstance) (*corev1.Secret, error) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName(inst), Namespace: inst.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		secret.Labels = instanceLabels(inst)
		if _, err := fillSecret(secret); err != nil {
			return err
		}
		return controllerutil.SetControllerReference(inst, secret, r.Scheme)
	})
	if err != nil {
		return nil, fmt.Errorf("secret: %w", err)
	}
	return secret, nil
}

func (r *MinecraftInstanceReconciler) ensurePVC(ctx context.Context, inst *v1alpha1.MinecraftInstance) error {
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: pvcName(inst), Namespace: inst.Namespace}}
	err := r.Get(ctx, client.ObjectKeyFromObject(pvc), pvc)
	if apierrors.IsNotFound(err) {
		pvc = buildPVC(inst)
		if !retainOnDelete(inst) {
			if err := controllerutil.SetControllerReference(inst, pvc, r.Scheme); err != nil {
				return err
			}
		}
		if err := r.Create(ctx, pvc); err != nil {
			return fmt.Errorf("pvc: %w", err)
		}
		r.Recorder.Eventf(inst, corev1.EventTypeNormal, "CreatedPVC", "created claim %s", pvc.Name)
		return nil
	}
	if err != nil {
		return err
	}
	// Only the owner reference follows the spec after creation; size and
	// class are immutable or need a deliberate expansion.
	orig := pvc.DeepCopy()
	if retainOnDelete(inst) {
		removeOwnerReference(pvc, inst)
	} else if err := controllerutil.SetControllerReference(inst, pvc, r.Scheme); err != nil {
		return err
	}
	if len(orig.OwnerReferences) != len(pvc.OwnerReferences) {
		return r.Patch(ctx, pvc, client.MergeFrom(orig))
	}
	return nil
}

func retainOnDelete(inst *v1alpha1.MinecraftInstance) bool {
	return inst.Spec.Storage.RetainOnDelete == nil || *inst.Spec.Storage.RetainOnDelete
}

func removeOwnerReference(obj client.Object, owner client.Object) {
	refs := obj.GetOwnerReferences()
	kept := refs[:0]
	for _, ref := range refs {
		if ref.UID != owner.GetUID() {
			kept = append(kept, ref)
		}
	}
	obj.SetOwnerReferences(kept)
}

func (r *MinecraftInstanceReconciler) ensureServices(ctx context.Context, inst *v1alpha1.MinecraftInstance) error {
	for _, desired := range []*corev1.Service{buildGameService(inst), buildSupervisorService(inst)} {
		svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: desired.Name, Namespace: desired.Namespace}}
		_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
			svc.Labels = desired.Labels
			svc.Annotations = mergeMaps(svc.Annotations, desired.Annotations)
			svc.Spec.Type = desired.Spec.Type
			svc.Spec.Selector = desired.Spec.Selector
			svc.Spec.Ports = desired.Spec.Ports
			svc.Spec.PublishNotReadyAddresses = desired.Spec.PublishNotReadyAddresses
			return controllerutil.SetControllerReference(inst, svc, r.Scheme)
		})
		if err != nil {
			return fmt.Errorf("service %s: %w", desired.Name, err)
		}
	}
	return nil
}

func mergeMaps(base, over map[string]string) map[string]string {
	if base == nil && over == nil {
		return nil
	}
	out := map[string]string{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}

func (r *MinecraftInstanceReconciler) ensureDeployment(ctx context.Context, inst *v1alpha1.MinecraftInstance, p *plan.Plan) error {
	supervisorImage := r.SupervisorImage
	if inst.Spec.Supervisor.Image != "" {
		supervisorImage = inst.Spec.Supervisor.Image
	}
	desired, err := buildDeployment(inst, DeploymentInput{JavaImage: p.JavaImage, SupervisorImage: supervisorImage})
	if err != nil {
		return err
	}
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: desired.Name, Namespace: desired.Namespace}}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		dep.Labels = desired.Labels
		dep.Spec = desired.Spec
		return controllerutil.SetControllerReference(inst, dep, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("deployment: %w", err)
	}
	return nil
}

// findPod returns the running pod for the instance, if any.
func (r *MinecraftInstanceReconciler) findPod(ctx context.Context, inst *v1alpha1.MinecraftInstance) (*corev1.Pod, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(inst.Namespace), client.MatchingLabels(selectorLabels(inst))); err != nil {
		return nil, err
	}
	var best *corev1.Pod
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !pod.DeletionTimestamp.IsZero() || pod.Status.Phase != corev1.PodRunning {
			continue
		}
		if best == nil || pod.CreationTimestamp.After(best.CreationTimestamp.Time) {
			best = pod
		}
	}
	return best, nil
}

func setCondition(inst *v1alpha1.MinecraftInstance, condType string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&inst.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: inst.Generation,
	})
}

func phaseOf(inst *v1alpha1.MinecraftInstance) string {
	is := func(t string) bool { return meta.IsStatusConditionTrue(inst.Status.Conditions, t) }
	switch {
	case is(v1alpha1.ConditionUpgradeBlocked):
		return "Blocked"
	case is(v1alpha1.ConditionReady):
		return "Ready"
	case is(v1alpha1.ConditionRunning):
		return "Starting"
	case inst.Spec.Stopped && is(v1alpha1.ConditionSupervisorReady):
		return "Stopped"
	case is(v1alpha1.ConditionInstalled):
		return "Installed"
	case is(v1alpha1.ConditionSupervisorReady):
		return "Installing"
	default:
		return "Pending"
	}
}

func trimErr(err error) string {
	s := err.Error()
	if len(s) > 512 {
		return s[:512] + "…"
	}
	return s
}

var _ = types.NamespacedName{}
