// This file is part of MinIO Direct CSI
// Copyright (c) 2021 MinIO, Inc.
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"k8s.io/apiextensions-apiserver/pkg/apihelpers"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/golang/glog"
	"github.com/minio/direct-csi/pkg/converter"
	"github.com/minio/direct-csi/pkg/utils"
	"github.com/minio/direct-csi/pkg/utils/installer"
	"k8s.io/apimachinery/pkg/api/errors"
)

const (
	currentCRDStorageVersion = "v1beta1"
	driveCRDName             = "directcsidrives.direct.csi.min.io"
	volumeCRDName            = "directcsivolumes.direct.csi.min.io"
)

func registerCRDs(ctx context.Context, identity string) error {
	crdObjs := []runtime.Object{}
	for _, asset := range AssetNames() {
		crdBytes, err := Asset(asset)
		if err != nil {
			return err
		}
		crdObj, err := utils.ParseSingleKubeNativeFromBytes(crdBytes)
		if err != nil {
			return err
		}
		crdObjs = append(crdObjs, crdObj)
	}

	crdClient := utils.GetCRDClient()
	for _, crd := range crdObjs {
		if dryRun {
			if err := utils.LogYAML(crd); err != nil {
				return err
			}
			continue
		}
		var crdObj apiextensions.CustomResourceDefinition
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(crd.(*unstructured.Unstructured).Object, &crdObj); err != nil {
			return err
		}

		existingCRD, err := crdClient.Get(ctx, crdObj.Name, metav1.GetOptions{})
		if err != nil {
			if !errors.IsNotFound(err) {
				return err
			}
			if err := setConversionWebhook(&crdObj, identity); err != nil {
				return err
			}
			if _, err := crdClient.Create(ctx, &crdObj, metav1.CreateOptions{}); err != nil {
				return err
			}
			continue
		}
		if err := syncCRD(ctx, existingCRD, crdObj, identity); err != nil {
			return err
		}
	}
	return nil
}

func syncCRD(ctx context.Context, existingCRD *apiextensions.CustomResourceDefinition, newCRD apiextensions.CustomResourceDefinition, identity string) error {
	existingCRDStorageVersion, err := apihelpers.GetCRDStorageVersion(existingCRD)
	if err != nil {
		return err
	}

	if existingCRDStorageVersion == currentCRDStorageVersion {
		return nil // CRDs already updated and holds the latest version
	}

	// Set all the existing versions to false
	func() {
		for i := range existingCRD.Spec.Versions {
			existingCRD.Spec.Versions[i].Storage = false
		}
	}()

	latestVersionObject, err := getLatestCRDVersionObject(newCRD)
	if err != nil {
		return err
	}

	existingCRD.Spec.Versions = append(existingCRD.Spec.Versions, latestVersionObject)

	if err := setConversionWebhook(existingCRD, identity); err != nil {
		return err
	}

	crdClient := utils.GetCRDClient()
	if _, err := crdClient.Update(ctx, existingCRD, metav1.UpdateOptions{}); err != nil {
		return err
	}

	glog.Infof("'%s' CRD succesfully updated to '%s'", existingCRD.Name, utils.Bold(currentCRDStorageVersion))

	return nil
}

func setConversionWebhook(crdObj *apiextensions.CustomResourceDefinition, identity string) error {

	name := installer.SanitizeName(identity)
	getServiceRef := func() *apiextensions.ServiceReference {
		path := func() string {
			switch crdObj.Name {
			case driveCRDName:
				return converter.DriveHandlerPath
			case volumeCRDName:
				return converter.VolumeHandlerPath
			default:
				panic("unknown crd name found")
			}
		}()

		return &apiextensions.ServiceReference{
			Namespace: name,
			Name:      installer.GetConversionServiceName(),
			Path:      &path,
		}
	}

	getWebhookClientConfig := func() (*apiextensions.WebhookClientConfig, error) {
		caBundle, err := installer.GetConversionCABundle()
		if err != nil {
			return nil, err
		}
		return &apiextensions.WebhookClientConfig{
			Service:  getServiceRef(),
			CABundle: []byte(caBundle),
		}, nil
	}

	getWebhookConversionSettings := func() (*apiextensions.WebhookConversion, error) {
		webhookClientConfig, err := getWebhookClientConfig()
		if err != nil {
			return nil, err
		}
		return &apiextensions.WebhookConversion{
			ClientConfig:             webhookClientConfig,
			ConversionReviewVersions: []string{"v1"},
		}, nil
	}

	getConversionSettings := func() (*apiextensions.CustomResourceConversion, error) {
		webhookConversionSettings, err := getWebhookConversionSettings()
		if err != nil {
			return nil, err
		}
		return &apiextensions.CustomResourceConversion{
			Strategy: apiextensions.WebhookConverter,
			Webhook:  webhookConversionSettings,
		}, nil
	}

	conversionSettings, err := getConversionSettings()
	if err != nil {
		return err
	}

	crdObj.Spec.Conversion = conversionSettings
	return nil
}

func getLatestCRDVersionObject(newCRD apiextensions.CustomResourceDefinition) (apiextensions.CustomResourceDefinitionVersion, error) {
	for i := range newCRD.Spec.Versions {
		if newCRD.Spec.Versions[i].Name == currentCRDStorageVersion {
			return newCRD.Spec.Versions[i], nil
		}
	}

	return apiextensions.CustomResourceDefinitionVersion{}, fmt.Errorf("No version %v foung crd %v", currentCRDStorageVersion, newCRD.Name)
}

func unregisterCRDs(ctx context.Context) error {
	crdNames := []string{}
	for _, asset := range AssetNames() {
		base := filepath.Base(asset)
		baseWithoutExtension := strings.Split(base, ".yaml")[0]
		parts := strings.Split(baseWithoutExtension, "_")
		if len(parts) != 2 {
			return fmt.Errorf("invalid crd name: %v", baseWithoutExtension)
		}
		crd := parts[1]
		apiGroup := parts[0]
		crdNames = append(crdNames, fmt.Sprintf("%s.%s", crd, apiGroup))
	}

	crdClient := utils.GetCRDClient()
	for _, crd := range crdNames {
		if err := crdClient.Delete(ctx, crd, metav1.DeleteOptions{}); err != nil {
			if !errors.IsNotFound(err) {
				return err
			}
		}
	}

	return nil
}
