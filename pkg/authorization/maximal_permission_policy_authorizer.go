/*
Copyright 2022 The KCP Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package authorization

import (
	"context"
	"fmt"

	kcpkubernetesinformers "github.com/kcp-dev/client-go/clients/informers"
	"github.com/kcp-dev/logicalcluster/v2"

	kaudit "k8s.io/apiserver/pkg/audit"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/client-go/tools/cache"
	"k8s.io/kubernetes/pkg/genericcontrolplane"
	"k8s.io/kubernetes/plugin/pkg/auth/authorizer/rbac"

	apisv1alpha1 "github.com/kcp-dev/kcp/pkg/apis/apis/v1alpha1"
	kcpinformers "github.com/kcp-dev/kcp/pkg/client/informers/externalversions"
	"github.com/kcp-dev/kcp/pkg/indexers"
	rbacwrapper "github.com/kcp-dev/kcp/pkg/virtual/framework/wrappers/rbac"
)

const (
	MaximalPermissionPolicyAccessNotPermittedReason = "access not permitted by maximal permission policy"

	MaximalPermissionPolicyAuditPrefix   = "maxpermissionpolicy.authorization.kcp.dev/"
	MaximalPermissionPolicyAuditDecision = MaximalPermissionPolicyAuditPrefix + "decision"
	MaximalPermissionPolicyAuditReason   = MaximalPermissionPolicyAuditPrefix + "reason"
)

// NewMaximalPermissionPolicyAuthorizer returns an authorizer that first checks if the request is for a
// bound resource or not. If the resource is bound it checks the maximal permission policy of the underlying API export.
func NewMaximalPermissionPolicyAuthorizer(kubeInformers kcpkubernetesinformers.SharedInformerFactory, kcpInformers kcpinformers.SharedInformerFactory, delegate authorizer.Authorizer) (authorizer.Authorizer, error) {
	apiBindingIndexer := kcpInformers.Apis().V1alpha1().APIBindings().Informer().GetIndexer()
	apiExportIndexer := kcpInformers.Apis().V1alpha1().APIExports().Informer().GetIndexer()

	// Make sure informer knows what to watch
	kubeInformers.Rbac().V1().Roles().Lister()
	kubeInformers.Rbac().V1().RoleBindings().Lister()
	kubeInformers.Rbac().V1().ClusterRoles().Lister()
	kubeInformers.Rbac().V1().ClusterRoleBindings().Lister()

	return &MaximalPermissionPolicyAuthorizer{
		getAPIBindingReferenceForAttributes: func(attr authorizer.Attributes, clusterName logicalcluster.Name) (*apisv1alpha1.ExportReference, bool, error) {
			return getAPIBindingReferenceForAttributes(apiBindingIndexer, attr, clusterName)
		},
		getAPIExportByReference: func(exportRef *apisv1alpha1.ExportReference) (*apisv1alpha1.APIExport, bool, error) {
			return getAPIExportByReference(apiExportIndexer, exportRef)
		},
		newAuthorizer: func(clusterName logicalcluster.Name) authorizer.Authorizer {
			return rbac.New(
				&rbac.RoleGetter{Lister: rbacwrapper.NewMergedRoleLister(
					kubeInformers.Rbac().V1().Roles().Lister().Cluster(clusterName),
					kubeInformers.Rbac().V1().Roles().Lister().Cluster(genericcontrolplane.LocalAdminCluster),
				)},
				&rbac.RoleBindingLister{Lister: kubeInformers.Rbac().V1().RoleBindings().Lister().Cluster(clusterName)},
				&rbac.ClusterRoleGetter{Lister: rbacwrapper.NewMergedClusterRoleLister(
					kubeInformers.Rbac().V1().ClusterRoles().Lister().Cluster(clusterName),
					kubeInformers.Rbac().V1().ClusterRoles().Lister().Cluster(genericcontrolplane.LocalAdminCluster),
				)},
				&rbac.ClusterRoleBindingLister{Lister: rbacwrapper.NewMergedClusterRoleBindingLister(
					kubeInformers.Rbac().V1().ClusterRoleBindings().Lister().Cluster(clusterName),
					kubeInformers.Rbac().V1().ClusterRoleBindings().Lister().Cluster(genericcontrolplane.LocalAdminCluster),
				)},
			)
		},
		delegate: delegate,
	}, nil
}

type MaximalPermissionPolicyAuthorizer struct {
	delegate authorizer.Authorizer

	getAPIBindingReferenceForAttributes func(attr authorizer.Attributes, clusterName logicalcluster.Name) (ref *apisv1alpha1.ExportReference, found bool, err error)
	getAPIExportByReference             func(exportRef *apisv1alpha1.ExportReference) (ref *apisv1alpha1.APIExport, found bool, err error)
	newAuthorizer                       func(clusterName logicalcluster.Name) authorizer.Authorizer
}

func (a *MaximalPermissionPolicyAuthorizer) Authorize(ctx context.Context, attr authorizer.Attributes) (authorizer.Decision, string, error) {
	// get the cluster from the ctx.
	lcluster, err := genericapirequest.ClusterNameFrom(ctx)
	if err != nil {
		kaudit.AddAuditAnnotations(
			ctx,
			MaximalPermissionPolicyAuditDecision, DecisionNoOpinion,
			MaximalPermissionPolicyAuditReason, fmt.Sprintf("error getting cluster from request: %v", err),
		)
		return authorizer.DecisionNoOpinion, MaximalPermissionPolicyAccessNotPermittedReason, err
	}

	bindingLogicalCluster, bound, err := a.getAPIBindingReferenceForAttributes(attr, lcluster)
	if err != nil {
		kaudit.AddAuditAnnotations(
			ctx,
			MaximalPermissionPolicyAuditDecision, DecisionNoOpinion,
			MaximalPermissionPolicyAuditReason, fmt.Sprintf("error getting API binding reference: %v", err),
		)
		return authorizer.DecisionNoOpinion, MaximalPermissionPolicyAccessNotPermittedReason, err
	}

	if !bound {
		kaudit.AddAuditAnnotations(
			ctx,
			MaximalPermissionPolicyAuditDecision, DecisionAllowed,
			MaximalPermissionPolicyAuditReason, "no API binding bound",
		)
		return a.delegate.Authorize(ctx, attr)
	}

	apiExport, found, err := a.getAPIExportByReference(bindingLogicalCluster)
	if err != nil {
		kaudit.AddAuditAnnotations(
			ctx,
			MaximalPermissionPolicyAuditDecision, DecisionNoOpinion,
			MaximalPermissionPolicyAuditReason, fmt.Sprintf("error getting API export: %v", err),
		)
		return authorizer.DecisionNoOpinion, MaximalPermissionPolicyAccessNotPermittedReason, err
	}

	path := "unknown"
	exportName := "unknown"
	if bindingLogicalCluster.Workspace != nil {
		exportName = bindingLogicalCluster.Workspace.ExportName
		path = bindingLogicalCluster.Workspace.Path
	}

	// If we can't find the export default to close
	if !found {
		kaudit.AddAuditAnnotations(
			ctx,
			MaximalPermissionPolicyAuditDecision, DecisionNoOpinion,
			MaximalPermissionPolicyAuditReason, fmt.Sprintf("API export %q not found, path: %q", exportName, path),
		)
		return authorizer.DecisionNoOpinion, MaximalPermissionPolicyAccessNotPermittedReason, err
	}

	if apiExport.Spec.MaximalPermissionPolicy == nil {
		kaudit.AddAuditAnnotations(
			ctx,
			MaximalPermissionPolicyAuditDecision, DecisionAllowed,
			MaximalPermissionPolicyAuditReason, fmt.Sprintf("no maximal permission policy present in API export %q, path: %q, owning cluster: %q", exportName, path, logicalcluster.From(apiExport)),
		)
		return a.delegate.Authorize(ctx, attr)
	}

	if apiExport.Spec.MaximalPermissionPolicy.Local == nil {
		kaudit.AddAuditAnnotations(
			ctx,
			MaximalPermissionPolicyAuditDecision, DecisionAllowed,
			MaximalPermissionPolicyAuditReason, fmt.Sprintf("no maximal local permission policy present in API export %q, path: %q, owning cluster: %q", apiExport.Name, path, logicalcluster.From(apiExport)),
		)
		return a.delegate.Authorize(ctx, attr)
	}

	// If bound, create a rbac authorizer filtered to the cluster.
	clusterAuthorizer := a.newAuthorizer(logicalcluster.From(apiExport))
	prefixedAttr := deepCopyAttributes(attr)
	userInfo := prefixedAttr.User.(*user.DefaultInfo)
	userInfo.Name = apisv1alpha1.MaximalPermissionPolicyRBACUserGroupPrefix + userInfo.Name
	userInfo.Groups = make([]string, 0, len(attr.GetUser().GetGroups()))
	for _, g := range attr.GetUser().GetGroups() {
		userInfo.Groups = append(userInfo.Groups, apisv1alpha1.MaximalPermissionPolicyRBACUserGroupPrefix+g)
	}
	dec, reason, err := clusterAuthorizer.Authorize(ctx, prefixedAttr)
	if err != nil {
		kaudit.AddAuditAnnotations(
			ctx,
			MaximalPermissionPolicyAuditDecision, DecisionNoOpinion,
			MaximalPermissionPolicyAuditReason, fmt.Sprintf("error authorizing RBAC in API export cluster %q: %v", logicalcluster.From(apiExport), err),
		)
		return authorizer.DecisionNoOpinion, reason, err
	}

	kaudit.AddAuditAnnotations(
		ctx,
		MaximalPermissionPolicyAuditDecision, DecisionString(dec),
		MaximalPermissionPolicyAuditReason, fmt.Sprintf("API export cluster %q reason: %v", logicalcluster.From(apiExport), reason),
	)

	if dec == authorizer.DecisionAllow {
		return a.delegate.Authorize(ctx, attr)
	}

	return authorizer.DecisionNoOpinion, reason, nil
}

func getAPIBindingReferenceForAttributes(apiBindingIndexer cache.Indexer, attr authorizer.Attributes, clusterName logicalcluster.Name) (*apisv1alpha1.ExportReference, bool, error) {
	objs, err := apiBindingIndexer.ByIndex(indexers.ByLogicalCluster, clusterName.String())
	if err != nil {
		return nil, false, err
	}
	for _, obj := range objs {
		apiBinding := obj.(*apisv1alpha1.APIBinding)
		for _, br := range apiBinding.Status.BoundResources {
			if br.Group == attr.GetAPIGroup() && br.Resource == attr.GetResource() {
				return &apiBinding.Spec.Reference, true, nil
			}
		}
	}
	return nil, false, nil
}

func getAPIExportByReference(apiExportIndexer cache.Indexer, exportRef *apisv1alpha1.ExportReference) (*apisv1alpha1.APIExport, bool, error) {
	objs, err := apiExportIndexer.ByIndex(indexers.ByLogicalCluster, exportRef.Workspace.Path)
	if err != nil {
		return nil, false, err
	}
	for _, obj := range objs {
		apiExport := obj.(*apisv1alpha1.APIExport)
		if apiExport.Name == exportRef.Workspace.ExportName {
			return apiExport, true, err
		}
	}
	return nil, false, err

}
