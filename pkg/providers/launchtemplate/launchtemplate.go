/*
Portions Copyright (c) Microsoft Corporation.

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

package launchtemplate

import (
	"context"
	"os"
	"strings"

	"github.com/Azure/go-autorest/autorest/to"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/imagefamily"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/launchtemplate/parameters"
	"github.com/Azure/karpenter-provider-azure/pkg/utils"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"

	"github.com/Azure/karpenter-provider-azure/pkg/apis/v1alpha2"
	"github.com/Azure/karpenter-provider-azure/pkg/operator/options"
	corev1beta1 "sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

const (
	karpenterManagedTagKey = "karpenter.azure.com/cluster"

	// AzureCNI VNET Labels
	vnetDataPlaneLabel      = "kubernetes.azure.com/ebpf-dataplane"
	vnetNetworkNameLabel    = "kubernetes.azure.com/network-name"
	vnetSubnetNameLabel     = "kubernetes.azure.com/network-subnet"
	vnetSubscriptionIDLabel = "kubernetes.azure.com/network-subscription"
	vnetGUIDLabel           = "kubernetes.azure.com/nodenetwork-vnetguid"
	vnetPodNetworkTypeLabel = "kubernetes.azure.com/podnetwork-type"
	
	ciliumNetworkPlugin = "cilium" 
	overlayNetworkType  = "overlay"
)

type Template struct {
	UserData string
	ImageID  string
	Tags     map[string]*string
}

type Provider struct {
	imageFamily            *imagefamily.Resolver
	imageProvider          *imagefamily.Provider
	caBundle               *string
	clusterEndpoint        string
	tenantID               string
	subscriptionID         string
	userAssignedIdentityID string
	resourceGroup          string
	location               string
}

// TODO: add caching of launch templates

func NewProvider(_ context.Context, imageFamily *imagefamily.Resolver, imageProvider *imagefamily.Provider, caBundle *string, clusterEndpoint string,
	tenantID, subscriptionID, userAssignedIdentityID, resourceGroup, location string,
) *Provider {
	return &Provider{
		imageFamily:            imageFamily,
		imageProvider:          imageProvider,
		caBundle:               caBundle,
		clusterEndpoint:        clusterEndpoint,
		tenantID:               tenantID,
		subscriptionID:         subscriptionID,
		userAssignedIdentityID: userAssignedIdentityID,
		resourceGroup:          resourceGroup,
		location:               location,
	}
}

func (p *Provider) GetTemplate(ctx context.Context, nodeClass *v1alpha2.AKSNodeClass, nodeClaim *corev1beta1.NodeClaim,
	instanceType *cloudprovider.InstanceType, additionalLabels map[string]string) (*Template, error) {
	staticParameters := p.getStaticParameters(ctx, instanceType, nodeClass, lo.Assign(nodeClaim.Labels, additionalLabels))
	kubeServerVersion, err := p.imageProvider.KubeServerVersion(ctx)
	if err != nil {
		return nil, err
	}
	staticParameters.KubernetesVersion = kubeServerVersion
	templateParameters, err := p.imageFamily.Resolve(ctx, nodeClass, nodeClaim, instanceType, staticParameters)
	if err != nil {
		return nil, err
	}
	launchTemplate, err := p.createLaunchTemplate(ctx, templateParameters)
	if err != nil {
		return nil, err
	}

	return launchTemplate, nil
}

func (p *Provider) getStaticParameters(ctx context.Context, instanceType *cloudprovider.InstanceType, nodeClass *v1alpha2.AKSNodeClass, labels map[string]string) *parameters.StaticParameters {
	var arch string = corev1beta1.ArchitectureAmd64
	if err := instanceType.Requirements.Compatible(scheduling.NewRequirements(scheduling.NewRequirement(v1.LabelArchStable, v1.NodeSelectorOpIn, corev1beta1.ArchitectureArm64))); err == nil {
		arch = corev1beta1.ArchitectureArm64
	}
	
	labels = lo.Assign(labels, getVnetLabelValues(nodeClass)) 
	return &parameters.StaticParameters{
		ClusterName:                    options.FromContext(ctx).ClusterName,
		ClusterEndpoint:                p.clusterEndpoint,
		Tags:                           nodeClass.Spec.Tags,
		Labels:                         labels,
		CABundle:                       p.caBundle,
		Arch:                           arch,
		GPUNode:                        utils.IsNvidiaEnabledSKU(instanceType.Name),
		GPUDriverVersion:               utils.GetGPUDriverVersion(instanceType.Name),
		GPUImageSHA:                    utils.GetAKSGPUImageSHA(instanceType.Name),
		TenantID:                       p.tenantID,
		SubscriptionID:                 p.subscriptionID,
		UserAssignedIdentityID:         p.userAssignedIdentityID,
		ResourceGroup:                  p.resourceGroup,
		Location:                       p.location,
		ClusterID:                      options.FromContext(ctx).ClusterID,
		APIServerName:                  options.FromContext(ctx).GetAPIServerName(),
		KubeletClientTLSBootstrapToken: options.FromContext(ctx).KubeletClientTLSBootstrapToken,
		NetworkPlugin:                  options.FromContext(ctx).NetworkPlugin,
		NetworkPolicy:                  options.FromContext(ctx).NetworkPolicy,
	}
}

func (p *Provider) createLaunchTemplate(_ context.Context, options *parameters.Parameters) (*Template, error) {
	// render user data
	userData, err := options.UserData.Script()
	if err != nil {
		return nil, err
	}

	// merge and convert to ARM tags
	azureTags := mergeTags(options.Tags, map[string]string{karpenterManagedTagKey: options.ClusterName})
	template := &Template{
		UserData: userData,
		ImageID:  options.ImageID,
		Tags:     azureTags,
	}
	return template, nil
}



// getVnetLabelValues returns the labels for AzureCNI for the vnet and subnet. 
// See how split logic works here: https://go.dev/play/p/l3l7Zrg_pdd
func getVnetLabelValues(nodeClass *v1alpha2.AKSNodeClass) map[string]string {
	// this assumes that we would panic if AZURE_SUBNET_ID is not set
	vnetSubnetID := lo.Ternary(nodeClass.Spec.VnetSubnetID != nil, *nodeClass.Spec.VnetSubnetID, os.Getenv("AZURE_SUBNET_ID"))
	vnetSubnetParts := strings.Split(vnetSubnetID, "/")

	vnetLabels := map[string]string{
		vnetDataPlaneLabel: ciliumNetworkPlugin,
		vnetNetworkNameLabel: vnetSubnetParts[len(vnetSubnetParts)-3],
		vnetSubnetNameLabel:vnetSubnetParts[len(vnetSubnetParts)-1],
		vnetSubscriptionIDLabel: vnetSubnetParts[2],
		vnetGUIDLabel: os.Getenv("AZURE_VNET_GUID"), // this configuration is resolved in handleVNET inside of the azure_clients
		vnetPodNetworkTypeLabel: overlayNetworkType,
	}

	return vnetLabels
}

// MergeTags takes a variadic list of maps and merges them together
// with format acceptable to ARM (no / in keys, pointer to strings as values)
func mergeTags(tags ...map[string]string) (result map[string]*string) {
	return lo.MapEntries(lo.Assign(tags...), func(key string, value string) (string, *string) {
		return strings.ReplaceAll(key, "/", "_"), to.StringPtr(value)
	})
}
