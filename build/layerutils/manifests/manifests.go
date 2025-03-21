// Copyright © 2021 Alibaba Group Holding Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package manifest

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/sealerio/sealer/build/layerutils"
)

type Manifests struct{}

// ListImages List all the containers images in manifest files
func (manifests *Manifests) ListImages(yamlFile string) ([]string, error) {
	var list []string

	yamlBytes, err := os.ReadFile(filepath.Clean(yamlFile))
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %s", err)
	}

	images := layerutils.DecodeImages(string(yamlBytes))
	if len(images) != 0 {
		list = append(list, images...)
	}

	if err != nil {
		return list, fmt.Errorf("failed to walk filepath: %s", err)
	}

	return list, nil
}

func NewManifests() (layerutils.Interface, error) {
	return &Manifests{}, nil
}
