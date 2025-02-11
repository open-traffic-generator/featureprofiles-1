// Copyright 2023 Google LLC
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

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/cloudbuild/v1"
	"gopkg.in/yaml.v2"

	"github.com/google/uuid"
)

type cloudBuild struct {
	device device

	buildClient *cloudbuild.Service
	storClient  *storage.Client
	f           fs.FS
}

// submitBuild creates a CB Build and returns the jobID and log URL created.
func (c *cloudBuild) submitBuild(ctx context.Context) (string, string, error) {
	build, err := c.defaultBuild()
	if err != nil {
		return "", "", err
	}

	vendor := strings.ToLower(c.device.Type.Vendor.String())
	vendor = strings.ReplaceAll(vendor, " ", "")
	model := strings.ToLower(c.device.Type.HardwareModel)
	model = strings.ReplaceAll(model, " ", "")
	build.Substitutions["_DUT_PLATFORM"] = vendor + "_" + model
	if machineType, ok := virtualDeviceMachineType[c.device.Type]; ok {
		if strings.Contains(machineType, "n2-standard") {
			build.Substitutions["_MACHINE_ARGS"] = "--enable-nested-virtualization"
		}
		build.Substitutions["_MACHINE_TYPE"] = machineType
	}
	var testPaths strings.Builder
	for i, t := range c.device.Tests {
		if i != 0 {
			testPaths.WriteString(" ")
		}
		fmt.Fprintf(&testPaths, "%s,%s", t.Path, t.BadgePath)
	}
	build.Substitutions["_DUT_TESTS"] = testPaths.String()

	objPath, err := c.prepareBuild(ctx)
	if err != nil {
		return "", "", err
	}

	build.Source = &cloudbuild.Source{
		StorageSource: &cloudbuild.StorageSource{
			Bucket: gcpCloudBuildBucketName,
			Object: objPath,
		},
	}

	resp, err := c.buildClient.Projects.Locations.Builds.Create("projects/"+gcpProjectID+"/locations/us-west1", build).Do()
	if err != nil {
		return "", "", err
	}

	var bom cloudbuild.BuildOperationMetadata
	if err := json.Unmarshal(resp.Metadata, &bom); err != nil {
		return "", "", fmt.Errorf("could not unmarshal BuildOperationMetadata: %w", err)
	}
	return bom.Build.Id, bom.Build.LogUrl, nil
}

// createTGZArchive returns a tar.gz compressed archive of the cloudBuild fs.
func (c *cloudBuild) createTGZArchive() (*bytes.Buffer, error) {
	var buf bytes.Buffer

	gzWriter := gzip.NewWriter(&buf)
	defer gzWriter.Close()

	tarWriter := tar.NewWriter(gzWriter)
	defer tarWriter.Close()

	err := fs.WalkDir(c.f, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == "." {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = path

		if err := tarWriter.WriteHeader(header); err != nil {
			return err
		}

		if d.IsDir() {
			return nil
		}

		file, err := c.f.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()

		_, err = io.Copy(tarWriter, file)
		return err
	})
	return &buf, err
}

// prepareBuild uploads the compressed repository to Object Store and returns the path to the object.
func (c *cloudBuild) prepareBuild(ctx context.Context) (string, error) {
	data, err := c.createTGZArchive()
	if err != nil {
		return "", err
	}

	u, err := uuid.NewRandom()
	if err != nil {
		return "", err
	}

	objPath := "source/" + strconv.FormatInt(time.Now().UTC().Unix(), 10) + "-" + hex.EncodeToString(u[:]) + ".tgz"
	obj := c.storClient.Bucket(gcpCloudBuildBucketName).Object(objPath).NewWriter(ctx)
	obj.ContentType = "application/x-tar"
	if _, err := data.WriteTo(obj); err != nil {
		return "", err
	}
	return objPath, obj.Close()
}

// defaultBuild returns the Cloud Build configuration stored in the repository.
func (c *cloudBuild) defaultBuild() (*cloudbuild.Build, error) {
	buildYAML, err := fs.ReadFile(c.f, "cloudbuild/virtual.yaml")
	if err != nil {
		return nil, err
	}

	var build *cloudbuild.Build
	err = yaml.Unmarshal(buildYAML, &build)
	return build, err
}
