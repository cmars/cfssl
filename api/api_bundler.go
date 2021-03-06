package api

import (
	"encoding/json"
	"net/http"

	"github.com/cloudflare/cfssl/bundler"
	"github.com/cloudflare/cfssl/errors"
	"github.com/cloudflare/cfssl/log"
)

// BundlerHandler accepts requests for either remote or uploaded
// certificates to be bundled, and returns a certificate bundle (or
// error).
type BundlerHandler struct {
	bundler *bundler.Bundler
}

func NewBundleHandler(caBundleFile, intBundleFile string) (http.Handler, error) {
	var err error

	b := new(BundlerHandler)
	if b.bundler, err = bundler.NewBundler(caBundleFile, intBundleFile); err != nil {
		return nil, err
	}

	log.Info("bundler API ready")
	return HttpHandler{b, "POST"}, nil
}

func (h *BundlerHandler) Handle(w http.ResponseWriter, r *http.Request) error {
	blob, matched, err := processRequestOneOf(r,
		[][]string{
			{"domain"},
			{"certificate"},
		})
	if err != nil {
		log.Warningf("invalid request: %v", err)
		return err
	}

	var result *bundler.Bundle
	switch matched[0] {
	case "domain":
		bundle, err := h.bundler.BundleFromRemote(blob["domain"], blob["ip"])
		if err != nil {
			log.Warningf("couldn't bundle from remote: %v", err)
			return errors.NewBadRequest(err)
		}
		result = bundle
	case "certificate":
		flavor := blob["flavor"]
		var bf bundler.BundleFlavor = bundler.Ubiquitous
		if flavor != "" {
			bf = bundler.BundleFlavor(flavor)
		}
		bundle, err := h.bundler.BundleFromPEM([]byte(blob["certificate"]), []byte(blob["private_key"]), bf)
		if err != nil {
			log.Warning("bad PEM certifcate or private key")
			return errors.NewBadRequest(err)
		}
		log.Infof("request for flavour %v", flavor)
		result = bundle
	}
	response := newSuccessResponse(result)
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	err = enc.Encode(response)
	return err
}
