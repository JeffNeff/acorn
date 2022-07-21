package uninstall

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/acorn-io/acorn/pkg/install"
	"github.com/acorn-io/acorn/pkg/k8sclient"
	"github.com/acorn-io/acorn/pkg/labels"
	"github.com/acorn-io/acorn/pkg/prompt"
	"github.com/acorn-io/acorn/pkg/system"
	"github.com/acorn-io/acorn/pkg/term"
	"github.com/pterm/pterm"
	"github.com/rancher/wrangler/pkg/merr"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierror "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	klabels "k8s.io/apimachinery/pkg/labels"
	kclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
)

type Options struct {
	All   bool
	Force bool
}

func key(obj kclient.Object) string {
	if obj.GetNamespace() == "" {
		return obj.GetName()
	}
	return obj.GetNamespace() + "/" + obj.GetName()
}

func baseResources(ctx context.Context, c kclient.Client) (resources []kclient.Object, _ error) {
	all, err := install.AllResources()
	if err != nil {
		return nil, err
	}

	for _, all := range all {
		if all.GetObjectKind().GroupVersionKind().Kind == "Namespace" && all.GetName() == system.DefaultUserNamespace {
			continue
		}
		resources = append(resources, all)
	}

	nses := &corev1.NamespaceList{}
	err = c.List(ctx, nses, &kclient.ListOptions{
		LabelSelector: klabels.SelectorFromSet(map[string]string{
			labels.AcornManaged: "true",
		}),
	})
	if err != nil {
		return nil, err
	}

	for _, ns := range nses.Items {
		ns := ns
		resources = append(resources, &ns)
	}

	crds := &v1.CustomResourceDefinitionList{}
	err = c.List(ctx, crds)
	if err != nil {
		return nil, err
	}

	for _, crd := range crds.Items {
		if strings.HasSuffix(crd.Name, ".internal.acorn.io") {
			c := crd
			resources = append(resources, &c)
		}
	}

	for i, resource := range resources {
		gvk, err := apiutil.GVKForObject(resource, c.Scheme())
		if err == nil {
			resources[i].GetObjectKind().SetGroupVersionKind(gvk)
		}
	}

	return resources, nil
}

func sortToDelete(resources []kclient.Object) {
	sort.Slice(resources, func(i, j int) bool {
		lKind := resources[i].GetObjectKind().GroupVersionKind().Kind
		rKind := resources[j].GetObjectKind().GroupVersionKind().Kind
		if lKind == rKind {
			return key(resources[i]) < key(resources[j])
		}
		if lKind == "Namespace" {
			return false
		}
		if rKind == "Namespace" {
			return true
		}
		return lKind < rKind
	})
}

func userResources(ctx context.Context, c kclient.Client) (resources []kclient.Object, _ error) {
	resources = append(resources, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: system.DefaultUserNamespace,
		},
	})

	secrets := &corev1.SecretList{}
	err := c.List(ctx, secrets, &kclient.ListOptions{
		LabelSelector: klabels.SelectorFromSet(map[string]string{
			labels.AcornManaged: "true",
		}),
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(secrets.Items, func(i, j int) bool {
		return key(&secrets.Items[i]) < key(&secrets.Items[j])
	})

	for _, secret := range secrets.Items {
		secret := secret
		resources = append(resources, &secret)
	}

	pvs := &corev1.PersistentVolumeList{}
	err = c.List(ctx, pvs, &kclient.ListOptions{
		LabelSelector: klabels.SelectorFromSet(map[string]string{
			labels.AcornManaged: "true",
		}),
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(pvs.Items, func(i, j int) bool {
		return key(&pvs.Items[i]) < key(&pvs.Items[j])
	})

	for _, pv := range pvs.Items {
		pv := pv
		resources = append(resources, &pv)
	}

	for i, resource := range resources {
		gvk, err := apiutil.GVKForObject(resource, c.Scheme())
		if err == nil {
			resources[i].GetObjectKind().SetGroupVersionKind(gvk)
		}
	}

	return resources, nil
}

func Uninstall(ctx context.Context, opts *Options) error {
	if opts == nil {
		opts = &Options{}
	}

	c, err := k8sclient.Default()
	if err != nil {
		return nil
	}

	toDelete, err := baseResources(ctx, c)
	if err != nil {
		return err
	}

	toKeep, err := userResources(ctx, c)
	if err != nil {
		return nil
	}

	if opts.All {
		toDelete = append(toDelete, toKeep...)
		toKeep = nil
	}

	sortToDelete(toDelete)

	if !opts.Force {
		if ok, err := shouldContinue(toDelete, toKeep); err != nil {
			return err
		} else if !ok {
			pterm.Warning.Println("Aborting uninstall")
			return nil
		}
	}

	var errs []error
	for _, resource := range toDelete {
		apiVersion, kind := resource.GetObjectKind().GroupVersionKind().ToAPIVersionAndKind()
		pterm.Info.Printf("Deleting %s %s %s\n", key(resource), kind, apiVersion)
		if err := c.Delete(ctx, resource); err != nil && !apierror.IsNotFound(err) {
			errs = append(errs, err)
		}
	}

	if err := merr.NewErrors(errs...); err != nil {
		return err
	}

	for _, resource := range toDelete {
		gvk := resource.GetObjectKind().GroupVersionKind()
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvk)
		err := c.Get(ctx, kclient.ObjectKey{
			Namespace: resource.GetNamespace(),
			Name:      resource.GetName(),
		}, u)
		if apierror.IsNotFound(err) {
			continue
		} else if err != nil {
			errs = append(errs, err)
			continue
		}

		apiVersion, kind := gvk.ToAPIVersionAndKind()
		pb := term.NewSpinner(fmt.Sprintf("Waiting for %s %s %s to delete", key(resource), kind, apiVersion))
		for {
			err := c.Get(ctx, kclient.ObjectKey{
				Namespace: resource.GetNamespace(),
				Name:      resource.GetName(),
			}, u)
			if apierror.IsNotFound(err) {
				pb.Success()
				break
			} else if err != nil {
				_ = pb.Fail(err)
				errs = append(errs, err)
				break
			}
		}
	}

	pterm.Success.Println("Acorn uninstalled")
	return nil
}

func shouldContinue(toDelete, toKeep []kclient.Object) (bool, error) {
	var data = [][]string{
		{"Action", "Namespace", "Name", "Kind", "API Version"},
	}
	namespaces := map[string]bool{}
	for _, resource := range toDelete {
		apiVersion, kind := resource.GetObjectKind().GroupVersionKind().ToAPIVersionAndKind()
		if kind == "Namespace" {
			kind = pterm.Red(pterm.Bold.Sprint(kind))
			namespaces[resource.GetName()] = true
		}
		data = append(data, []string{
			pterm.Red("delete"),
			resource.GetNamespace(),
			pterm.Red(resource.GetName()),
			kind,
			apiVersion,
		})
	}
	for _, resource := range toKeep {
		apiVersion, kind := resource.GetObjectKind().GroupVersionKind().ToAPIVersionAndKind()
		if namespaces[resource.GetNamespace()] {
			data = append(data, []string{
				pterm.Red("delete"),
				resource.GetNamespace(),
				pterm.Red(resource.GetName()),
				kind,
				apiVersion,
			})
			continue
		}
		data = append(data, []string{
			pterm.Green("keep"),
			pterm.Gray(resource.GetNamespace()),
			pterm.Gray(resource.GetName()),
			pterm.Gray(kind),
			pterm.Gray(apiVersion),
		})
	}
	if err := pterm.DefaultTable.WithHasHeader().WithData(data).Render(); err != nil {
		return false, err
	}
	if len(toKeep) == 0 {
		return prompt.Bool("Do you want to delete the above resources?", false)
	}
	return prompt.Bool("Do you want to delete/keep the above resources? "+
		"To delete all resources pass run \"acorn uninstall --all\"", false)

}