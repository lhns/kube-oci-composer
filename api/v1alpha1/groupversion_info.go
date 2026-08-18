// Package v1alpha1 contains API Schema definitions for the kube-oci-composer v1alpha1 API group.
// +kubebuilder:object:generate=true
// +groupName=oci.lhns.de
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Built on apimachinery rather than controller-runtime's scheme.Builder, which is deprecated for
// precisely the reason that applies here: an api package should be cheap to import, and reaching
// for controller-runtime to register types makes every consumer of these types depend on the
// controller machinery as well.

var (
	// GroupVersion is the group/version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "oci.lhns.de", Version: "v1alpha1"}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion,
		&ImageComposition{}, &ImageCompositionList{},
		&DockerBuild{}, &DockerBuildList{},
	)
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}
