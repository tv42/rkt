// Copyright 2015 The rkt Authors
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

package aci

import (
	"fmt"
	"log"

	"github.com/coreos/rkt/pkg/uid"

	ptar "github.com/coreos/rkt/pkg/tar"

	"github.com/coreos/rkt/Godeps/_workspace/src/github.com/appc/spec/pkg/acirenderer"
	"github.com/coreos/rkt/Godeps/_workspace/src/github.com/appc/spec/schema/types"
)

// Given an imageID, start with the matching image available in the store,
// build its dependency list and render it inside dir
func RenderACIWithImageID(imageID types.Hash, dir string, ap acirenderer.ACIRegistry, uidRange *uid.UidRange) error {
	log.Printf("RenderACIWithImageID %v to %v", imageID, dir)
	renderedACI, err := acirenderer.GetRenderedACIWithImageID(imageID, ap)
	if err != nil {
		log.Printf("RenderACIWithImageID error %v", err)
		return err
	}
	return renderImage(renderedACI, dir, ap, uidRange)
}

// Given an image app name and optional labels, get the best matching image
// available in the store, build its dependency list and render it inside dir
func RenderACI(name types.ACIdentifier, labels types.Labels, dir string, ap acirenderer.ACIRegistry, uidRange *uid.UidRange) error {
	renderedACI, err := acirenderer.GetRenderedACI(name, labels, ap)
	if err != nil {
		return err
	}
	return renderImage(renderedACI, dir, ap, uidRange)
}

// Given an already populated dependency list, it will extract, under the provided
// directory, the rendered ACI
func RenderACIFromList(imgs acirenderer.Images, dir string, ap acirenderer.ACIProvider, uidRange *uid.UidRange) error {
	renderedACI, err := acirenderer.GetRenderedACIFromList(imgs, ap)
	if err != nil {
		return err
	}
	return renderImage(renderedACI, dir, ap, uidRange)
}

// Given a RenderedACI, it will extract, under the provided directory, the
// needed files from the right source ACI.
// The manifest will be extracted from the upper ACI.
// No file overwriting is done as it should usually be called
// providing an empty directory.
func renderImage(renderedACI acirenderer.RenderedACI, dir string, ap acirenderer.ACIProvider, uidRange *uid.UidRange) error {
	for _, ra := range renderedACI {
		rs, err := ap.ReadStream(ra.Key)
		if err != nil {
			return err
		}

		// Overwrite is not needed. If a file needs to be overwritten then the renderedACI builder has a bug
		if err := ptar.ExtractTar(rs, dir, false, uidRange, ra.FileMap); err != nil {
			rs.Close()
			return fmt.Errorf("error extracting ACI: %v", err)
		}
		rs.Close()
	}

	return nil
}
