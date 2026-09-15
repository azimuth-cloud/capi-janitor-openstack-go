/*
Copyright 2026.

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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func TestManagerReadsSecretsWithoutListOrWatch(t *testing.T) {
	var collectionRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/namespaces/default/secrets/clouds":
			_ = json.NewEncoder(w).Encode(&corev1.Secret{
				TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "clouds"},
				Data:       map[string][]byte{"clouds.yaml": []byte("test-cloud")},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/namespaces/default/secrets/missing":
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, "missing").ErrStatus)
		default:
			collectionRequests.Add(1)
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(&metav1.Status{
				TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
				Status:   metav1.StatusFailure,
				Reason:   metav1.StatusReasonForbidden,
				Code:     http.StatusForbidden,
				Message:  "Secret list and watch are forbidden",
			})
		}
	}))
	t.Cleanup(server.Close)

	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{corev1.SchemeGroupVersion})
	mapper.Add(corev1.SchemeGroupVersion.WithKind("Secret"), meta.RESTScopeNamespace)
	mgr, err := ctrl.NewManager(&rest.Config{Host: server.URL}, ctrl.Options{
		Scheme: scheme,
		Client: managerClientOptions(),
		MapperProvider: func(*rest.Config, *http.Client) (meta.RESTMapper, error) {
			return mapper, nil
		},
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	stopped := make(chan error, 1)
	go func() { stopped <- mgr.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-stopped:
			if err != nil {
				t.Errorf("Manager stopped with an error: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Manager did not stop")
		}
	})
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		t.Fatal("Manager cache did not start")
	}

	var secret corev1.Secret
	if err := mgr.GetClient().Get(ctx, types.NamespacedName{Namespace: "default", Name: "clouds"}, &secret); err != nil {
		t.Fatalf("Read Secret with only named GET allowed: %v", err)
	}
	if got := string(secret.Data["clouds.yaml"]); got != "test-cloud" {
		t.Errorf("Secret data = %q, want %q", got, "test-cloud")
	}
	err = mgr.GetClient().Get(ctx, types.NamespacedName{Namespace: "default", Name: "missing"}, &corev1.Secret{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("Missing Secret error = %v, want NotFound", err)
	}
	if got := collectionRequests.Load(); got != 0 {
		t.Errorf("Secret collection requests = %d, want 0", got)
	}
}
