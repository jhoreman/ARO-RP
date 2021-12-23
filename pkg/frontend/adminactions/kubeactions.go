package adminactions

// Copyright (c) Microsoft Corporation.
// Licensed under the Apache License 2.0.

import (
	"context"
	"net/http"
	"time"

	configclient "github.com/openshift/client-go/config/clientset/versioned"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/kubectl/pkg/drain"

	"github.com/Azure/ARO-RP/pkg/api"
	"github.com/Azure/ARO-RP/pkg/env"
	"github.com/Azure/ARO-RP/pkg/util/dynamichelper"
	"github.com/Azure/ARO-RP/pkg/util/restconfig"
)

// KubeActions are those that involve k8s objects, and thus depend upon k8s clients being createable
type KubeActions interface {
	KubeGet(ctx context.Context, groupKind, namespace, name string) ([]byte, error)
	KubeList(ctx context.Context, groupKind, namespace string) ([]byte, error)
	KubeCreateOrUpdate(ctx context.Context, obj *unstructured.Unstructured) error
	KubeDelete(ctx context.Context, groupKind, namespace, name string) error
	KubeDrain(ctx context.Context, name string) error
	Upgrade(ctx context.Context, upgradeY bool) error
}

type kubeActions struct {
	log *logrus.Entry
	oc  *api.OpenShiftCluster

	gvrResolver dynamichelper.GVRResolver

	kubernetescli kubernetes.Interface
	dyn           dynamic.Interface
	configcli     configclient.Interface
}

// NewKubeActions returns a kubeActions
func NewKubeActions(log *logrus.Entry, env env.Interface, oc *api.OpenShiftCluster) (KubeActions, error) {
	restConfig, err := restconfig.RestConfig(env, oc)
	if err != nil {
		return nil, err
	}

	gvrResolver, err := dynamichelper.NewGVRResolver(log, restConfig)
	if err != nil {
		return nil, err
	}

	dyn, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, err
	}

	kubernetescli, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, err
	}

	configcli, err := configclient.NewForConfig(restConfig)
	if err != nil {
		return nil, err
	}

	return &kubeActions{
		log: log,
		oc:  oc,

		gvrResolver: gvrResolver,

		kubernetescli: kubernetescli,
		dyn:           dyn,
		configcli:     configcli,
	}, nil
}

func (k *kubeActions) KubeGet(ctx context.Context, groupKind, namespace, name string) ([]byte, error) {
	gvr, err := k.gvrResolver.Resolve(groupKind, "")
	if err != nil {
		return nil, err
	}

	un, err := k.dyn.Resource(*gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	return un.MarshalJSON()
}

func (k *kubeActions) KubeList(ctx context.Context, groupKind, namespace string) ([]byte, error) {
	gvr, err := k.gvrResolver.Resolve(groupKind, "")
	if err != nil {
		return nil, err
	}

	// protect RP memory by not reading in more than 1000 items
	ul, err := k.dyn.Resource(*gvr).Namespace(namespace).List(ctx, metav1.ListOptions{Limit: 1000})
	if err != nil {
		return nil, err
	}

	if ul.GetContinue() != "" {
		return nil, api.NewCloudError(
			http.StatusInternalServerError, api.CloudErrorCodeInternalServerError,
			groupKind, "Too many items returned.")
	}

	return ul.MarshalJSON()
}

func (k *kubeActions) KubeCreateOrUpdate(ctx context.Context, o *unstructured.Unstructured) error {
	gvr, err := k.gvrResolver.Resolve(o.GroupVersionKind().GroupKind().String(), o.GroupVersionKind().Version)
	if err != nil {
		return err
	}

	_, err = k.dyn.Resource(*gvr).Namespace(o.GetNamespace()).Update(ctx, o, metav1.UpdateOptions{})
	if !kerrors.IsNotFound(err) {
		return err
	}

	_, err = k.dyn.Resource(*gvr).Namespace(o.GetNamespace()).Create(ctx, o, metav1.CreateOptions{})
	return err
}

func (k *kubeActions) KubeDelete(ctx context.Context, groupKind, namespace, name string) error {
	gvr, err := k.gvrResolver.Resolve(groupKind, "")
	if err != nil {
		return err
	}

	return k.dyn.Resource(*gvr).Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{})
}

func (k *kubeActions) KubeDrain(ctx context.Context, name string) error {
	return drain.RunNodeDrain(&drain.Helper{
		Client:              k.kubernetescli,
		Force:               true,
		GracePeriodSeconds:  -1,
		IgnoreAllDaemonSets: true,
		Timeout:             2 * time.Minute,
		DeleteEmptyDirData:  true,
		DisableEviction:     true,
		OnPodDeletedOrEvicted: func(pod *corev1.Pod, usingEviction bool) {
			k.log.Printf("deleted pod %s/%s", pod.Namespace, pod.Name)
		},
		Out:    k.log.Writer(),
		ErrOut: k.log.Writer(),
	}, name)
}
