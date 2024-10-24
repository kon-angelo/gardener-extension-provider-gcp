// Copyright (c) 2020 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file
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

package app

import (
	"context"
	"fmt"

	admissioncmd "github.com/gardener/gardener-extension-provider-gcp/pkg/admission/cmd"
	gcpinstall "github.com/gardener/gardener-extension-provider-gcp/pkg/apis/gcp/install"
	providergcp "github.com/gardener/gardener-extension-provider-gcp/pkg/gcp"

	controllercmd "github.com/gardener/gardener/extensions/pkg/controller/cmd"
	"github.com/gardener/gardener/extensions/pkg/util"
	"github.com/gardener/gardener/extensions/pkg/util/index"
	webhookcmd "github.com/gardener/gardener/extensions/pkg/webhook/cmd"
	"github.com/gardener/gardener/pkg/apis/core/install"
	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	componentbaseconfig "k8s.io/component-base/config"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

var log = logf.Log.WithName("gardener-extension-admission-gcp")

// NewAdmissionCommand creates a new command for running a GCP gardener-extension-admission-gcp webhook.
func NewAdmissionCommand(ctx context.Context) *cobra.Command {
	var (
		restOpts = &controllercmd.RESTOptions{}
		mgrOpts  = &controllercmd.ManagerOptions{
			WebhookServerPort: 443,
		}

		webhookSwitches = admissioncmd.GardenWebhookSwitchOptions()
		webhookOptions  = webhookcmd.NewAddToManagerSimpleOptions(webhookSwitches)

		aggOption = controllercmd.NewOptionAggregator(
			restOpts,
			mgrOpts,
			webhookOptions,
		)
	)

	cmd := &cobra.Command{
		Use: fmt.Sprintf("admission-%s", providergcp.Type),

		RunE: func(cmd *cobra.Command, args []string) error {
			if err := aggOption.Complete(); err != nil {
				return fmt.Errorf("error completing options: %v", err)
			}

			util.ApplyClientConnectionConfigurationToRESTConfig(&componentbaseconfig.ClientConnectionConfiguration{
				QPS:   100.0,
				Burst: 130,
			}, restOpts.Completed().Config)

			mgr, err := manager.New(restOpts.Completed().Config, mgrOpts.Completed().Options())
			if err != nil {
				return fmt.Errorf("could not instantiate manager: %v", err)
			}

			install.Install(mgr.GetScheme())

			if err := gcpinstall.AddToScheme(mgr.GetScheme()); err != nil {
				return fmt.Errorf("could not update manager scheme: %v", err)
			}

			if err := mgr.GetFieldIndexer().IndexField(ctx, &gardencorev1beta1.SecretBinding{}, index.SecretRefNamespaceField, index.SecretRefNamespaceIndexerFunc); err != nil {
				return err
			}
			if err := mgr.GetFieldIndexer().IndexField(ctx, &gardencorev1beta1.Shoot{}, index.SecretBindingNameField, index.SecretBindingNameIndexerFunc); err != nil {
				return err
			}

			log.Info("Setting up webhook server")

			if err := webhookOptions.Completed().AddToManager(mgr); err != nil {
				return err
			}

			// List all secrets to implicitly start a WATCH. This will fill up the webhook server's caches (hence, increasing
			// its memory usage) right at the start-up. This is done to prevent unpredictable load spikes when the server receives
			// its first request (without this list it would only then start the WATCH, leading to a huge memory increase on large
			// landscapes. Hence, this "early listing" should prevent the server from being OOMKilled and (potentially multiple times)
			// scaled by VPA (we have observed too "optimistic" up-scaling behavior of VPA in such cases).
			// See https://github.com/gardener/gardener-extension-provider-gcp/pull/234 for more details.
			if err := mgr.Add(&secretLister{mgr}); err != nil {
				return err
			}

			return mgr.Start(ctx)
		},
	}

	aggOption.AddFlags(cmd.Flags())

	return cmd
}

// secretLister implements the manager.Runnable interface.
type secretLister struct{ mgr manager.Manager }

func (s *secretLister) Start(ctx context.Context) error {
	return s.mgr.GetClient().List(ctx, &corev1.SecretList{})
}
