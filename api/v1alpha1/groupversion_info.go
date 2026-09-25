// Package v1alpha1 contains the minecraft.bedini.au/v1alpha1 API group.
//
// +kubebuilder:object:generate=true
// +groupName=minecraft.bedini.au
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is the group and version of this API.
	GroupVersion = schema.GroupVersion{Group: "minecraft.bedini.au", Version: "v1alpha1"}

	// SchemeBuilder registers the types with a scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds the types to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &MinecraftInstance{}, &MinecraftInstanceList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}
