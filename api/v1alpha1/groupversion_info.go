// Package v1alpha1 contains the minecraft.bedini.au/v1alpha1 API group.
//
// +kubebuilder:object:generate=true
// +groupName=minecraft.bedini.au
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the group and version of this API.
	GroupVersion = schema.GroupVersion{Group: "minecraft.bedini.au", Version: "v1alpha1"}

	// SchemeBuilder registers the types with a scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
