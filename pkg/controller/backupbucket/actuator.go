// SPDX-FileCopyrightText: 2024 SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package backupbucket

import (
	"context"
	"fmt"

	"cloud.google.com/go/storage"
	"github.com/gardener/gardener/extensions/pkg/controller/backupbucket"
	"github.com/gardener/gardener/extensions/pkg/util"
	extensionsv1alpha1 "github.com/gardener/gardener/pkg/apis/extensions/v1alpha1"
	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/gardener/gardener-extension-provider-gcp/pkg/admission"
	"github.com/gardener/gardener-extension-provider-gcp/pkg/apis/gcp"
	"github.com/gardener/gardener-extension-provider-gcp/pkg/apis/gcp/helper"
	gcpv1alpha1 "github.com/gardener/gardener-extension-provider-gcp/pkg/apis/gcp/v1alpha1"
	gcpclient "github.com/gardener/gardener-extension-provider-gcp/pkg/gcp/client"
)

type actuator struct {
	backupbucket.Actuator
	client client.Client
}

func newActuator(mgr manager.Manager) backupbucket.Actuator {
	return &actuator{
		client: mgr.GetClient(),
	}
}

func (a *actuator) Reconcile(ctx context.Context, _ logr.Logger, bb *extensionsv1alpha1.BackupBucket) error {
	storageClient, err := gcpclient.NewStorageClientFromSecretRef(ctx, a.client, bb.Spec.SecretRef)
	if err != nil {
		return util.DetermineError(err, helper.KnownCodes)
	}

	backupBucketConfig, err := admission.DecodeBackupBucketConfig(serializer.NewCodecFactory(a.client.Scheme(), serializer.EnableStrict).UniversalDecoder(), bb.Spec.ProviderConfig)
	if err != nil {
		return err
	}

	attrs, err := storageClient.Attrs(ctx, bb.Name)
	if err != nil && err != storage.ErrObjectNotExist {
		return err
	}
	// create bucket
	if err == storage.ErrObjectNotExist {
		attrs = &storage.BucketAttrs{
			Name:     bb.Name,
			Location: bb.Spec.Region,
			UniformBucketLevelAccess: storage.UniformBucketLevelAccess{
				Enabled: true,
			},
			SoftDeletePolicy: &storage.SoftDeletePolicy{
				RetentionDuration: 0,
			},
		}
		if backupBucketConfig != nil && backupBucketConfig.Immutability != nil {
			attrs.RetentionPolicy = &storage.RetentionPolicy{
				RetentionPeriod: backupBucketConfig.Immutability.RetentionPeriod.Duration,
			}
		}
		if err := storageClient.CreateBucket(ctx, attrs); err != nil {
			return err
		}
	} else {
		if isUpdateRequired(attrs, backupBucketConfig) {
			if attrs, err = storageClient.UpdateBucket(ctx, bb.Name, bb.Spec.Region, storage.BucketAttrsToUpdate{}); err != nil {
				return err
			}
		}
	}

	if backupBucketConfig != nil && backupBucketConfig.Immutability != nil {
		return storageClient.LockBucket(ctx, bb.Name)
	}
	return nil
}

func isUpdateRequired(attrs *storage.BucketAttrs, config *gcp.BackupBucketConfig) bool {
	// Determine if an update is required based on the desired and current retention policies.
	isUpdateRequired := true
	if config.Immutability.RetentionPeriod.Duration desiredRetentionPolicy == nil && attrs.RetentionPolicy == nil {
		isUpdateRequired = false
	}

	if desiredRetentionPolicy != nil && attrs.RetentionPolicy != nil && *desiredRetentionPolicy == *attrs.RetentionPolicy {
		isUpdateRequired = false
	}

	// Perform the update if needed
	if isUpdateRequired {
		// If the desired retention policy is nil and the current retention policy is not nil,
		// it indicates that the retention policy needs to be removed. To achieve this, set
		// the RetentionPeriod to 0. This is required by the Google Cloud Storage API to
		// explicitly update and remove an existing retention policy.
		// For more details, refer to:
		// https://github.com/googleapis/google-cloud-go/blob/main/storage/bucket.go#L1172
		if desiredRetentionPolicy == nil && attrs.RetentionPolicy != nil {
			desiredRetentionPolicy = &storage.RetentionPolicy{}
		}
		bucketAttrsToUpdate := storage.BucketAttrsToUpdate{
			RetentionPolicy: desiredRetentionPolicy,
		}
		var err error
		attrs, err = bucket.Update(ctx, bucketAttrsToUpdate)
		if err != nil {
			return fmt.Errorf("failed to update retention policy for bucket %q: %w", bucket.BucketName(), err)
		}
	}
}

func (a *actuator) Delete(ctx context.Context, _ logr.Logger, bb *extensionsv1alpha1.BackupBucket) error {
	storageClient, err := gcpclient.NewStorageClientFromSecretRef(ctx, a.client, bb.Spec.SecretRef)
	if err != nil {
		return util.DetermineError(err, helper.KnownCodes)
	}

	return util.DetermineError(storageClient.DeleteBucketIfExists(ctx, bb.Name), helper.KnownCodes)
}
