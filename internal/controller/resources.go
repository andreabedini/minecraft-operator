package controller

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/utils/ptr"

	"github.com/andreabedini/minecraft-operator/api/v1alpha1"
	"github.com/andreabedini/minecraft-operator/internal/plan"
)

// Names and paths shared by the resource builders and the reconciler.
const (
	labelInstance = "minecraft.bedini.au/instance"
	labelName     = "app.kubernetes.io/name"
	labelValue    = "minecraft"

	dataMountPath      = "/data"
	supervisorBinDir   = "/opt/supervisor"
	supervisorBinPath  = supervisorBinDir + "/supervisor"
	supervisorSecrets  = "/etc/supervisor"
	supervisorPort     = 9800
	supervisorPortName = "supervisor"
	gamePortName       = "game"

	secretKeyToken         = "token"
	secretKeyReadOnlyToken = "readonly-token"
	secretKeyManagement    = "management-secret"

	runAsUser              = int64(1000)
	terminationGracePeriod = int64(180)
)

func instanceLabels(inst *v1alpha1.MinecraftInstance) map[string]string {
	return map[string]string{
		labelName:                     labelValue,
		"app.kubernetes.io/instance":  inst.Name,
		"app.kubernetes.io/component": "server",
		labelInstance:                 inst.Name,
	}
}

func selectorLabels(inst *v1alpha1.MinecraftInstance) map[string]string {
	return map[string]string{labelName: labelValue, labelInstance: inst.Name}
}

func secretName(inst *v1alpha1.MinecraftInstance) string     { return inst.Name + "-supervisor" }
func pvcName(inst *v1alpha1.MinecraftInstance) string        { return inst.Name + "-data" }
func deploymentName(inst *v1alpha1.MinecraftInstance) string { return inst.Name }
func gameServiceName(inst *v1alpha1.MinecraftInstance) string {
	return inst.Name + "-game"
}
func supervisorServiceName(inst *v1alpha1.MinecraftInstance) string {
	return inst.Name + "-supervisor"
}

func gamePort(inst *v1alpha1.MinecraftInstance) int32 {
	if inst.Spec.Service.Game.Port > 0 {
		return inst.Spec.Service.Game.Port
	}
	return 25565
}

// randomHex returns n random bytes as hex.
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// randomAlphanumeric returns a string of n characters from [A-Za-z0-9], the
// alphabet the management protocol expects for its secret.
func randomAlphanumeric(n int) (string, error) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	out := make([]byte, n)
	for i := range out {
		idx, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			return "", err
		}
		out[i] = alphabet[idx.Int64()]
	}
	return string(out), nil
}

// fillSecret adds any missing keys to the instance Secret.
func fillSecret(secret *corev1.Secret) (changed bool, err error) {
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	if len(secret.Data[secretKeyToken]) == 0 {
		v, err := randomHex(32)
		if err != nil {
			return false, err
		}
		secret.Data[secretKeyToken] = []byte(v)
		changed = true
	}
	if len(secret.Data[secretKeyReadOnlyToken]) == 0 {
		v, err := randomHex(32)
		if err != nil {
			return false, err
		}
		secret.Data[secretKeyReadOnlyToken] = []byte(v)
		changed = true
	}
	if len(secret.Data[secretKeyManagement]) == 0 {
		v, err := randomAlphanumeric(40)
		if err != nil {
			return false, err
		}
		secret.Data[secretKeyManagement] = []byte(v)
		changed = true
	}
	return changed, nil
}

// buildPVC returns the operator-created claim.
func buildPVC(inst *v1alpha1.MinecraftInstance) *corev1.PersistentVolumeClaim {
	size := resource.MustParse("20Gi")
	if inst.Spec.Storage.Size != nil {
		size = *inst.Spec.Storage.Size
	}
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: pvcName(inst), Namespace: inst.Namespace, Labels: instanceLabels(inst)},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: inst.Spec.Storage.StorageClassName,
			Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: size}},
		},
	}
}

// claimName is the PVC the pod mounts.
func claimName(inst *v1alpha1.MinecraftInstance) string {
	if inst.Spec.Storage.ExistingClaim != "" {
		return inst.Spec.Storage.ExistingClaim
	}
	return pvcName(inst)
}

// buildGameService returns the player-facing Service.
func buildGameService(inst *v1alpha1.MinecraftInstance) *corev1.Service {
	labels := instanceLabels(inst)
	for k, v := range inst.Spec.Service.Game.Labels {
		labels[k] = v
	}
	svcType := inst.Spec.Service.Game.Type
	if svcType == "" {
		svcType = corev1.ServiceTypeLoadBalancer
	}
	ports := []corev1.ServicePort{{
		Name:       gamePortName,
		Port:       gamePort(inst),
		TargetPort: intstr.FromString(gamePortName),
		Protocol:   corev1.ProtocolTCP,
	}}
	for _, p := range inst.Spec.Service.ExtraPorts {
		proto := p.Protocol
		if proto == "" {
			proto = corev1.ProtocolTCP
		}
		ports = append(ports, corev1.ServicePort{Name: p.Name, Port: p.Port, TargetPort: intstr.FromString(p.Name), Protocol: proto})
	}
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        gameServiceName(inst),
			Namespace:   inst.Namespace,
			Labels:      labels,
			Annotations: inst.Spec.Service.Game.Annotations,
		},
		Spec: corev1.ServiceSpec{
			Type:     svcType,
			Selector: selectorLabels(inst),
			Ports:    ports,
		},
	}
}

// buildSupervisorService returns the ClusterIP Service for the gRPC port. It
// publishes not-ready addresses because readiness follows the game port.
func buildSupervisorService(inst *v1alpha1.MinecraftInstance) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: supervisorServiceName(inst), Namespace: inst.Namespace, Labels: instanceLabels(inst)},
		Spec: corev1.ServiceSpec{
			Type:                     corev1.ServiceTypeClusterIP,
			Selector:                 selectorLabels(inst),
			PublishNotReadyAddresses: true,
			Ports: []corev1.ServicePort{{
				Name:       supervisorPortName,
				Port:       supervisorPort,
				TargetPort: intstr.FromString(supervisorPortName),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
}

// DeploymentInput is what the Deployment builder needs beyond the instance.
type DeploymentInput struct {
	JavaImage       string
	SupervisorImage string
	// InsecureDownloads adds -allow-insecure-downloads to the supervisor.
	InsecureDownloads bool
}

// buildPodTemplate returns the pod template before overrides.
func buildPodTemplate(inst *v1alpha1.MinecraftInstance, in DeploymentInput) corev1.PodTemplateSpec {
	ports := []corev1.ContainerPort{
		{Name: gamePortName, ContainerPort: gamePort(inst), Protocol: corev1.ProtocolTCP},
		{Name: supervisorPortName, ContainerPort: supervisorPort, Protocol: corev1.ProtocolTCP},
	}
	for _, p := range inst.Spec.Service.ExtraPorts {
		proto := p.Protocol
		if proto == "" {
			proto = corev1.ProtocolTCP
		}
		ports = append(ports, corev1.ContainerPort{Name: p.Name, ContainerPort: p.Port, Protocol: proto})
	}
	dataMount := corev1.VolumeMount{Name: "data", MountPath: dataMountPath, SubPath: inst.Spec.Storage.SubPath}
	supervisorArgs := []string{
		"-data-root", dataMountPath,
		"-listen", fmt.Sprintf(":%d", supervisorPort),
		"-token-file", supervisorSecrets + "/" + secretKeyToken,
		"-readonly-token-file", supervisorSecrets + "/" + secretKeyReadOnlyToken,
	}
	if in.InsecureDownloads {
		supervisorArgs = append(supervisorArgs, "-allow-insecure-downloads")
	}
	restricted := &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr.To(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: instanceLabels(inst)},
		Spec: corev1.PodSpec{
			TerminationGracePeriodSeconds: ptr.To(terminationGracePeriod),
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   ptr.To(true),
				RunAsUser:      ptr.To(runAsUser),
				RunAsGroup:     ptr.To(runAsUser),
				FSGroup:        ptr.To(runAsUser),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			InitContainers: []corev1.Container{{
				Name:            "supervisor",
				Image:           in.SupervisorImage,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Args:            []string{"copy-self", supervisorBinPath},
				SecurityContext: restricted,
				VolumeMounts:    []corev1.VolumeMount{{Name: "supervisor-bin", MountPath: supervisorBinDir}},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("16Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")},
				},
			}},
			Containers: []corev1.Container{{
				Name:    "server",
				Image:   in.JavaImage,
				Command: []string{supervisorBinPath},
				Args:    supervisorArgs,
				Env: []corev1.EnvVar{
					{Name: "HOME", Value: dataMountPath},
					{Name: "SUPERVISOR_DATA_ROOT", Value: dataMountPath},
				},
				Ports:           ports,
				SecurityContext: restricted,
				Resources:       inst.Spec.Resources,
				VolumeMounts: []corev1.VolumeMount{
					dataMount,
					{Name: "supervisor-bin", MountPath: supervisorBinDir, ReadOnly: true},
					{Name: "supervisor-secrets", MountPath: supervisorSecrets, ReadOnly: true},
				},
				// The supervisor answers as soon as it is up; the game port
				// decides readiness so a stopped server leaves the Service.
				StartupProbe: &corev1.Probe{
					ProbeHandler:     corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromString(supervisorPortName)}},
					PeriodSeconds:    2,
					FailureThreshold: 30,
				},
				LivenessProbe: &corev1.Probe{
					ProbeHandler:     corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromString(supervisorPortName)}},
					PeriodSeconds:    30,
					TimeoutSeconds:   3,
					FailureThreshold: 5,
				},
				ReadinessProbe: &corev1.Probe{
					ProbeHandler:     corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromString(gamePortName)}},
					PeriodSeconds:    10,
					TimeoutSeconds:   2,
					FailureThreshold: 3,
				},
			}},
			Volumes: []corev1.Volume{
				{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claimName(inst)}}},
				{Name: "supervisor-bin", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				{Name: "supervisor-secrets", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: secretName(inst), DefaultMode: ptr.To(int32(0o440))}}},
			},
		},
	}
}

// applyPodOverrides applies the spec's strategic merge patch to the template.
func applyPodOverrides(tmpl corev1.PodTemplateSpec, inst *v1alpha1.MinecraftInstance) (corev1.PodTemplateSpec, error) {
	if inst.Spec.PodOverrides == nil || len(inst.Spec.PodOverrides.Raw) == 0 {
		return tmpl, nil
	}
	original, err := json.Marshal(tmpl)
	if err != nil {
		return tmpl, err
	}
	patched, err := strategicpatch.StrategicMergePatch(original, inst.Spec.PodOverrides.Raw, corev1.PodTemplateSpec{})
	if err != nil {
		return tmpl, fmt.Errorf("podOverrides: %w", err)
	}
	var out corev1.PodTemplateSpec
	if err := json.Unmarshal(patched, &out); err != nil {
		return tmpl, fmt.Errorf("podOverrides: %w", err)
	}
	// Labels must keep the selector intact.
	if out.Labels == nil {
		out.Labels = map[string]string{}
	}
	for k, v := range selectorLabels(inst) {
		out.Labels[k] = v
	}
	return out, nil
}

// buildDeployment returns the server Deployment.
func buildDeployment(inst *v1alpha1.MinecraftInstance, in DeploymentInput) (*appsv1.Deployment, error) {
	tmpl, err := applyPodOverrides(buildPodTemplate(inst, in), inst)
	if err != nil {
		return nil, err
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: deploymentName(inst), Namespace: inst.Namespace, Labels: instanceLabels(inst)},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(1)),
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Selector: &metav1.LabelSelector{MatchLabels: selectorLabels(inst)},
			Template: tmpl,
		},
	}, nil
}

// supervisorURL is the base URL for a pod's supervisor.
func supervisorURL(podIP string) string {
	return fmt.Sprintf("http://%s", joinHostPort(podIP, supervisorPort))
}

func joinHostPort(host string, port int) string {
	for _, c := range host {
		if c == ':' {
			return fmt.Sprintf("[%s]:%d", host, port)
		}
	}
	return fmt.Sprintf("%s:%d", host, port)
}

// managementPort is re-exported for status and tests.
var _ = plan.ManagementPort
