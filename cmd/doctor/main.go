/*
Copyright 2017 The Kubernetes Authors.

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

package main

import (
	"flag"
	"io"
	"log"
	"net/http"
	"sort"

	"github.com/gorilla/mux"

	"github.com/google/go-containerregistry/pkg/authn/k8schain"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/knative/pkg/configmap"
)

var (
	image = flag.String("image", "", "The name of the image to serve.")

	bigLayers map[string]string
)

type Handler func(http.ResponseWriter, *http.Request)

func main() {
	flag.Parse()

	cm, err := configmap.Load("/etc/recipe")
	if err != nil {
		log.Fatalf("Error loading image configuration: %v", err)
	}
	bigLayers = cm

	rtr := mux.NewRouter()
	rtr.HandleFunc("/v2/{repository:[a-z0-9-_/]+}/manifests/{tag:[a-z0-9-_]+}", logger(tag))
	rtr.HandleFunc("/v2/{repository:[a-z0-9-_/]+}/blobs/{digest:sha256[:][a-f0-9]+}", logger(blob))
	rtr.HandleFunc("/v2/", logger(ping)).Methods("GET")

	// TODO(mattmoor): At startup we should construct a v1.Image for each of the images we plan to serve.

	http.Handle("/", rtr)
	http.ListenAndServe(":8080", nil)
}

func logger(h Handler) Handler {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s", r.Method, r.URL.String())
		log.Printf("Params: %#v", mux.Vars(r))
		h(w, r)
	}
}

func ping(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("OK"))
}

func getImage(imgName string) (v1.Image, error) {
	ref, err := name.ParseReference(imgName, name.WeakValidation)
	if err != nil {
		return nil, err
	}

	kc, err := k8schain.NewNoClient()
	if err != nil {
		return nil, err
	}

	return remote.Image(ref, remote.WithAuthFromKeychain(kc))
}

func getSyntheticImage(r *http.Request) (v1.Image, error) {
	var layerOrder []string
	layerMap := make(map[string]v1.Layer)
	for k, v := range bigLayers {
		img, err := getImage(v)
		if err != nil {
			return nil, err
		}

		h := v1.Hash{
			Algorithm: "sha256",
			Hex:       k,
		}

		l, err := img.LayerByDigest(h)
		if err != nil {
			return nil, err
		}

		layerOrder = append(layerOrder, h.String())
		layerMap[h.String()] = l
	}
	sort.Strings(layerOrder)

	var layers []v1.Layer
	for _, d := range layerOrder {
		layers = append(layers, layerMap[d])
	}

	executable, err := getImage(*image)
	if err != nil {
		return nil, err
	}

	// The executable's files go on top to take precedence.
	el, err := executable.Layers()
	if err != nil {
		return nil, err
	}
	layers = append(layers, el...)

	// Glue all of the layers together to create the final
	// image filesystem.
	allLayers, err := mutate.AppendLayers(empty.Image, layers...)
	if err != nil {
		return nil, err
	}

	// Use the Config of the executable container as the Config for
	// the resulting image.
	cf, err := executable.ConfigFile()
	if err != nil {
		return nil, err
	}

	return mutate.Config(allLayers, cf.Config)
}

func tag(w http.ResponseWriter, r *http.Request) {
	// TODO(mattmoor): We will compute and serve this JSON, but for now proxy it.

	img, err := getSyntheticImage(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	mt, err := img.MediaType()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	rm, err := img.RawManifest()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("content-type", string(mt))
	// TODO(mattmoor): docker-content-digest
	w.Write(rm)
}

func blob(w http.ResponseWriter, r *http.Request) {
	img, err := getSyntheticImage(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	params := mux.Vars(r)
	h, err := v1.NewHash(params["digest"])
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	cn, err := img.ConfigName()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// If they are looking for the Config, then return it.
	if cn == h {
		rcf, err := img.RawConfigFile()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Write(rcf)
		return
	}

	l, err := img.LayerByDigest(h)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	rc, err := l.Compressed()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rc.Close()

	// Proxy the response back without buffering it locally (it could be large)
	// TODO(mattmoor): There is no good way to check for errors
	// here once we start writing.
	io.Copy(w, rc)
}
