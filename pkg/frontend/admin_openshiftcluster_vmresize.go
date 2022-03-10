package frontend

// Copyright (c) Microsoft Corporation.
// Licensed under the Apache License 2.0.

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	kruntime "k8s.io/apimachinery/pkg/runtime"

	"github.com/Azure/ARO-RP/pkg/api"
	"github.com/Azure/ARO-RP/pkg/database/cosmosdb"
	"github.com/Azure/ARO-RP/pkg/frontend/middleware"
)

func (f *frontend) postAdminOpenShiftClusterVMResize(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	log := ctx.Value(middleware.ContextKeyLog).(*logrus.Entry)
	r.URL.Path = filepath.Dir(r.URL.Path)

	err := f._postAdminOpenShiftClusterVMResize(ctx, r, log)

	adminReply(log, w, nil, nil, err)
}

func (f *frontend) _postAdminOpenShiftClusterVMResize(ctx context.Context, r *http.Request, log *logrus.Entry) error {
	vars := mux.Vars(r)

	vmName := r.URL.Query().Get("vmName")
	err := validateAdminVMName(vmName)
	if err != nil {
		return err
	}

	resourceID := strings.TrimPrefix(r.URL.Path, "/admin")

	doc, err := f.dbOpenShiftClusters.Get(ctx, resourceID)
	switch {
	case cosmosdb.IsErrorStatusCode(err, http.StatusNotFound):
		return api.NewCloudError(http.StatusNotFound, api.CloudErrorCodeResourceNotFound, "",
			"The Resource '%s/%s' under resource group '%s' was not found.",
			vars["resourceType"], vars["resourceName"], vars["resourceGroupName"])
	case err != nil:
		return err
	}

	subscriptionDoc, err := f.getSubscriptionDocument(ctx, doc.Key)
	if err != nil {
		return err
	}

	vmSize := r.URL.Query().Get("vmSize")
	err = validateAdminVMSize(vmSize)
	if err != nil {
		return err
	}

	a, err := f.azureActionsFactory(log, f.env, doc.OpenShiftCluster, subscriptionDoc)
	if err != nil {
		return err
	}

	_, _ = a.VMSizeList(ctx)

	k, err := f.kubeActionsFactory(log, f.env, doc.OpenShiftCluster)
	if err != nil {
		return err
	}

	nodeList, err := k.KubeList(ctx, "node", "")
	if err != nil {
		return err
	}

	var u unstructured.Unstructured
	var nodes corev1.NodeList
	if err = json.Unmarshal(nodeList, &u); err != nil {
		return err
	}

	err = kruntime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &nodes)
	if err != nil {
		return err
	}

	var resizeNode *corev1.Node
	for _, node := range nodes.Items {
		if _, ok := node.ObjectMeta.Labels["node-role.kubernetes.io/master"]; !ok {
			continue
		}

		if strings.EqualFold(vmName, node.ObjectMeta.Name) {
			resizeNode = node.DeepCopy()
		}

	}

	if resizeNode == nil {
		return api.NewCloudError(http.StatusNotFound, api.CloudErrorCodeNotFound, "",
			"The master node '%s' under resource group '%s' was not found.",
			vmName, vars["resourceGroupName"])
	}

	resizeNode.Spec.Unschedulable = true
	resizeNode.Status = corev1.NodeStatus{}
	unstruct := &unstructured.Unstructured{}
	uMap, err := kruntime.DefaultUnstructuredConverter.ToUnstructured(resizeNode)
	if err != nil {
		return err
	}
	unstruct.Object = uMap

	err = k.KubeCreateOrUpdate(ctx, unstruct)
	if err != nil {
		return err
	}

	err = k.KubeDrain(ctx, vmName)
	if err != nil {
		return err
	}

	err = a.VMStopAndWait(ctx, vmName)
	if err != nil {
		return err
	}

	err = a.VMResize(ctx, vmName, vmSize)
	if err != nil {
		return err
	}

	err = a.VMStartAndWait(ctx, vmName)
	if err != nil {
		return err
	}

	bNode, err := k.KubeGet(ctx, "node", "", vmName)
	if err != nil {
		return err
	}

	var node corev1.Node
	if err = json.Unmarshal(bNode, &u); err != nil {
		return err
	}

	err = kruntime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &node)
	if err != nil {
		return err
	}

	node.Spec.Unschedulable = false
	node.Status = corev1.NodeStatus{}

	uMap, err = kruntime.DefaultUnstructuredConverter.ToUnstructured(&node)
	if err != nil {
		return err
	}
	unstruct.Object = uMap

	err = k.KubeCreateOrUpdate(ctx, unstruct)
	if err != nil {
		return err
	}

	return nil
}
