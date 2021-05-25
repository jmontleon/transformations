package transform

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/json"

	v1 "k8s.io/api/core/v1"

	jsonpatch "github.com/evanphx/json-patch"
	k8s "github.com/konveyor/transformations/pkg/kubernetes"
	"github.com/konveyor/transformations/pkg/openshift"
)

type TransformFile struct {
	FileInfo     os.FileInfo
	Path         string
	Unstructured unstructured.Unstructured
}

type TransformOptions struct {
	Annotations map[string]string
	// We should make this more generic in the future
	OldInternalRegistry string
	NewInternalRegistry string
	IsOpenshift         bool
	StartingPath        string
	OutputDirPath       string
}

var PodGK = schema.GroupKind{
	Group: "",
	Kind:  "Pod",
}

var SecretGK = schema.GroupKind{
	Group: "",
	Kind:  "Secret",
}

var ServiceAccountGK = schema.GroupKind{
	Group: "",
	Kind:  "ServiceAccount",
}

var RouteGK = schema.GroupKind{
	Group: "route.openshift.io",
	Kind:  "Route",
}

var EndpointGK = schema.GroupKind{
	Group: "",
	Kind:  "Endpoints",
}

var EndpointSliceGK = schema.GroupKind{
	Group: "discovery.k8s.io",
	Kind:  "EndpointSlice",
}

var PVCGK = schema.GroupKind{
	Group: "",
	Kind:  "PersistentVolumeClaim",
}

var AOS3RoleBindingGK = schema.GroupKind{
	Group: "rbac.authorization.k8s.io",
	Kind:  "RoleBinding",
}

var OCP4RoleBindingGK = schema.GroupKind{
	Group: "authorization.openshift.io",
	Kind:  "RoleBinding",
}

var ServiceGK = schema.GroupKind{
	Group: "",
	Kind:  "Service",
}

var LimitRangeGK = schema.GroupKind{
	Group: "",
	Kind:  "LimitRange",
}

const serviceOriginAnnotation = "service.alpha.openshift.io/originating-service-name"
const routeHostGenerated = "openshift.io/host.generated"

func OutputTransforms(files []TransformFile, transformOptions TransformOptions) error {
	for _, file := range files {
		fmt.Printf("\n")

		u := file.Unstructured

		//If OwnerRef remove the file by creating white out file.
		if len(u.GetOwnerReferences()) > 0 {
			fmt.Printf("\nCreate Whiteout File %v -- %v", u.GroupVersionKind(), u.GetName())
			createWhiteOutFile(file, transformOptions)

			continue

		}

		if transformOptions.IsOpenshift && openshiftWhiteOuts(file, u) {
			fmt.Printf("\nCreate Whiteout File %v -- %v", u.GroupVersionKind(), u.GetName())
			createWhiteOutFile(file, transformOptions)
			continue
		}

		if kubernetesWhiteOuts(file, u) {
			fmt.Printf("\nCreate Whiteout File %v -- %v", u.GroupVersionKind(), u.GetName())
			createWhiteOutFile(file, transformOptions)
			continue
		}

		fmt.Printf("\n%v", u.GetName())

		// Always Apply common add anootations
		jsonPatch := k8s.AddAnnotation(transformOptions.Annotations)

		// Special Case for pods to remove NodeName
		if u.GroupVersionKind().GroupKind() == PodGK {
			transformPod(file, transformOptions, u)
		}

		//Clearn this if block up eventually.
		if template, ok := isPodSpecable(u); ok {
			for i, container := range template.Spec.Containers {
				image := strings.Replace(container.Image, transformOptions.OldInternalRegistry, transformOptions.NewInternalRegistry, 1)
				if image != container.Image {
					jp := k8s.UpdatePodSpecable(fmt.Sprintf("/spec/template/spec/containers/%v/image", i), image)
					jsonPatch = append(jsonPatch, jp...)
				}
			}
			for i, container := range template.Spec.InitContainers {
				image := strings.Replace(container.Image, transformOptions.OldInternalRegistry, transformOptions.NewInternalRegistry, 1)
				if image != container.Image {
					jp := k8s.UpdatePodSpecable(fmt.Sprintf("/spec/template/spec/initContainers/%v/image", i), image)
					jsonPatch = append(jsonPatch, jp...)
				}
			}
		}

		// If Openshift create openshift specific transformations
		if transformOptions.IsOpenshift {
			jps := openshiftPatches(file, u)
			jsonPatch = append(jsonPatch, jps...)
		}
		if u.GroupVersionKind().GroupKind() == ServiceGK {
			jps := k8s.RemoveServiceClusterIPs()
			jsonPatch = append(jsonPatch, jps...)
		}

		fmt.Printf("---%#v", jsonPatch)

		createTransformFile(file, transformOptions, jsonPatch)
	}
	return nil
}

func transformPod(file TransformFile, transformOptions TransformOptions, u unstructured.Unstructured) jsonpatch.Patch {
	jp := k8s.RemovePodSelectedNode()
	if transformOptions.IsOpenshift {
		// handle pull secrets
		patches := openshift.UpdateDefaultPullSecrets(u)

		jp = append(jp, patches...)

	}
	return jp
}

func isPodSpecable(u unstructured.Unstructured) (*v1.PodTemplateSpec, bool) {
	// Get Spec
	spec, ok := u.UnstructuredContent()["spec"]
	if !ok {
		return nil, false
	}

	specMap, ok := spec.(map[string]interface{})
	if !ok {
		return nil, false
	}

	// Is template apart of the spec
	templateInterface, ok := specMap["template"]
	if !ok {
		return nil, false
	}

	// does template marshal into PodTemplateSpec

	jsonTemplate, err := json.Marshal(templateInterface)
	if err != nil {
		return nil, false
	}

	template := v1.PodTemplateSpec{}

	err = json.Unmarshal(jsonTemplate, &template)
	if err != nil {
		return nil, false
	}

	return &template, true
}

func openshiftWhiteOuts(file TransformFile, u unstructured.Unstructured) bool {
	_, ok := u.GetAnnotations()[serviceOriginAnnotation]
	// secrets if ownded by originating-service-name by annoation
	if ok && u.GetObjectKind().GroupVersionKind().GroupKind() == SecretGK {
		return true
	}
	// Assume if from/to openshift that the admin rolebinding is already created for the user.
	if u.GetName() == "system:image-builders" ||
		u.GetName() == "system:image-pullers" ||
		u.GetName() == "system:deployers" {
		if u.GetObjectKind().GroupVersionKind().GroupKind() == AOS3RoleBindingGK ||
			u.GetObjectKind().GroupVersionKind().GroupKind() == OCP4RoleBindingGK {
			return true
		}
	}

	if u.GetObjectKind().GroupVersionKind().GroupKind() == ServiceAccountGK {
		for _, suffix := range []string{"deployer", "default", "builder"} {
			if strings.HasSuffix(u.GetName(), suffix) {
				return true
			}
		}
	}

	if u.GetObjectKind().GroupVersionKind().GroupKind() == SecretGK {
		for _, prefix := range []string{
			"deployer-dockercfg",
			"deployer-token",
			"default-dockercfg",
			"default-token",
			"builder-dockercfg",
			"builder-token"} {
			if strings.HasPrefix(u.GetName(), prefix) {
				return true
			}
		}
	}

	if u.GetObjectKind().GroupVersionKind().GroupKind() == LimitRangeGK {
		return true
	}

	return false
}

func kubernetesWhiteOuts(file TransformFile, u unstructured.Unstructured) bool {
	// Endpoints are cluster specfici and usually managed by the service.
	// In some rare cases a user may create this but they would have to update it
	// with the new address spaces.
	if u.GetObjectKind().GroupVersionKind().GroupKind() == EndpointGK {
		return true
	}
	// EndpointSlices are the next iteration of endpoints and should also be removed.
	if u.GetObjectKind().GroupVersionKind().GroupKind() == EndpointSliceGK {
		return true
	}

	// Assume PVC's are transfered with storage transfer workflow.
	if u.GetObjectKind().GroupVersionKind().GroupKind() == PVCGK {
		return true
	}
	return false
}

func openshiftPatches(file TransformFile, u unstructured.Unstructured) jsonpatch.Patch {
	jps := jsonpatch.Patch{}
	// Handle Service Account secrets
	if u.GetObjectKind().GroupVersionKind().GroupKind() == ServiceAccountGK {
		jp := openshift.UpdateServiceAccount(u)
		jps = append(jps, jp...)
	}

	_, ok := u.GetAnnotations()[routeHostGenerated]

	if ok && u.GetObjectKind().GroupVersionKind().GroupKind() == RouteGK {
		jp := openshift.UpdateRoute(u)
		jps = append(jps, jp...)
	}

	return jps
}

func createWhiteOutFile(file TransformFile, opts TransformOptions) {
	fname, dir := GetWhiteOutFilePath(opts.OutputDirPath, opts.StartingPath, file.Path)
	err := os.MkdirAll(dir, 0777)
	if err != nil {
		fmt.Printf("%v", err)
		panic(err)
	}

	f, err := os.Create(fname)
	if err != nil {
		fmt.Printf("%v", err)
		panic(err)
	}
	if err = f.Close(); err != nil {
		fmt.Printf("%v", err)
		panic(err)
	}
}

func createTransformFile(file TransformFile, opts TransformOptions, jp jsonpatch.Patch) {
	fname, dir := GetTransformPath(opts.OutputDirPath, opts.StartingPath, file.Path)

	err := os.MkdirAll(dir, 0777)
	if err != nil {
		fmt.Printf("%v", err)
		panic(err)
	}

	data, err := json.Marshal(jp)
	if err != nil {
		fmt.Printf("%v", err)
		panic(err)
	}

	err = ioutil.WriteFile(fname, data, 0664)
	if err != nil {
		fmt.Printf("%v", err)
		panic(err)
	}
}

func GetTransformPath(transformDir, startingPath, filePath string) (string, string) {
	dir, fname := filepath.Split(filePath)

	dir = strings.Replace(dir, startingPath, transformDir, 1)

	fname = fmt.Sprintf("transforms-%v", fname)
	fname = filepath.Join(dir, fname)

	return fname, dir
}

func GetWhiteOutFilePath(transformDir, startingPath, filePath string) (string, string) {
	dir, fname := filepath.Split(filePath)
	dir = strings.Replace(dir, startingPath, transformDir, 1)
	fname = fmt.Sprintf(".wh.%v", fname)
	fname = filepath.Join(dir, fname)
	return fname, dir
}
