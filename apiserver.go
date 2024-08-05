// Copyright 2019 Google LLC All Rights Reserved.
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

// Package apiserver manages kubernetes api extension apis
package main

import (
	"fmt"
	"net/http"
	"reflect"
	"strings"

	"agones.dev/agones/pkg/util/https"
	"agones.dev/agones/pkg/util/runtime"
	restful "github.com/emicklei/go-restful/v3"
	"github.com/munnerz/goautoneg"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apiserver/pkg/endpoints/openapi"
	genericapiserver "k8s.io/apiserver/pkg/server"
	kmux "k8s.io/apiserver/pkg/server/mux"
	"k8s.io/apiserver/pkg/server/routes"
)

var (
	// Reference:
	// Scheme scheme for unversioned types - such as APIResourceList, and Status
	Scheme = k8sruntime.NewScheme()
	// Codecs for unversioned types - such as APIResourceList, and Status
	Codecs = serializer.NewCodecFactory(Scheme)

	unversionedVersion = schema.GroupVersion{Version: "v1"}
	unversionedTypes   = []k8sruntime.Object{
		&metav1.Status{},
		&metav1.APIResourceList{},
	}
)

const (
	// ContentTypeHeader = "Content-Type"
	ContentTypeHeader = "Content-Type"
	// AcceptHeader = "Accept"
	AcceptHeader = "Accept"
)

func init() {
	Scheme.AddUnversionedTypes(unversionedVersion, unversionedTypes...)
}

// CRDHandler is a http handler, that gets passed the Namespace it's working
// on, and returns an error if a server error occurs
type CRDHandler func(http.ResponseWriter, *http.Request, string) error

// APIServer is a lightweight library for registering, and providing handlers
// for Kubernetes APIServer extensions.
type APIServer struct {
	logger              *logrus.Entry
	mux                 *http.ServeMux
	resourceList        map[schema.GroupVersion]*metav1.APIResourceList
	delegates           map[schema.GroupVersionResource]CRDHandler
	namespacedDelegates map[schema.GroupVersionResource]CRDHandler
}

func noop(*restful.Request, *restful.Response) {
}

// NewAPIServer returns a new API Server from the given Mux.
// creates a empty Swagger definition and sets up the endpoint.
func NewAPIServer(mux *http.ServeMux) *APIServer {
	oapiConfigV2 := genericapiserver.DefaultOpenAPIConfig(
		getDefinitions,
		openapi.NewDefinitionNamer(Scheme),
	)

	oapiConfigV3 := genericapiserver.DefaultOpenAPIV3Config(
		getDefinitions,
		openapi.NewDefinitionNamer(Scheme),
	)

	// TODO: Get rid of NewDefinitionNamer because it keeps the whole
	// type's PkgPath which isn't what we want.
	// eg: openapi-mux.apis.tomlebreux.com.v1alpha1.RancherToken
	//      instead of
	//     com.tomlebreux.v1alpha1.RancherToken

	oapiRoutes := routes.OpenAPI{
		Config:   oapiConfigV2,
		V3Config: oapiConfigV3,
	}

	theMux := kmux.NewPathRecorderMux("hi")

	container := restful.NewContainer()
	ws := &restful.WebService{}
	ws.Path("/apis/tomlebreux.com/v1alpha1")
	ws.Route(
		ws.GET("/namespaces/{namespace}/ranchertokens").
			To(noop).
			// Ends up being changed to listTomlebreuxComV1alpha1RancherTokens ..
			Operation("listRancherTokens").
			Doc("list all your tokens").
			Consumes(restful.MIME_JSON).
			Produces(restful.MIME_JSON).
			Reads(&RancherToken{}).
			Returns(200, "OK", &RancherToken{}).
			// Whatever, just testing things
			ReturnsError(404, "not found", &RancherToken{}),
	)
	ws.Route(
		ws.DELETE("/namespaces/{namespace}/ranchertokens/{name}").
			To(noop).
			// Ends up being changed to listTomlebreuxComV1alpha1RancherTokens ..
			Operation("deleteRancherTokens").
			Doc("delete the token").
			Consumes(restful.MIME_JSON).
			Produces(restful.MIME_JSON).
			Reads(&RancherToken{}).
			Returns(200, "OK", &RancherToken{}).
			// Whatever, just testing things
			ReturnsError(404, "not found", &RancherToken{}),
	)
	container.Add(ws)

	oapiRoutes.InstallV2(container, theMux)
	oapiRoutes.InstallV3(container, theMux)

	s := &APIServer{
		mux:                 mux,
		resourceList:        map[schema.GroupVersion]*metav1.APIResourceList{},
		delegates:           map[schema.GroupVersionResource]CRDHandler{},
		namespacedDelegates: map[schema.GroupVersionResource]CRDHandler{},
	}
	s.logger = runtime.NewLoggerWithType(s)
	s.logger.Debug("API Server Started")

	mux.Handle("/openapi/v3", theMux)
	mux.Handle("/openapi/v2", theMux)
	mux.Handle("/openapi/v3/", theMux)
	mux.Handle("/openapi/v2/", theMux)

	// This endpoint serves APIGroupDiscoveryList objects which is defined in this
	// KEP: https://github.com/kubernetes/enhancements/blob/master/keps/sig-api-machinery/3352-aggregated-discovery/
	// Since it's only GA in v1.30, we don't need to support it yet.
	//
	// Apparently Aggregate Discovery expects a 406, otherwise namespaces don't successfully terminate on <= 1.27.2
	// (according to the code I forked)
	mux.HandleFunc("/apis", func(w http.ResponseWriter, r *http.Request) {
		https.LogRequest(s.logger, r).Info("TopLevel /apis")
		w.WriteHeader(http.StatusNotAcceptable)
		w.Header().Set(ContentTypeHeader, k8sruntime.ContentTypeJSON)
	})

	return s
}

// AddAPIResource stores the APIResource under the given groupVersion string, and returns it
// in the appropriate place for the K8s discovery service
// e.g. http://localhost:8001/apis/scheduling.k8s.io/v1
// as well as registering a CRDHandler that all http requests for the given APIResource are routed to
func (as *APIServer) AddAPIResource(groupVersion schema.GroupVersion, resource metav1.APIResource, handler CRDHandler) {
	_, ok := as.resourceList[groupVersion]
	if !ok {
		// discovery handler
		list := &metav1.APIResourceList{GroupVersion: groupVersion.String(), APIResources: []metav1.APIResource{}}
		as.resourceList[groupVersion] = list
		pattern := fmt.Sprintf("/apis/%s", groupVersion)
		as.addSerializedHandler(pattern, list)
		as.logger.WithField("groupversion", groupVersion).WithField("pattern", pattern).Info("Adding Discovery Handler")

		pattern = fmt.Sprintf("/apis/%s/namespaces/", groupVersion)
		as.mux.HandleFunc(pattern, https.ErrorHTTPHandler(as.logger, as.namespacedResourceHandler(groupVersion)))
		as.logger.WithField("groupversion", groupVersion.String()).WithField("pattern", pattern).WithField("namespaced", "true").Info("Adding Resource Handler")

		pattern = fmt.Sprintf("/apis/%s/", groupVersion)
		as.mux.HandleFunc(pattern, https.ErrorHTTPHandler(as.logger, as.resourceHandler(groupVersion)))
		as.logger.WithField("groupversion", groupVersion.String()).WithField("pattern", pattern).WithField("namespaced", "false").Info("Adding Resource Handler")
	}

	// discovery resource
	as.resourceList[groupVersion].APIResources = append(as.resourceList[groupVersion].APIResources, resource)

	// add specific crd resource handler
	gvr := schema.GroupVersionResource{
		Group:    groupVersion.Group,
		Version:  groupVersion.Version,
		Resource: resource.Name,
	}
	if resource.Namespaced {
		as.namespacedDelegates[gvr] = handler
	} else {
		as.delegates[gvr] = handler
	}

	as.logger.WithField("groupversion", groupVersion).WithField("apiresource", resource).Info("Adding APIResource")
}

// Namespaced: /apis/tomlebreux.com/v1/namespaces/<namespace>/<resource>
// namespacedResourceHandler handles namespaced resource calls, and sends them to the appropriate CRDHandler delegate
func (as *APIServer) namespacedResourceHandler(groupVersion schema.GroupVersion) https.ErrorHandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		namespace, resource, err := splitNamespaceResource(r.URL.Path)
		if err != nil {
			https.FourZeroFour(as.logger.WithError(err), w, r)
			return nil
		}

		gvr := schema.GroupVersionResource{
			Group:    groupVersion.Group,
			Version:  groupVersion.Version,
			Resource: resource,
		}
		delegate, ok := as.namespacedDelegates[gvr]
		if !ok {
			https.FourZeroFour(as.logger, w, r)
			return nil
		}

		if err := delegate(w, r, namespace); err != nil {
			return err
		}

		return nil
	}
}

// Cluster: /apis/tomlebreux.com/v1/<resource>
func (as *APIServer) resourceHandler(groupVersion schema.GroupVersion) https.ErrorHandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		resource, err := splitResource(r.URL.Path)
		if err != nil {
			https.FourZeroFour(as.logger.WithError(err), w, r)
			return nil
		}

		gvr := schema.GroupVersionResource{
			Group:    groupVersion.Group,
			Version:  groupVersion.Version,
			Resource: resource,
		}
		delegate, ok := as.delegates[gvr]
		if !ok {
			https.FourZeroFour(as.logger, w, r)
			return nil
		}

		if err := delegate(w, r, namespace); err != nil {
			return err
		}

		return nil
	}
}

// addSerializedHandler sets up a handler than will send the serialised content
// to the specified path.
func (as *APIServer) addSerializedHandler(pattern string, m k8sruntime.Object) {
	as.mux.HandleFunc(pattern, https.ErrorHTTPHandler(as.logger, func(w http.ResponseWriter, r *http.Request) error {
		if r.Method == http.MethodGet {
			info, err := AcceptedSerializer(r, Codecs)
			if err != nil {
				return err
			}

			shallowCopy := shallowCopyObjectForTargetKind(m)
			w.Header().Set(ContentTypeHeader, info.MediaType)
			err = Codecs.EncoderForVersion(info.Serializer, unversionedVersion).Encode(shallowCopy, w)
			if err != nil {
				return errors.New("error marshalling")
			}
		} else {
			https.FourZeroFour(as.logger, w, r)
		}

		return nil
	}))
}

// shallowCopyObjectForTargetKind ensures obj is unique by performing a shallow copy
// of the struct Object points to (all Object must be a pointer to a struct in a scheme).
// Copied from https://github.com/kubernetes/kubernetes/pull/101123 until the referenced PR is merged
func shallowCopyObjectForTargetKind(obj k8sruntime.Object) k8sruntime.Object {
	v := reflect.ValueOf(obj).Elem()
	copied := reflect.New(v.Type())
	copied.Elem().Set(v)
	return copied.Interface().(k8sruntime.Object)
}

// AcceptedSerializer takes the request, and returns a serialiser (if it exists)
// for the given codec factory and
// for the Accepted media types.  If not found, returns error
func AcceptedSerializer(r *http.Request, codecs serializer.CodecFactory) (k8sruntime.SerializerInfo, error) {
	// this is so we know what we can accept
	mediaTypes := codecs.SupportedMediaTypes()
	alternatives := make([]string, len(mediaTypes))
	for i, media := range mediaTypes {
		alternatives[i] = media.MediaType
	}
	header := r.Header.Get(AcceptHeader)
	accept := goautoneg.Negotiate(header, alternatives)
	if accept == "" {
		accept = k8sruntime.ContentTypeJSON
	}
	info, ok := k8sruntime.SerializerInfoForMediaType(mediaTypes, accept)
	if !ok {
		return info, errors.Errorf("Could not find serializer for Accept: %s", header)
	}

	return info, nil
}

// splitNameSpaceResource returns the namespace and the type of resource
func splitNamespaceResource(path string) (string, string, error) {
	list := strings.Split(strings.Trim(path, "/"), "/")
	if len(list) < 4 {
		return "", "", errors.Errorf("could not find namespace and resource in path: %s", path)
	}
	last := list[3:]

	if last[0] != "namespaces" {
		return "", "", errors.Errorf("wrong format in path: %s", path)
	}

	return last[1], last[2], nil
}

// Cluster: /apis/tomlebreux.com/v1/<resource>
func splitResource(path string) (string, error) {
	list := strings.Split(strings.Trim(path, "/"), "/")
	if len(list) < 4 {
		return "", errors.Errorf("could not find resource in path: %s", path)
	}
	last := list[len(list)-1:]

	return last[0], nil
}
