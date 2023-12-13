// Copyright (c) 2019 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package infrastructure

import (
	"context"
	"fmt"
	"strings"

	extensionscontroller "github.com/gardener/gardener/extensions/pkg/controller"
	"github.com/gardener/gardener/extensions/pkg/controller/infrastructure"
	extensionsv1alpha1 "github.com/gardener/gardener/pkg/apis/extensions/v1alpha1"
	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/gardener/gardener-extension-provider-gcp/pkg/apis/gcp/v1alpha1"
	"github.com/gardener/gardener-extension-provider-gcp/pkg/controller/infrastructure/infraflow"
	gcptypes "github.com/gardener/gardener-extension-provider-gcp/pkg/gcp"
	"github.com/gardener/gardener-extension-provider-gcp/pkg/internal"
	infrainternal "github.com/gardener/gardener-extension-provider-gcp/pkg/internal/infrastructure"
)

type actuator struct {
	client                     client.Client
	restConfig                 *rest.Config
	disableProjectedTokenMount bool
}

// NewActuator creates a new infrastructure.Actuator.
func NewActuator(mgr manager.Manager, disableProjectedTokenMount bool) infrastructure.Actuator {
	return &actuator{
		client:                     mgr.GetClient(),
		restConfig:                 mgr.GetConfig(),
		disableProjectedTokenMount: disableProjectedTokenMount,
	}
}

func (a *actuator) updateProviderStatusAndState(
	ctx context.Context,
	infra *extensionsv1alpha1.Infrastructure,
	status *v1alpha1.InfrastructureStatus,
	state *runtime.RawExtension,
) error {
	infra.Status.ProviderStatus = &runtime.RawExtension{Object: status}
	infra.Status.State = state
	return a.client.Status().Patch(ctx, infra, client.MergeFrom(infra.DeepCopy()))
}

func (a *actuator) cleanupTerraformerResources(ctx context.Context, log logr.Logger, infra *extensionsv1alpha1.Infrastructure) error {
	tf, err := internal.NewTerraformer(log, a.restConfig, infrainternal.TerraformerPurpose, infra, a.disableProjectedTokenMount)
	if err != nil {
		return err
	}

	if err := tf.CleanupConfiguration(ctx); err != nil {
		return err
	}

	return tf.RemoveTerraformerFinalizerFromConfig(ctx) // Explicitly clean up the terraformer finalizers
}

func hasFlowState(status extensionsv1alpha1.InfrastructureStatus) (bool, error) {
	if status.State == nil {
		return false, nil
	}

	flowState := runtime.TypeMeta{}
	stateJson, err := status.State.MarshalJSON()
	if err != nil {
		return false, err
	}

	if err := json.Unmarshal(stateJson, &flowState); err != nil {
		return false, err
	}

	if flowState.GroupVersionKind().GroupVersion() == v1alpha1.SchemeGroupVersion {
		return true, nil
	}

	infraState := &infrainternal.InfrastructureState{}
	if err := json.Unmarshal(status.State.Raw, infraState); err != nil {
		return false, err
	}

	if infraState.TerraformState != nil {
		return false, nil
	}

	return false, fmt.Errorf("unknown infrastructure state format")
}

// HasFlowAnnotation returns true if the new flow reconciler should be used for the reconciliation.
func HasFlowAnnotation(infrastructure *extensionsv1alpha1.Infrastructure, cluster *extensionscontroller.Cluster) bool {
	if hasShootAnnotation(infrastructure, cluster, gcptypes.AnnotationKeyUseTerraform) {
		return false
	}

	if hasShootAnnotation(infrastructure, cluster, gcptypes.AnnotationKeyUseFlow) {
		return true
	}

	return cluster.Seed != nil && cluster.Seed.Annotations != nil && strings.EqualFold(cluster.Seed.Annotations[gcptypes.AnnotationKeyUseFlow], "true")
}

func hasShootAnnotation(infrastructure *extensionsv1alpha1.Infrastructure, cluster *extensionscontroller.Cluster, key string) bool {
	return (infrastructure.Annotations != nil && strings.EqualFold(infrastructure.Annotations[key], "true")) || (cluster.Shoot != nil && cluster.Shoot.Annotations != nil && strings.EqualFold(cluster.Shoot.Annotations[key], "true"))
}

func getFlowStateFromInfrastructureStatus(infrastructure *extensionsv1alpha1.Infrastructure) (*infraflow.FlowState, error) {
	if infrastructure.Status.State == nil || len(infrastructure.Status.State.Raw) == 0 {
		return nil, nil
	}

	stateJSON, err := infrastructure.Status.State.MarshalJSON()
	if err != nil {
		return nil, err
	}

	isFlowState, err := infraflow.IsJSONFlowState(stateJSON)
	if err != nil {
		return nil, err
	}
	if isFlowState {
		return infraflow.NewFlowStateFromJSON(stateJSON)
	}

	return nil, nil
}

func shouldUseFlow(infra *extensionsv1alpha1.Infrastructure, cluster *extensionscontroller.Cluster) (bool, error) {
	state, err := getFlowStateFromInfrastructureStatus(infra)
	if err != nil {
		return false, err
	}

	if state != nil {
		return true, nil
	}

	return strings.EqualFold(infra.Annotations[gcptypes.AnnotationKeyUseFlow], "true") ||
		(cluster.Shoot != nil && strings.EqualFold(cluster.Shoot.Annotations[gcptypes.AnnotationKeyUseFlow], "true")) ||
		(cluster.Seed != nil && strings.EqualFold(cluster.Seed.Labels[gcptypes.SeedLabelKeyUseFlow], "true")), nil
}
