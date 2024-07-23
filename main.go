package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"agones.dev/agones/pkg/util/https"
	"agones.dev/agones/pkg/util/runtime"
	"github.com/rancher/dynamiclistener"
	"github.com/rancher/dynamiclistener/server"
	wadmission "github.com/rancher/wrangler/v3/pkg/generated/controllers/admissionregistration.k8s.io"
	wapiregistration "github.com/rancher/wrangler/v3/pkg/generated/controllers/apiregistration.k8s.io"
	"github.com/rancher/wrangler/v3/pkg/generated/controllers/core"
	wcorev1 "github.com/rancher/wrangler/v3/pkg/generated/controllers/core/v1"
	"github.com/rancher/wrangler/v3/pkg/generic"
	"github.com/rancher/wrangler/v3/pkg/kubeconfig"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	apiregistrationv1 "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"
)

const (
	serviceName      = "apiserver-poc"
	namespace        = "default"
	tlsName          = "apiserver-poc.default.svc"
	certName         = "cattle-apiextension-tls"
	caName           = "cattle-apiextension-ca"
	defaultHTTPSPort = 9443
)

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func init() {
	AddToScheme(Scheme)
}

// Examples:
// - Bypassing ETCD for temp/sensitive data
//   - Changing password -> No need for mutating webhook, or access to read it,
//   or ..
// - Filtering a list
//   - Listing tokens that belong to ME

func ptr[T any](t T) *T {
	return &t
}

func CreateOrUpdate[T generic.RuntimeMetaObject, TList k8sruntime.Object](
	out T,
	client generic.NonNamespacedControllerInterface[T, TList],
	setFn func(T),
) error {
	old, err := client.Get(out.GetName(), metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			setFn(out)
			_, err = client.Create(out)
		}
		return err
	} else {
		setFn(old)
		_, err = client.Update(old)
		return err
	}
}

func CreateOrUpdateNamespaced[T generic.RuntimeMetaObject, TList k8sruntime.Object](
	out T,
	client generic.ControllerInterface[T, TList],
	setFn func(T),
) error {
	old, err := client.Get(out.GetNamespace(), out.GetName(), metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			setFn(out)
			_, err = client.Create(out)
		}
		return err
	} else {
		setFn(old)
		_, err = client.Update(old)
		return err
	}
}

func getSecretAndToken(secretClient wcorev1.SecretController, ns string, resourceName string) (*corev1.Secret, *RancherToken, error) {
	secret, err := secretClient.Get(ns, resourceName, metav1.GetOptions{})
	if err != nil {
		return nil, nil, err
	}

	token := &RancherToken{
		ObjectMeta: secret.ObjectMeta,
		Spec: RancherTokenSpec{
			UserID:      string(secret.Data["userID"]),
			ClusterName: string(secret.Data["clusterName"]),
			TTL:         string(secret.Data["ttl"]),
			Enabled:     string(secret.Data["enabled"]),
		},
		Status: RancherTokenStatus{
			HashedToken: string(secret.Data["hashedToken"]),
		},
	}
	return secret, token, nil
}

func secretFromToken(token *RancherToken) *corev1.Secret {
	secret := &corev1.Secret{
		ObjectMeta: token.ObjectMeta,
		StringData: make(map[string]string),
		Data:       make(map[string][]byte),
	}
	secret.StringData["userID"] = token.Spec.UserID
	secret.StringData["clusterName"] = token.Spec.ClusterName
	secret.StringData["ttl"] = token.Spec.TTL
	secret.StringData["hashedToken"] = token.Status.HashedToken
	secret.StringData["enabled"] = token.Spec.Enabled
	return secret
}

func main() {
	ctx := context.Background()

	restConfig, err := kubeconfig.GetNonInteractiveClientConfig(os.Getenv("KUBECONFIG")).ClientConfig()
	must(err)

	coreFactory, err := core.NewFactoryFromConfig(restConfig)
	must(err)

	factory, err := wapiregistration.NewFactoryFromConfig(restConfig)
	must(err)

	admissionFactory, err := wadmission.NewFactoryFromConfig(restConfig)
	must(err)

	mutatingClient := admissionFactory.Admissionregistration().V1().MutatingWebhookConfiguration()
	validatingClient := admissionFactory.Admissionregistration().V1().ValidatingWebhookConfiguration()

	apiServiceClient := factory.Apiregistration().V1().APIService()
	secretClient := coreFactory.Core().V1().Secret()

	// Update the APIService resource when the TLS CA changes
	coreFactory.Core().V1().Secret().OnChange(ctx, "update-api-service", func(name string, secret *corev1.Secret) (*corev1.Secret, error) {
		if secret == nil || secret.Name != caName || secret.Namespace != namespace {
			return secret, nil
		}

		if err := syncMutatingWebhook(mutatingClient, secret); err != nil {
			return nil, err
		}

		if err := syncValidatingWebhook(validatingClient, secret); err != nil {
			return nil, err
		}

		it := &apiregistrationv1.APIService{
			ObjectMeta: metav1.ObjectMeta{
				Name: fmt.Sprintf("%s.%s", SchemeGroupVersion.Version, SchemeGroupVersion.Group),
			},
		}
		err = CreateOrUpdate(it, apiServiceClient, func(apiService *apiregistrationv1.APIService) {
			apiService.Spec = apiregistrationv1.APIServiceSpec{
				Service: &apiregistrationv1.ServiceReference{
					Namespace: namespace,
					Name:      serviceName,
					Port:      ptr(int32(defaultHTTPSPort)),
				},
				CABundle: secret.Data[corev1.TLSCertKey],
				Group:    SchemeGroupVersion.Group,
				Version:  SchemeGroupVersion.Version,
				// TODO: Verify what "good default" values should be
				GroupPriorityMinimum: 100,
				VersionPriority:      10,
			}
		})
		if err != nil {
			return nil, err
		}
		return nil, nil
	})

	mux := http.DefaultServeMux
	apiSrv := NewAPIServer(mux)

	apiSrv.AddAPIResource(SchemeGroupVersion, metav1.APIResource{
		Name:         "clusterranchertokens",
		SingularName: "clusterranchertoken",
		Namespaced:   false,
		Kind:         "ClusterRancherToken",
		Verbs: metav1.Verbs{
			"create",
			"list",
		},
	}, func(w http.ResponseWriter, req *http.Request, s string) error {
		logger := runtime.NewLoggerWithType(s)
		https.LogRequest(logger, req).Info("ClusterRancherTokens")
		return nil
	})

	apiSrv.AddAPIResource(SchemeGroupVersion, metav1.APIResource{
		Name:         "ranchertokens",
		SingularName: "ranchertoken",
		Namespaced:   true,
		Kind:         "RancherToken",
		Verbs: metav1.Verbs{
			"create",
			"get",
		},
	}, func(w http.ResponseWriter, req *http.Request, ns string) error {
		logger := runtime.NewLoggerWithType(ns)
		https.LogRequest(logger, req).Info("RancherTokens")

		switch req.Method {
		case "DELETE":
			fields := strings.Split(req.URL.Path, "/")
			resourceName := fields[len(fields)-1]
			err := secretClient.Delete(ns, resourceName, &metav1.DeleteOptions{})
			if err != nil {
				return err
			}
			status := &metav1.Status{
				Status: "Success",
			}
			info, err := AcceptedSerializer(req, Codecs)
			must(err)
			w.Header().Set("Content-Type", info.MediaType)
			w.WriteHeader(http.StatusOK)
			return Codecs.EncoderForVersion(info.Serializer, SchemeGroupVersion).Encode(status, w)
		case "GET":
			fields := strings.Split(req.URL.Path, "/")
			resourceName := fields[len(fields)-1]

			_, token, err := getSecretAndToken(secretClient, ns, resourceName)
			if err != nil {
				return err
			}

			info, err := AcceptedSerializer(req, Codecs)
			must(err)
			w.Header().Set("Content-Type", info.MediaType)
			w.WriteHeader(http.StatusOK)
			return Codecs.EncoderForVersion(info.Serializer, SchemeGroupVersion).Encode(token, w)
		case "POST":
			bytes, err := io.ReadAll(req.Body)
			if err != nil {
				return err
			}

			token := &RancherToken{}
			_, _, err = Codecs.UniversalDecoder(SchemeGroupVersion).Decode(bytes, nil, token)
			if err != nil {
				return err
			}

			token.Status.PlaintextToken = "the-plaintext-token"
			token.Status.HashedToken = "the-hashed-token"

			secret := secretFromToken(token)
			_, err = secretClient.Create(secret)
			if err != nil {
				return err
			}

			info, err := AcceptedSerializer(req, Codecs)
			if err != nil {
				return err
			}
			w.Header().Set("Content-Type", info.MediaType)
			w.WriteHeader(http.StatusOK)
			err = Codecs.EncoderForVersion(info.Serializer, SchemeGroupVersion).Encode(token, w)
			if err != nil {
				return err
			}
			return nil
		case "PATCH":
			if req.Header.Get("Content-Type") != "application/merge-patch+json" {
				return fmt.Errorf("unsupported patch")
			}

			bytes, err := io.ReadAll(req.Body)
			if err != nil {
				return err
			}
			patchToken := &RancherToken{}
			_, _, err = Codecs.UniversalDecoder(SchemeGroupVersion).Decode(bytes, nil, patchToken)
			if err != nil {
				return err
			}

			fields := strings.Split(req.URL.Path, "/")
			resourceName := fields[len(fields)-1]
			_, token, err := getSecretAndToken(secretClient, ns, resourceName)
			if err != nil {
				return err
			}
			token.Spec.Enabled = patchToken.Spec.Enabled
			secret := secretFromToken(token)
			err = CreateOrUpdateNamespaced(secret, secretClient, func(updated *corev1.Secret) {
				updated.Data = secret.Data
				updated.StringData = secret.StringData
			})
			fmt.Println("HITHERE", err)
			if err != nil {
				return err
			}

			info, err := AcceptedSerializer(req, Codecs)
			if err != nil {
				return err
			}
			w.Header().Set("Content-Type", info.MediaType)
			w.WriteHeader(http.StatusOK)
			err = Codecs.EncoderForVersion(info.Serializer, SchemeGroupVersion).Encode(token, w)
			if err != nil {
				return err
			}
			return nil
		default:
			return fmt.Errorf("unsupported request")
		}

	})

	mux.HandleFunc("/", func(_ http.ResponseWriter, req *http.Request) {
		bytes, err := io.ReadAll(req.Body)
		must(err)
		slog.Info("Received a request", "path", req.URL.Path, "method", req.Method, "body", string(bytes))
	})

	err = setupWebhook(mux)
	must(err)

	fmt.Println("Listening on ", defaultHTTPSPort)
	err = server.ListenAndServe(ctx, defaultHTTPSPort, 0, mux, &server.ListenOpts{
		Secrets:       coreFactory.Core().V1().Secret(),
		CAName:        caName,
		CANamespace:   namespace,
		CertName:      certName,
		CertNamespace: namespace,
		TLSListenerConfig: dynamiclistener.Config{
			SANs: []string{tlsName},
			FilterCN: func(cns ...string) []string {
				return []string{tlsName}
			},
		},
	})
	must(err)

	err = coreFactory.ControllerFactory().Start(ctx, 4)
	must(err)

	<-ctx.Done()
	fmt.Println("Done listen", err)
}
