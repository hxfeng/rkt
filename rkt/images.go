// Copyright 2015 CoreOS, Inc.
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

//+build linux

package main

import (
	"container/list"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	docker2aci "github.com/coreos/rkt/Godeps/_workspace/src/github.com/appc/docker2aci/lib"
	"github.com/coreos/rkt/Godeps/_workspace/src/github.com/appc/spec/aci"
	"github.com/coreos/rkt/Godeps/_workspace/src/github.com/appc/spec/discovery"
	"github.com/coreos/rkt/Godeps/_workspace/src/github.com/appc/spec/schema/types"
	"github.com/coreos/rkt/Godeps/_workspace/src/github.com/mitchellh/ioprogress"
	"github.com/coreos/rkt/Godeps/_workspace/src/golang.org/x/crypto/openpgp"
	"github.com/coreos/rkt/common/apps"
	"github.com/coreos/rkt/pkg/keystore"
	"github.com/coreos/rkt/rkt/config"
	"github.com/coreos/rkt/store"
)

type imageActionData struct {
	s                  *store.Store
	ks                 *keystore.Keystore
	headers            map[string]config.Headerer
	dockerAuth         map[string]config.BasicCredentials
	insecureSkipVerify bool
	debug              bool
}

type finder struct {
	imageActionData
	local    bool
	withDeps bool
}

// findImages uses findImage to attain a list of image hashes using discovery if necessary
func (f *finder) findImages(al *apps.Apps) error {
	return al.Walk(func(app *apps.App) error {
		h, err := f.findImage(app.Image, app.Asc, true)
		if err != nil {
			return err
		}
		app.ImageID = *h
		return nil
	})
}

// findImage will recognize a ACI hash and use that, import a local file, use
// discovery or download an ACI directly.
func (f *finder) findImage(img string, asc string, discover bool) (*types.Hash, error) {
	// check if it is a valid hash, if so let it pass through
	h, err := types.NewHash(img)
	if err == nil {
		fullKey, err := f.s.ResolveKey(img)
		if err != nil {
			return nil, fmt.Errorf("could not resolve key: %v", err)
		}
		h, err = types.NewHash(fullKey)
		if err != nil {
			// should never happen
			panic(err)
		}
		return h, nil
	}

	// try fetching the image, potentially remotely
	ft := &fetcher{
		imageActionData: f.imageActionData,
		local:           f.local,
		withDeps:        f.withDeps,
	}
	key, err := ft.fetchImage(img, asc, discover)
	if err != nil {
		return nil, err
	}
	h, err = types.NewHash(key)
	if err != nil {
		// should never happen
		panic(err)
	}

	return h, nil
}

type fetcher struct {
	imageActionData
	local    bool
	withDeps bool
}

// fetchImage will take an image as either a URL or a name string and import it
// into the store if found. If discover is true meta-discovery is enabled. If
// asc is not "", it must exist as a local file and will be used
// as the signature file for verification, unless verification is disabled.
// If f.withDeps is true also image dependencies are fetched.
func (f *fetcher) fetchImage(img string, asc string, discover bool) (string, error) {
	if f.withDeps && !discover {
		return "", fmt.Errorf("cannot fetch image's dependencies with discovery disabled")
	}
	hash, err := f.fetchSingleImage(img, asc, discover)
	if err != nil {
		return "", err
	}
	if f.withDeps {
		err = f.fetchImageDeps(hash)
		if err != nil {
			return "", err
		}
	}
	return hash, nil
}

func (f *fetcher) getImageDeps(hash string) (types.Dependencies, error) {
	key, err := f.s.ResolveKey(hash)
	if err != nil {
		return nil, err
	}
	im, err := f.s.GetImageManifest(key)
	if err != nil {
		return nil, err
	}
	return im.Dependencies, nil
}

func (f *fetcher) addImageDeps(hash string, imgsl *list.List, seen map[string]struct{}) error {
	dependencies, err := f.getImageDeps(hash)
	if err != nil {
		return err
	}
	for _, d := range dependencies {
		app, err := discovery.NewApp(d.App.String(), d.Labels.ToMap())
		if err != nil {
			return err
		}
		imgsl.PushBack(app.String())
		if _, ok := seen[app.String()]; ok {
			return fmt.Errorf("dependency %s specified multiple times in the dependency tree for imageID: %s", app.String(), hash)
		}
		seen[app.String()] = struct{}{}
	}
	return nil
}

// fetchImageDeps will recursively fetch all the image dependencies
func (f *fetcher) fetchImageDeps(hash string) error {
	imgsl := list.New()
	seen := map[string]struct{}{}
	f.addImageDeps(hash, imgsl, seen)
	for el := imgsl.Front(); el != nil; el = el.Next() {
		img := el.Value.(string)
		hash, err := f.fetchSingleImage(img, "", true)
		if err != nil {
			return err
		}
		f.addImageDeps(hash, imgsl, seen)
	}
	return nil
}

// fetchSingleImage will take an image as either a URL or a name string and
// import it into the store if found.  If discover is true meta-discovery is
// enabled.  If asc is not "", it must exist as a local file and will be used
// as the signature file for verification, unless verification is disabled.
func (f *fetcher) fetchSingleImage(img string, asc string, discover bool) (string, error) {
	var (
		ascFile *os.File
		err     error
	)
	if asc != "" && f.ks != nil {
		ascFile, err = os.Open(asc)
		if err != nil {
			return "", fmt.Errorf("unable to open signature file: %v", err)
		}
		defer ascFile.Close()
	}

	u, err := url.Parse(img)
	if err != nil {
		return "", fmt.Errorf("not a valid image reference (%s)", img)
	}

	// if img refers to a local file, ensure the scheme is file:// and make the url path absolute
	_, err = os.Stat(u.Path)
	if err == nil {
		u.Path, err = filepath.Abs(u.Path)
		if err != nil {
			return "", fmt.Errorf("unable to get abs path: %v", err)
		}
		u.Scheme = "file"
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("unable to access %q: %v", img, err)
	}

	if discover && u.Scheme == "" {
		if f.local {
			app, err := discovery.NewAppFromString(img)
			if err != nil {
				return "", err
			}
			labels, err := types.LabelsFromMap(app.Labels)
			if err != nil {
				return "", err
			}
			return f.s.GetACI(app.Name, labels)
		}
		if app := newDiscoveryApp(img); app != nil {
			stdout("rkt: searching for app image %s", img)
			ep, attempts, err := discovery.DiscoverEndpoints(*app, true)

			if globalFlags.Debug {
				for _, a := range attempts {
					stderr("meta tag 'ac-discovery' not found on %s: %v", a.Prefix, a.Error)
				}
			}

			if err != nil {
				return "", err
			}

			if len(ep.ACIEndpoints) == 0 {
				return "", fmt.Errorf("no endpoints discovered")
			}

			latest := false
			// No specified version label, mark it as latest
			if _, ok := app.Labels["version"]; !ok {
				latest = true
			}
			return f.fetchImageFromEndpoints(ep, ascFile, latest)
		}
	}

	switch u.Scheme {
	case "http", "https", "docker", "file":
	default:
		return "", fmt.Errorf("rkt only supports http, https, docker or file URLs (%s)", img)
	}
	return f.fetchImageFromURL(u.String(), u.Scheme, ascFile, false)
}

func (f *fetcher) fetchImageFromEndpoints(ep *discovery.Endpoints, ascFile *os.File, latest bool) (string, error) {
	return f.fetchImageFrom(ep.ACIEndpoints[0].ACI, ep.ACIEndpoints[0].ASC, "", ascFile, latest)
}

func (f *fetcher) fetchImageFromURL(imgurl string, scheme string, ascFile *os.File, latest bool) (string, error) {
	return f.fetchImageFrom(imgurl, ascURLFromImgURL(imgurl), scheme, ascFile, latest)
}

func (f *fetcher) fetchImageFrom(aciURL, ascURL, scheme string, ascFile *os.File, latest bool) (string, error) {
	if scheme != "file" || f.debug {
		stdout("rkt: fetching image from %s", aciURL)
	}

	if f.insecureSkipVerify {
		if f.ks != nil {
			stdout("rkt: warning: signature verification has been disabled")
		}
	} else if scheme == "docker" {
		return "", fmt.Errorf("signature verification for docker images is not supported (try --insecure-skip-verify)")
	}
	var key string
	rem, ok, err := f.s.GetRemote(aciURL)
	if err == nil {
		key = rem.BlobKey
	} else {
		return "", err
	}
	if !ok {
		if f.local && scheme != "file" {
			return "", fmt.Errorf("url %s not available in local store", aciURL)
		}
		entity, aciFile, err := f.fetch(aciURL, ascURL, ascFile)
		if err != nil {
			return "", err
		}
		if scheme != "file" {
			defer os.Remove(aciFile.Name())
		}

		if entity != nil && !f.insecureSkipVerify {
			fmt.Println("rkt: signature verified: ")
			for _, v := range entity.Identities {
				stdout("  %s", v.Name)
			}
		}
		key, err = f.s.WriteACI(aciFile, latest)
		if err != nil {
			return "", err
		}

		if scheme != "file" {
			rem = store.NewRemote(aciURL, ascURL)
			rem.BlobKey = key
			err = f.s.WriteRemote(rem)
			if err != nil {
				return "", err
			}
		}
	}
	return key, nil
}

// fetch opens/downloads and verifies the remote ACI.
// If ascFile is not nil, it will be used as the signature file and ascURL will be ignored.
// If Keystore is nil signature verification will be skipped, regardless of ascFile.
// fetch returns the signer, an *os.File representing the ACI, and an error if any.
// err will be nil if the ACI fetches successfully and the ACI is verified.
func (f *fetcher) fetch(aciURL, ascURL string, ascFile *os.File) (*openpgp.Entity, *os.File, error) {
	var entity *openpgp.Entity
	u, err := url.Parse(aciURL)
	if err != nil {
		return nil, nil, fmt.Errorf("error parsing ACI url: %v", err)
	}
	if u.Scheme == "docker" {
		registryURL := strings.TrimPrefix(aciURL, "docker://")

		tmpDir, err := f.s.TmpDir()
		if err != nil {
			return nil, nil, fmt.Errorf("error creating temporary dir for docker to ACI conversion: %v", err)
		}

		indexName := docker2aci.GetIndexName(registryURL)
		user := ""
		password := ""
		if creds, ok := f.dockerAuth[indexName]; ok {
			user = creds.User
			password = creds.Password
		}
		acis, err := docker2aci.Convert(registryURL, true, tmpDir, user, password)
		if err != nil {
			return nil, nil, fmt.Errorf("error converting docker image to ACI: %v", err)
		}

		aciFile, err := os.Open(acis[0])
		if err != nil {
			return nil, nil, fmt.Errorf("error opening squashed ACI file: %v", err)
		}

		return nil, aciFile, nil
	}

	if f.ks != nil && ascFile == nil {
		u, err := url.Parse(ascURL)
		if err != nil {
			return nil, nil, fmt.Errorf("error parsing ASC url: %v", err)
		}
		if u.Scheme == "file" {
			ascFile, err = os.Open(u.Path)
			if err != nil {
				return nil, nil, fmt.Errorf("error opening signature file: %v", err)
			}
		} else {
			stdout("Downloading signature from %v\n", ascURL)
			ascFile, err = f.s.TmpFile()
			if err != nil {
				return nil, nil, fmt.Errorf("error setting up temporary file: %v", err)
			}
			if err = f.downloadSignatureFile(ascURL, ascFile); err != nil {
				return nil, nil, fmt.Errorf("error downloading the signature file: %v", err)
			}
			defer os.Remove(ascFile.Name())
		}
		defer ascFile.Close()
	}

	var aciFile *os.File
	if u.Scheme == "file" {
		aciFile, err = os.Open(u.Path)
		if err != nil {
			return nil, nil, fmt.Errorf("error opening ACI file: %v", err)
		}
	} else {
		aciFile, err = f.s.TmpFile()
		if err != nil {
			return nil, aciFile, fmt.Errorf("error setting up temporary file: %v", err)
		}
		defer os.Remove(aciFile.Name())

		if err = f.downloadACI(aciURL, aciFile); err != nil {
			return nil, nil, fmt.Errorf("error downloading ACI: %v", err)
		}
	}

	if f.ks != nil {
		manifest, err := aci.ManifestFromImage(aciFile)
		if err != nil {
			return nil, aciFile, err
		}

		if _, err := aciFile.Seek(0, 0); err != nil {
			return nil, aciFile, fmt.Errorf("error seeking ACI file: %v", err)
		}
		if _, err := ascFile.Seek(0, 0); err != nil {
			return nil, aciFile, fmt.Errorf("error seeking signature file: %v", err)
		}
		if entity, err = f.ks.CheckSignature(manifest.Name.String(), aciFile, ascFile); err != nil {
			return nil, aciFile, err
		}
	}

	if _, err := aciFile.Seek(0, 0); err != nil {
		return nil, aciFile, fmt.Errorf("error seeking ACI file: %v", err)
	}
	return entity, aciFile, nil
}

type writeSyncer interface {
	io.Writer
	Sync() error
}

// downloadACI gets the aci specified at aciurl
func (f *fetcher) downloadACI(aciurl string, out writeSyncer) error {
	return f.downloadHTTP(aciurl, "ACI", out)
}

// downloadSignatureFile gets the signature specified at sigurl
func (f *fetcher) downloadSignatureFile(sigurl string, out writeSyncer) error {
	return f.downloadHTTP(sigurl, "signature", out)
}

// downloadHTTP retrieves url, creating a temp file using getTempFile
// file:// http:// and https:// urls supported
func (f *fetcher) downloadHTTP(url, label string, out writeSyncer) error {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	options := make(http.Header)
	// Send credentials only over secure channel
	if req.URL.Scheme == "https" {
		if hostOpts, ok := f.headers[req.URL.Host]; ok {
			options = hostOpts.Header()
		}
	}
	for k, v := range options {
		for _, e := range v {
			req.Header.Add(k, e)
		}
	}
	transport := http.DefaultTransport
	if f.insecureSkipVerify {
		transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}
	client := &http.Client{Transport: transport}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	prefix := "Downloading " + label
	fmtBytesSize := 18
	barSize := int64(80 - len(prefix) - fmtBytesSize)
	bar := ioprogress.DrawTextFormatBar(barSize)
	fmtfunc := func(progress, total int64) string {
		return fmt.Sprintf(
			"%s: %s %s",
			prefix,
			bar(progress, total),
			ioprogress.DrawTextFormatBytes(progress, total),
		)
	}

	reader := &ioprogress.Reader{
		Reader:       res.Body,
		Size:         res.ContentLength,
		DrawFunc:     ioprogress.DrawTerminalf(os.Stdout, fmtfunc),
		DrawInterval: time.Second,
	}

	// TODO(jonboulle): handle http more robustly (redirects?)
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("bad HTTP status code: %d", res.StatusCode)
	}

	if _, err := io.Copy(out, reader); err != nil {
		return fmt.Errorf("error copying %s: %v", label, err)
	}

	if err := out.Sync(); err != nil {
		return fmt.Errorf("error writing %s: %v", label, err)
	}

	return nil
}

func ascURLFromImgURL(imgurl string) string {
	s := strings.TrimSuffix(imgurl, ".aci")
	return s + ".aci.asc"
}

// newDiscoveryApp creates a discovery app if the given img is an app name and
// has a URL-like structure, for example example.com/reduce-worker.
// Or it returns nil.
func newDiscoveryApp(img string) *discovery.App {
	app, err := discovery.NewAppFromString(img)
	if err != nil {
		return nil
	}
	u, err := url.Parse(app.Name.String())
	if err != nil || u.Scheme != "" {
		return nil
	}
	if _, ok := app.Labels["arch"]; !ok {
		app.Labels["arch"] = defaultArch
	}
	if _, ok := app.Labels["os"]; !ok {
		app.Labels["os"] = defaultOS
	}
	return app
}
