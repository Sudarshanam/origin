/*
Copyright 2016 The Kubernetes Authors.

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

package apiserver

import (
	"net/http"
	"net/url"
	"sync/atomic"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/apimachinery/pkg/util/httpstream/spdy"
	"k8s.io/apiserver/pkg/endpoints/handlers/responsewriters"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	genericrest "k8s.io/apiserver/pkg/registry/generic/rest"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/transport"

	apiregistrationapi "k8s.io/kube-aggregator/pkg/apis/apiregistration"
)

// proxyHandler provides a http.Handler which will proxy traffic to locations
// specified by items implementing Redirector.
type proxyHandler struct {
	contextMapper genericapirequest.RequestContextMapper

	// localDelegate is used to satisfy local APIServices
	localDelegate http.Handler

	// proxyClientCert/Key are the client cert used to identify this proxy. Backing APIServices use
	// this to confirm the proxy's identity
	proxyClientCert []byte
	proxyClientKey  []byte

	handlingInfo atomic.Value
}

type proxyHandlingInfo struct {
	// local indicates that this APIService is locally satisfied
	local bool

	// restConfig holds the information for building a roundtripper
	restConfig *restclient.Config
	// transportBuildingError is an error produced while building the transport.  If this
	// is non-nil, it will be reported to clients.
	transportBuildingError error
	// proxyRoundTripper is the re-useable portion of the transport.  It does not vary with any request.
	proxyRoundTripper http.RoundTripper
	// destinationHost is the hostname of the backing API server
	destinationHost string
}

func (r *proxyHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	value := r.handlingInfo.Load()
	if value == nil {
		r.localDelegate.ServeHTTP(w, req)
		return
	}
	handlingInfo := value.(proxyHandlingInfo)
	if handlingInfo.local {
		if r.localDelegate == nil {
			http.Error(w, "", http.StatusNotFound)
			return
		}
		r.localDelegate.ServeHTTP(w, req)
		return
	}

	if handlingInfo.transportBuildingError != nil {
		http.Error(w, handlingInfo.transportBuildingError.Error(), http.StatusInternalServerError)
		return
	}
	proxyRoundTripper := handlingInfo.proxyRoundTripper
	if proxyRoundTripper == nil {
		http.Error(w, "", http.StatusNotFound)
		return
	}

	ctx, ok := r.contextMapper.Get(req)
	if !ok {
		http.Error(w, "missing context", http.StatusInternalServerError)
		return
	}
	user, ok := genericapirequest.UserFrom(ctx)
	if !ok {
		http.Error(w, "missing user", http.StatusInternalServerError)
		return
	}

	// write a new location based on the existing request pointed at the target service
	location := &url.URL{}
	location.Scheme = "https"
	location.Host = handlingInfo.destinationHost
	location.Path = req.URL.Path
	location.RawQuery = req.URL.Query().Encode()

	// make a new request object with the updated location and the body we already have
	newReq, err := http.NewRequest(req.Method, location.String(), req.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	mergeHeader(newReq.Header, req.Header)
	newReq.ContentLength = req.ContentLength
	// Copy the TransferEncoding is for future-proofing. Currently Go only supports "chunked" and
	// it can determine the TransferEncoding based on ContentLength and the Body.
	newReq.TransferEncoding = req.TransferEncoding

	upgrade := false
	// we need to wrap the roundtripper in another roundtripper which will apply the front proxy headers
	proxyRoundTripper, upgrade, err = maybeWrapForConnectionUpgrades(handlingInfo.restConfig, proxyRoundTripper, req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	proxyRoundTripper = transport.NewAuthProxyRoundTripper(user.GetName(), user.GetGroups(), user.GetExtra(), proxyRoundTripper)

	// if we are upgrading, then the upgrade path tries to use this request with the TLS config we provide, but it does
	// NOT use the roundtripper.  Its a direct call that bypasses the round tripper.  This means that we have to
	// attach the "correct" user headers to the request ahead of time.  After the initial upgrade, we'll be back
	// at the roundtripper flow, so we only have to muck with this request, but we do have to do it.
	if upgrade {
		transport.SetAuthProxyHeaders(newReq, user.GetName(), user.GetGroups(), user.GetExtra())
	}

	handler := genericrest.NewUpgradeAwareProxyHandler(location, proxyRoundTripper, true, upgrade, &responder{w: w})
	handler.ServeHTTP(w, newReq)
}

// maybeWrapForConnectionUpgrades wraps the roundtripper for upgrades.  The bool indicates if it was wrapped
func maybeWrapForConnectionUpgrades(restConfig *restclient.Config, rt http.RoundTripper, req *http.Request) (http.RoundTripper, bool, error) {
	if !httpstream.IsUpgradeRequest(req) {
		return rt, false, nil
	}

	tlsConfig, err := restclient.TLSConfigFor(restConfig)
	if err != nil {
		return nil, true, err
	}
	upgradeRoundTripper := spdy.NewRoundTripper(tlsConfig)
	wrappedRT, err := restclient.HTTPWrappersForConfig(restConfig, upgradeRoundTripper)
	if err != nil {
		return nil, true, err
	}

	return wrappedRT, true, nil
}

func mergeHeader(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// responder implements rest.Responder for assisting a connector in writing objects or errors.
type responder struct {
	w http.ResponseWriter
}

// TODO this should properly handle content type negotiation
// if the caller asked for protobuf and you write JSON bad things happen.
func (r *responder) Object(statusCode int, obj runtime.Object) {
	responsewriters.WriteRawJSON(statusCode, obj, r.w)
}

func (r *responder) Error(err error) {
	http.Error(r.w, err.Error(), http.StatusInternalServerError)
}

// these methods provide locked access to fields

func (r *proxyHandler) updateAPIService(apiService *apiregistrationapi.APIService, destinationHost string) {
	if apiService.Spec.Service == nil {
		r.handlingInfo.Store(proxyHandlingInfo{local: true})
		return
	}

	newInfo := proxyHandlingInfo{
		destinationHost: destinationHost,
		restConfig: &restclient.Config{
			TLSClientConfig: restclient.TLSClientConfig{
				Insecure:   apiService.Spec.InsecureSkipTLSVerify,
				ServerName: apiService.Spec.Service.Name + "." + apiService.Spec.Service.Namespace + ".svc",
				CertData:   r.proxyClientCert,
				KeyData:    r.proxyClientKey,
				CAData:     apiService.Spec.CABundle,
			},
		},
	}
	newInfo.proxyRoundTripper, newInfo.transportBuildingError = restclient.TransportFor(newInfo.restConfig)
	r.handlingInfo.Store(newInfo)
}
