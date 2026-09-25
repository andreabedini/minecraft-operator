package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Condition types reported in MinecraftInstanceStatus.Conditions.
const (
	// ConditionSupervisorReady means the supervisor's gRPC API is reachable.
	ConditionSupervisorReady = "SupervisorReady"
	// ConditionInstalled means the files on the PVC match the resolved spec.
	ConditionInstalled = "Installed"
	// ConditionRunning means the supervisor reports the Java process alive.
	ConditionRunning = "Running"
	// ConditionReady means the management protocol reports the server started.
	ConditionReady = "Ready"
	// ConditionConfigDrift means staged config writes await a restart.
	ConditionConfigDrift = "ConfigDrift"
	// ConditionUpgradeBlocked means a version change was refused.
	ConditionUpgradeBlocked = "UpgradeBlocked"
)

// FlavourSpec selects the server software. Exactly one field must be set.
// +kubebuilder:validation:XValidation:rule="[has(self.vanilla), has(self.fabric), has(self.paper), has(self.forge)].filter(x, x).size() == 1",message="exactly one flavour must be set"
type FlavourSpec struct {
	// Vanilla runs the Mojang server jar.
	// +optional
	Vanilla *VanillaFlavour `json:"vanilla,omitempty"`
	// Fabric runs the Fabric launcher jar.
	// +optional
	Fabric *FabricFlavour `json:"fabric,omitempty"`
	// Paper runs a PaperMC build.
	// +optional
	Paper *PaperFlavour `json:"paper,omitempty"`
	// Forge runs a MinecraftForge build (1.17 and later).
	// +optional
	Forge *ForgeFlavour `json:"forge,omitempty"`
}

// VanillaFlavour has no options.
type VanillaFlavour struct{}

// FabricFlavour pins the loader and installer. Unset values resolve to the
// latest stable ones and are recorded in status.
type FabricFlavour struct {
	// +optional
	LoaderVersion string `json:"loaderVersion,omitempty"`
	// +optional
	InstallerVersion string `json:"installerVersion,omitempty"`
}

// PaperFlavour pins a build. Unset resolves to the newest build in the
// requested channel or better and is recorded in status.
type PaperFlavour struct {
	// +optional
	Build *int64 `json:"build,omitempty"`
	// Channel is the least mature channel to accept when Build is unset.
	// +kubebuilder:validation:Enum=stable;beta;alpha
	// +kubebuilder:default=stable
	// +optional
	Channel string `json:"channel,omitempty"`
}

// ForgeFlavour pins a full build string such as "1.20.1-47.2.0". Unset
// resolves to the recommended build.
type ForgeFlavour struct {
	// +optional
	Build string `json:"build,omitempty"`
}

// JavaSpec selects the JRE image.
type JavaSpec struct {
	// Image is a full image reference. When empty the operator derives one
	// from the resolved Java major with its image template.
	// +optional
	Image string `json:"image,omitempty"`
}

// JVMSpec configures the Java process.
type JVMSpec struct {
	// +kubebuilder:default=1024
	// +optional
	MinMemoryMiB int32 `json:"minMemoryMiB,omitempty"`
	// +kubebuilder:default=2048
	// +optional
	MaxMemoryMiB int32 `json:"maxMemoryMiB,omitempty"`
	// ExtraArgs are appended verbatim before the launch target.
	// +optional
	ExtraArgs []string `json:"extraArgs,omitempty"`
	// Env is added to the process environment.
	// +optional
	Env map[string]string `json:"env,omitempty"`
}

// DigestSpec is an expected file digest.
type DigestSpec struct {
	// +kubebuilder:validation:Enum=sha1;sha256;sha512
	Algorithm string `json:"algorithm"`
	// Value is lower-case hex.
	Value string `json:"value"`
}

// ModrinthSource identifies a Modrinth version.
type ModrinthSource struct {
	// Project is the Modrinth project id or slug.
	Project string `json:"project"`
	// Version is the Modrinth version id or version number.
	Version string `json:"version"`
}

// ModSpec is one jar in the mods directory.
// +kubebuilder:validation:XValidation:rule="has(self.modrinth) != has(self.url)",message="set exactly one of modrinth or url"
type ModSpec struct {
	// Name identifies the mod in status and events.
	Name string `json:"name"`
	// +optional
	Modrinth *ModrinthSource `json:"modrinth,omitempty"`
	// URL downloads the jar directly. Digest is required with URL.
	// +optional
	URL string `json:"url,omitempty"`
	// +optional
	Digest *DigestSpec `json:"digest,omitempty"`
}

// MergeMode says how a config file is written.
// +kubebuilder:validation:Enum=replace;properties
type MergeMode string

const (
	// MergeReplace writes the file as given.
	MergeReplace MergeMode = "replace"
	// MergeProperties merges keys into an existing Java properties file.
	MergeProperties MergeMode = "properties"
)

// ConfigFileSpec is a file on the PVC sourced from a ConfigMap or Secret.
// +kubebuilder:validation:XValidation:rule="has(self.configMapKeyRef) != has(self.secretKeyRef)",message="set exactly one of configMapKeyRef or secretKeyRef"
type ConfigFileSpec struct {
	// Path is relative to the instance directory.
	Path string `json:"path"`
	// +optional
	ConfigMapKeyRef *corev1.ConfigMapKeySelector `json:"configMapKeyRef,omitempty"`
	// +optional
	SecretKeyRef *corev1.SecretKeySelector `json:"secretKeyRef,omitempty"`
	// +kubebuilder:default=replace
	// +optional
	Merge MergeMode `json:"merge,omitempty"`
}

// StorageSpec describes the instance PVC.
// +kubebuilder:validation:XValidation:rule="!(has(self.existingClaim) && has(self.size))",message="size applies only to an operator-created claim"
type StorageSpec struct {
	// Size of the claim the operator creates.
	// +optional
	Size *resource.Quantity `json:"size,omitempty"`
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`
	// ExistingClaim mounts a claim the operator does not own.
	// +optional
	ExistingClaim string `json:"existingClaim,omitempty"`
	// SubPath inside the claim that holds the instance directory.
	// +optional
	SubPath string `json:"subPath,omitempty"`
	// RetainOnDelete keeps the operator-created claim when the instance is
	// deleted.
	// +kubebuilder:default=true
	// +optional
	RetainOnDelete *bool `json:"retainOnDelete,omitempty"`
}

// GameServiceSpec configures the player-facing Service.
type GameServiceSpec struct {
	// +kubebuilder:default=LoadBalancer
	// +optional
	Type corev1.ServiceType `json:"type,omitempty"`
	// +kubebuilder:default=25565
	// +optional
	Port int32 `json:"port,omitempty"`
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
	// +optional
	Labels map[string]string `json:"labels,omitempty"`
}

// ExtraPort is an additional port on the game Service (voice chat,
// Bedrock).
type ExtraPort struct {
	Name string `json:"name"`
	Port int32  `json:"port"`
	// +kubebuilder:default=TCP
	// +optional
	Protocol corev1.Protocol `json:"protocol,omitempty"`
}

// ServiceSpec configures the Services.
type ServiceSpec struct {
	// +optional
	Game GameServiceSpec `json:"game,omitempty"`
	// +optional
	ExtraPorts []ExtraPort `json:"extraPorts,omitempty"`
}

// RestartPolicy says what happens on config drift.
// +kubebuilder:validation:Enum=manual;automatic
type RestartPolicy string

const (
	// RestartManual leaves staged changes for a human to apply.
	RestartManual RestartPolicy = "manual"
	// RestartAutomatic restarts the server when staged changes appear.
	RestartAutomatic RestartPolicy = "automatic"
)

// UpgradeSpec guards version changes.
type UpgradeSpec struct {
	// +kubebuilder:default=true
	// +optional
	BackupBeforeUpgrade *bool `json:"backupBeforeUpgrade,omitempty"`
	// +optional
	AllowDowngrade bool `json:"allowDowngrade,omitempty"`
	// Force proceeds despite mod incompatibility.
	// +optional
	Force bool `json:"force,omitempty"`
}

// EndpointSpec is a metrics endpoint served from inside the server.
type EndpointSpec struct {
	Port int32 `json:"port"`
	// +kubebuilder:default=/metrics
	// +optional
	Path string `json:"path,omitempty"`
}

// MetricsSpec configures scraping.
type MetricsSpec struct {
	// StatsExporter runs the world-stats exporter as a separate pod.
	// +optional
	StatsExporter *bool `json:"statsExporter,omitempty"`
	// ModEndpoint is scraped when set.
	// +optional
	ModEndpoint *EndpointSpec `json:"modEndpoint,omitempty"`
}

// SupervisorSpec pins the supervisor.
type SupervisorSpec struct {
	// Image overrides the operator's default supervisor image.
	// +optional
	Image string `json:"image,omitempty"`
}

// MinecraftInstanceSpec is the desired state of a server.
type MinecraftInstanceSpec struct {
	// Version is the Minecraft version, e.g. "26.3".
	// +kubebuilder:validation:MinLength=1
	Version string      `json:"version"`
	Flavour FlavourSpec `json:"flavour"`
	// +optional
	Java JavaSpec `json:"java,omitempty"`
	// +optional
	JVM JVMSpec `json:"jvm,omitempty"`
	// +optional
	Mods []ModSpec `json:"mods,omitempty"`
	// +optional
	ConfigFiles []ConfigFileSpec `json:"configFiles,omitempty"`
	// +optional
	Storage StorageSpec `json:"storage,omitempty"`
	// +optional
	Service ServiceSpec `json:"service,omitempty"`
	// Autostart makes the supervisor relaunch the server on boot.
	// +kubebuilder:default=true
	// +optional
	Autostart *bool `json:"autostart,omitempty"`
	// Stopped keeps the server process stopped while the pod stays up, for
	// maintenance. The operator stops a running server and does not start
	// it until this is cleared.
	// +optional
	Stopped bool `json:"stopped,omitempty"`
	// +kubebuilder:default=manual
	// +optional
	RestartPolicy RestartPolicy `json:"restartPolicy,omitempty"`
	// +optional
	Upgrade UpgradeSpec `json:"upgrade,omitempty"`
	// +optional
	Metrics MetricsSpec `json:"metrics,omitempty"`
	// +optional
	Supervisor SupervisorSpec `json:"supervisor,omitempty"`
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
	// PodOverrides is a strategic merge patch applied to the generated pod
	// template (sidecars, sysctls, volumes, securityContext).
	// +kubebuilder:pruning:PreserveUnknownFields
	// +optional
	PodOverrides *runtime.RawExtension `json:"podOverrides,omitempty"`
}

// ResolvedFile is a file the operator installed.
type ResolvedFile struct {
	Path   string `json:"path"`
	Digest string `json:"digest,omitempty"`
}

// ResolvedMod is one installed mod.
type ResolvedMod struct {
	Name   string `json:"name"`
	File   string `json:"file"`
	Digest string `json:"digest,omitempty"`
}

// ResolvedStatus records what the spec resolved to.
type ResolvedStatus struct {
	Version   string `json:"version,omitempty"`
	JavaMajor int32  `json:"javaMajor,omitempty"`
	JavaImage string `json:"javaImage,omitempty"`
	// +optional
	LoaderVersion string `json:"loaderVersion,omitempty"`
	// +optional
	InstallerVersion string `json:"installerVersion,omitempty"`
	// +optional
	PaperBuild *int64 `json:"paperBuild,omitempty"`
	// +optional
	ForgeBuild string `json:"forgeBuild,omitempty"`
	// +optional
	ServerJar *ResolvedFile `json:"serverJar,omitempty"`
	// +optional
	Mods []ResolvedMod `json:"mods,omitempty"`
	// LaunchSpecHash is the supervisor's hash of the launch spec in effect.
	// +optional
	LaunchSpecHash string `json:"launchSpecHash,omitempty"`
}

// PlayersStatus is the current player count.
type PlayersStatus struct {
	Online int32 `json:"online"`
	Max    int32 `json:"max,omitempty"`
}

// UpgradeStatus follows a world upgrade.
type UpgradeStatus struct {
	Phase string `json:"phase,omitempty"`
	// Progress is 0 to 100.
	Progress int32 `json:"progress,omitempty"`
}

// MinecraftInstanceStatus is the observed state.
type MinecraftInstanceStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// Phase is a one-word summary for kubectl.
	// +optional
	Phase string `json:"phase,omitempty"`
	// +optional
	Resolved *ResolvedStatus `json:"resolved,omitempty"`
	// +optional
	SupervisorVersion string `json:"supervisorVersion,omitempty"`
	// +optional
	ManagementProtocolVersion string `json:"managementProtocolVersion,omitempty"`
	// +optional
	Players *PlayersStatus `json:"players,omitempty"`
	// +optional
	LastSave *metav1.Time `json:"lastSave,omitempty"`
	// +optional
	Upgrade *UpgradeStatus `json:"upgrade,omitempty"`
}

// MinecraftInstance is one Minecraft server.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=mci
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.version`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Players",type=integer,JSONPath=`.status.players.online`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type MinecraftInstance struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MinecraftInstanceSpec   `json:"spec,omitempty"`
	Status MinecraftInstanceStatus `json:"status,omitempty"`
}

// MinecraftInstanceList is a list of MinecraftInstance.
//
// +kubebuilder:object:root=true
type MinecraftInstanceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MinecraftInstance `json:"items"`
}

// FlavourName returns the flavour as a string.
func (s *FlavourSpec) FlavourName() string {
	switch {
	case s.Fabric != nil:
		return "fabric"
	case s.Paper != nil:
		return "paper"
	case s.Forge != nil:
		return "forge"
	default:
		return "vanilla"
	}
}
