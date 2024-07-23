package main

import (
	"context"
	"fmt"
	"net/http"

	wadmission "github.com/rancher/wrangler/v3/pkg/generated/controllers/admissionregistration.k8s.io/v1"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

var (
	mutatingHook = &webhook.Admission{
		Handler: admission.HandlerFunc(func(ctx context.Context, req webhook.AdmissionRequest) webhook.AdmissionResponse {
			fmt.Println("HITHERE mutating webhook")
			return webhook.Patched("some changes",
				webhook.JSONPatchOp{Operation: "add", Path: "/metadata/annotations/access", Value: "granted"},
				webhook.JSONPatchOp{Operation: "add", Path: "/metadata/annotations/reason", Value: "not blocked"},
			)
		}),
	}

	validatingHook = &webhook.Admission{
		Handler: admission.HandlerFunc(func(ctx context.Context, req webhook.AdmissionRequest) webhook.AdmissionResponse {
			fmt.Println("HITHERE validating webhook")
			if req.Name == "blocked" {
				return webhook.Denied("none shall pass!")
			}
			return webhook.Allowed("")
		}),
	}
)

func setupWebhook(
	mux *http.ServeMux,
) error {
	mutatingHookHandler, err := admission.StandaloneWebhook(mutatingHook, admission.StandaloneOptions{})
	if err != nil {
		return err
	}

	validatingHookHandler, err := admission.StandaloneWebhook(validatingHook, admission.StandaloneOptions{})
	if err != nil {
		return err
	}

	// Register the webhook handlers to your server
	mux.Handle("/mutating", mutatingHookHandler)
	mux.Handle("/validating", validatingHookHandler)
	return nil
}

func syncMutatingWebhook(mutatingClient wadmission.MutatingWebhookConfigurationController, secret *corev1.Secret) error {
	it := &admissionv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("%s.%s", SchemeGroupVersion.Version, SchemeGroupVersion.Group),
		},
	}
	err := CreateOrUpdate(it, mutatingClient, func(mutating *admissionv1.MutatingWebhookConfiguration) {
		mutating.Webhooks = []admissionv1.MutatingWebhook{
			{
				Name: mutating.Name,
				ClientConfig: admissionv1.WebhookClientConfig{
					Service: &admissionv1.ServiceReference{
						Namespace: namespace,
						Name:      serviceName,
						Path:      ptr("/mutating"),
					},
					CABundle: secret.Data[corev1.TLSCertKey],
				},
				Rules: []admissionv1.RuleWithOperations{
					{
						Operations: []admissionv1.OperationType{"*"},
						Rule: admissionv1.Rule{
							APIGroups:   []string{"tomlebreux.com"},
							APIVersions: []string{"*"},
							Resources:   []string{"*"},
						},
					},
				},
				SideEffects:             ptr(admissionv1.SideEffectClassNone),
				AdmissionReviewVersions: []string{"v1"},
			},
		}
	})
	return err
}

func syncValidatingWebhook(validatingClient wadmission.ValidatingWebhookConfigurationController, secret *corev1.Secret) error {
	it := &admissionv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("%s.%s", SchemeGroupVersion.Version, SchemeGroupVersion.Group),
		},
	}
	err := CreateOrUpdate(it, validatingClient, func(validating *admissionv1.ValidatingWebhookConfiguration) {
		validating.Webhooks = []admissionv1.ValidatingWebhook{
			{
				Name: validating.Name,
				ClientConfig: admissionv1.WebhookClientConfig{
					Service: &admissionv1.ServiceReference{
						Namespace: namespace,
						Name:      serviceName,
						Path:      ptr("/validating"),
					},
					CABundle: secret.Data[corev1.TLSCertKey],
				},
				Rules: []admissionv1.RuleWithOperations{
					{
						Operations: []admissionv1.OperationType{"*"},
						Rule: admissionv1.Rule{
							APIGroups:   []string{"tomlebreux.com"},
							APIVersions: []string{"*"},
							Resources:   []string{"*"},
						},
					},
				},
				SideEffects:             ptr(admissionv1.SideEffectClassNone),
				AdmissionReviewVersions: []string{"v1"},
			},
		}
	})
	return err
}
